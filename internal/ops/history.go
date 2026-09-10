// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"encoding/json"
	"fmt"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
)

// HistoryWindowMs is the documented /v2/allOrders + /v2/myTrades retention
// (36h per the public API docs), the default lookback for an unbounded history
// walk. It lives here (the operations layer) because the walk policy is an ops
// concern; the stream layer re-exports it for its own backfill window.
const HistoryWindowMs = 36 * 60 * 60 * 1000

// historyPageLimit is the server's maximum `limit` for /v2/allOrders and
// /v2/myTrades. A page with this many rows may have been truncated; the walk
// pages until a short page proves the window is covered.
const historyPageLimit = 1000

// maxHistoryPages bounds the backward pagination — far beyond any realistic
// window; hitting the cap is reported as incomplete rather than silently
// stopping.
const maxHistoryPages = 20

// HistoryMaxRows is the largest --limit (total-row cap) the history/fills
// operations accept: the window walk's natural ceiling of maxHistoryPages full
// server pages. A larger --limit could never be satisfied anyway, so it is the
// validation Max; the walk still requests legal server-sized pages and the
// operation trims the deduped result to the requested cap.
const HistoryMaxRows = historyPageLimit * maxHistoryPages

// WalkHistory pages a cursorless history endpoint (/v2/allOrders, /v2/myTrades)
// until the requested window (startTime >= startMs; startTime inclusive,
// endTime exclusive) is fully covered. The server returns rows newest-first, so
// a saturated page truncated the OLDEST rows and the next page sets endTime to
// the page's oldest timestamp + 1 (the +1 keeps the boundary rows in range —
// endTime is exclusive — so pages overlap rather than risk a skip; overlap
// duplicates are removed by the caller's dedupe or are idempotent re-states).
// The sort order is observed, not contractual, so each page's direction is
// re-detected from its row timestamps: an oldest-first page paginates forward
// by startTime instead. Every page is handed to emitPage as it arrives. Returns
// complete=false when the window could not be fully covered (page cap hit, rows
// missing the timestamp field, no pagination progress) so the caller can TELL
// the consumer instead of silently under-delivering.
//
// get runs one page request: it receives the paging params (limit, startTime,
// optional endTime) appended to base and must return the endpoint's data
// document (a JSON array). endMs, when > 0, bounds the window's upper edge
// (endTime exclusive); 0 means "now". This is the engine the stream layer's
// reconnect backfill and the bot API's order.history/fills bindings both run.
func WalkHistory(get func(params []apiclient.KV) (json.RawMessage, error), base []apiclient.KV, startMs, endMs int64, tsField string, emitPage func(rows []json.RawMessage)) (complete bool, err error) {
	curStart, curEnd := startMs, endMs
	for page := 0; page < maxHistoryPages; page++ {
		params := append(append([]apiclient.KV{}, base...),
			apiclient.KV{Key: "limit", Value: fmt.Sprintf("%d", historyPageLimit)},
			apiclient.KV{Key: "startTime", Value: fmt.Sprintf("%d", curStart)},
		)
		if curEnd > 0 {
			params = append(params, apiclient.KV{Key: "endTime", Value: fmt.Sprintf("%d", curEnd)})
		}
		data, err := get(params)
		if err != nil {
			return false, err
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(data, &rows); err != nil {
			return false, fmt.Errorf("history response was not an array: %v", err)
		}
		emitPage(rows)
		if len(rows) < historyPageLimit {
			return true, nil // short page: the (sub-)window is fully covered
		}

		minTs, maxTs := int64(0), int64(0)
		for _, row := range rows {
			ts := rowTimestamp(row, tsField)
			if ts <= 0 {
				return false, nil // cannot paginate without timestamps
			}
			if minTs == 0 || ts < minTs {
				minTs = ts
			}
			if ts > maxTs {
				maxTs = ts
			}
		}
		firstTs := rowTimestamp(rows[0], tsField)
		lastTs := rowTimestamp(rows[len(rows)-1], tsField)

		switch {
		case lastTs > firstTs: // oldest-first page: the NEWEST rows were truncated
			next := maxTs // startTime is inclusive: re-covers the boundary rows
			if next <= curStart {
				return false, nil // no progress
			}
			curStart = next
		default: // newest-first (the observed server order): the OLDEST rows were truncated
			if minTs <= startMs {
				return true, nil // the saturated page already reached the window start
			}
			next := minTs + 1
			if curEnd > 0 && next >= curEnd {
				return false, nil // no progress (>limit rows share one timestamp)
			}
			curEnd = next
		}
	}
	return false, nil
}

// rowTimestamp extracts a millisecond timestamp field from one row, 0 when
// absent/invalid.
func rowTimestamp(row json.RawMessage, field string) int64 {
	var m map[string]json.RawMessage
	if json.Unmarshal(row, &m) != nil {
		return 0
	}
	raw, found := m[field]
	if !found {
		return 0
	}
	var ts int64
	if json.Unmarshal(raw, &ts) != nil {
		return 0
	}
	return ts
}

// jsNames maps a command-id segment to its JS name where they differ. It is the
// same mapping the bot API uses; ops keeps a copy so the truncation notes read
// identically regardless of the frontend.
var jsNames = map[string]string{"ticksize": "tickSize"}

func jsName(segment string) string {
	if n, ok := jsNames[segment]; ok {
		return n
	}
	return segment
}

// opJSKeyName renders an operation's dotted method name without the api.
// prefix — the history/funding truncation notes name the JS method this way.
func opJSKeyName(m OpMeta) string {
	name := jsName(m.ID[0])
	if len(m.ID) == 2 {
		name += "." + jsName(m.ID[1])
	}
	return name
}
