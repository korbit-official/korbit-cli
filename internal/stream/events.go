// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Origin says which path an event's payload took, which also determines its
// schema: WebSocket frames for OriginRealtime/OriginSnapshot, REST response
// shapes for OriginBackfill.
type Origin string

const (
	// OriginRealtime is a live WebSocket frame. Payload is the verbatim frame.
	OriginRealtime Origin = "realtime"
	// OriginSnapshot is the WebSocket snapshot frame the public endpoint sends
	// first after a subscribe (`snapshot:true`). Payload is the verbatim frame.
	OriginSnapshot Origin = "snapshot"
	// OriginBackfill is data fetched over REST to backfill private channels or to
	// patch a gap. Payload is the REST response data (REST field shapes, which
	// differ slightly from the WebSocket frames — e.g. a REST fill has
	// feeQty where a WebSocket myTrade has fee).
	OriginBackfill Origin = "backfill"
	// OriginDerived is data the CLI synthesized itself rather than relayed — a
	// channel that does not exist on the Korbit WebSocket API (the monitor's
	// candle channel, built by internal/candles from the trade stream plus REST
	// seeds). Payload is a CLI-owned document, a stable contract of the emitting
	// layer. A Session never emits this origin.
	OriginDerived Origin = "derived"
)

// Event is one element of a Session's output stream: either a Data or a
// Notice. The stream is the only output a Session has; data and notices are
// interleaved in emission order.
type Event interface{ isEvent() }

// Data is one delivery of channel data.
//
// Ordering: within OriginRealtime on one connection, events preserve server
// order. Across origins there is no global order — a backfill patching an old
// gap is emitted after newer realtime frames. Consumers that materialize
// state must order per key (tradeId for trades; per-order/per-currency
// timestamps otherwise), not by arrival.
type Data struct {
	// Channel is the logical channel: ticker | orderbook | trade | myOrder |
	// myTrade | myAsset.
	Channel string
	// Symbol is the trading pair, when the channel is per-symbol (empty for
	// myAsset).
	Symbol string
	// Origin tells which path the payload took and therefore its schema.
	Origin Origin
	// ServerTime is the server's timestamp for the data in unix ms: the frame
	// timestamp for WebSocket origins; for backfill, the session's
	// server-clock estimate when the REST fetch began — conservative, so
	// "newest ServerTime wins per key" never lets an older delta overwrite a
	// fresher REST snapshot (rows may additionally carry their own per-row
	// timestamps).
	ServerTime int64
	// Source is the REST path that produced an OriginBackfill payload (e.g.
	// "/v2/openOrders" — which also identifies the payload's row shape, and
	// distinguishes an authoritative open-orders snapshot from gap-history
	// rows). Empty for WebSocket origins.
	Source string
	// AccountSeq is the sub-account this private-channel event belongs to (1 =
	// main). It is the request's accountSeq for an OriginBackfill private payload
	// (whose bare-array rows do not echo it) and the wrapper's accountSeq for a
	// live private frame (order/trade/asset). nil when not applicable (public
	// channels) or not tagged — the server omits the wrapper accountSeq unless
	// the subscription explicitly requested accountSeqs, and an unpinned backfill
	// call carries none. A consumer that materializes per-account state reads nil
	// as the default account (1).
	AccountSeq *int
	// PrivateEpoch is the private-connection generation an OriginBackfill private
	// payload was fetched in (the connManager's connection count at backfill
	// start). It lets a state consumer tell a snapshot from a superseded
	// connection — one whose slow REST landed after a newer reconnect already
	// advanced the generation — from a current one, without a transport
	// timestamp. 0 means "not stamped": live frames leave it 0 because they are
	// always delivered before the next connection's CONNECTED (so a consumer reads
	// 0 as the current generation), and public events never set it. Internal to
	// the stream→state path; NOT surfaced in the monitor NDJSON or the bot event.
	PrivateEpoch int64
	// Payload is the verbatim source document, so decimal strings pass
	// through untouched. See Origin for the schema. The one exception to
	// verbatim: a trade/myTrade frame whose rows were partially delivered
	// before is re-emitted with the already-delivered rows removed (rows stay
	// verbatim; the enclosing frame is rebuilt). A resubscribe trade SNAPSHOT
	// whose rows were ALL already delivered is rebuilt empty (data:[]) and still
	// emitted — the snapshot marks the (re)subscribe boundary, so it is delivered
	// rather than suppressed; a live (non-snapshot) all-duplicate frame is dropped.
	Payload json.RawMessage
}

func (Data) isEvent() {}

// Level grades a Notice.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// NoticeCode enumerates everything the stream can tell its consumer besides
// data. The set and the meaning of each code are a stable contract for agents
// watching the stream — extend additively, never rename.
type NoticeCode string

const (
	// Connected: a WebSocket connection was established (first time or after a
	// drop) and subscriptions were sent. Details: endpoint, attempt,
	// downtimeMs (0 on first connect).
	Connected NoticeCode = "CONNECTED"
	// Disconnected: a connection was lost; reconnection starts automatically.
	// Details: endpoint, reason.
	Disconnected NoticeCode = "DISCONNECTED"
	// ConnectFailed: a connection attempt failed; the session keeps retrying
	// with backoff. Details: endpoint, error, nextRetryMs.
	ConnectFailed NoticeCode = "CONNECT_FAILED"
	// SubscribeFailed: the server rejected one subscription item (e.g.
	// INVALID_SYMBOL). The subscription is dropped — that channel/symbol set
	// will produce no further data. Details: endpoint, channel, code, message.
	SubscribeFailed NoticeCode = "SUBSCRIBE_FAILED"
	// BackfillStart: recovery began. For private recovery, details: endpoint,
	// reason ("initial" | "reconnect" | "retry" — a self-heal re-run of the
	// leaf calls a prior pass left transiently failed, with `attempt` and
	// `units`, the exact calls re-run: channel, source, and where applicable
	// symbol and accountSeq), channels. For a public trade gap, details:
	// endpoint="public", reason="gap", channel="trade", symbol, afterTradeId,
	// beforeTradeId.
	BackfillStart NoticeCode = "BACKFILL_START"
	// BackfillDone: recovery finished. For private recovery, details:
	// endpoint, reason, channels, failures (and `attempt` on a retry pass). For
	// a public trade gap, details: endpoint="public", reason="gap",
	// channel="trade", symbol, afterTradeId, beforeTradeId, recoveredRows,
	// failures, complete. The level always matches the paired BackfillStart.
	//
	// PAIRING IS A CONTRACT: every BACKFILL_START is closed by exactly one
	// BACKFILL_DONE, even when the pass failed (failures/complete in details)
	// or its result was discarded — BACKFILL_FAILED is an additional alarm,
	// never a substitute for the DONE. Consumers may balance START against
	// DONE (the state store's Health.Backfilling does); every emitter,
	// including derived-channel synthesizers layered on the session
	// (internal/candles), must uphold it.
	BackfillDone NoticeCode = "BACKFILL_DONE"
	// BackfillFailed: a REST recovery call failed after retries, or configured
	// recovery could not run. Live frames keep flowing but the consumer's view may
	// be missing whatever the gap contained. For private recovery a transiently
	// failed call (network/5xx/429) is re-run automatically with backoff while
	// the connection stays up — each re-run is a fresh BACKFILL_START (reason
	// "retry") scoped to exactly the still-failing calls — but recovery is not
	// guaranteed; a definitive 4xx rejection is re-attempted only on the next
	// (re)connect. Details: endpoint, channel, error; public trade gap notices
	// also include reason="gap", symbol, source, and id bounds.
	BackfillFailed NoticeCode = "BACKFILL_FAILED"
	// BackfillNotViable: the gap cannot be recovered from REST (e.g. the
	// disconnection exceeded the server's 36-hour order/fill history window).
	// Details: endpoint, channel, reason.
	BackfillNotViable NoticeCode = "BACKFILL_NOT_VIABLE"
	// BackfillDisabled: the consumer disabled backfill and a (re)connection or
	// public trade gap happened, so gaps will NOT be patched. Details: endpoint,
	// reason; public trade gap notices also include channel="trade", symbol, and
	// id bounds.
	BackfillDisabled NoticeCode = "BACKFILL_DISABLED"
	// BackfillSnapshotOnly: the consumer chose snapshot-only private backfill
	// (Config.BackfillSnapshotOnly), so on every connect only balances and the
	// open-order snapshot are backfilled and the per-symbol order/fill history WALKS
	// are skipped — order/fill transitions during a disconnect are NOT recovered.
	// Details: endpoint, reason, channels.
	BackfillSnapshotOnly NoticeCode = "BACKFILL_SNAPSHOT_ONLY"
	// DataGap: data was provably or probably lost and could not be fully
	// patched (e.g. a public-trade gap wider than the REST window). Details:
	// channel, symbol, plus gap bounds when known.
	DataGap NoticeCode = "DATA_GAP"
	// DataDelayed: frames are arriving with server-to-client delay above the
	// configured threshold — the consumer is seeing the past. Rate-limited.
	// Details: endpoint, delayMs.
	DataDelayed NoticeCode = "DATA_DELAYED"
	// DataCurrent: the recovery edge for DataDelayed — a standing delay warning
	// has cleared, either because a later frame is current again (Details:
	// endpoint, delayMs) or because the feed went quiet, leaving no late frames
	// to report (Details: endpoint). Emitted once, when a prior DataDelayed
	// clears.
	DataCurrent NoticeCode = "DATA_CURRENT"
	// ConnectionUnreliable: the connection is up but degraded — repeated
	// recent drops or sustained high RTT. Rate-limited. Details: endpoint,
	// disconnects, windowMs and/or rttMs.
	ConnectionUnreliable NoticeCode = "CONNECTION_UNRELIABLE"
	// ConnectionStable: the recovery edge for ConnectionUnreliable — RTT has
	// fallen back under the threshold and recent drops have aged out of the
	// window, so the connection is reliable again. Emitted once, when a prior
	// ConnectionUnreliable clears. Details: endpoint.
	ConnectionStable NoticeCode = "CONNECTION_STABLE"
	// ServerError: the server sent an error control message
	// ({"status":"error", ...}) not tied to a subscription. Details: endpoint,
	// message, code.
	ServerError NoticeCode = "SERVER_ERROR"
	// Keepalive: no data event for the configured interval; the session is
	// alive (details say whether its connections are up), there has just been
	// no data to deliver. Repeats every interval while silence lasts.
	// Details: silentMs, connectionsUp, connectionsTotal.
	Keepalive NoticeCode = "KEEPALIVE"
	// Fatal: the session cannot continue and Run is about to return an error —
	// an auth/permission/config-class failure that reconnecting cannot fix, or
	// every subscription was rejected. Details: endpoint, error.
	Fatal NoticeCode = "FATAL"
)

// Notice is an out-of-band condition report, in-band in the event stream.
type Notice struct {
	Code  NoticeCode
	Level Level
	// Message is one human-readable sentence.
	Message string
	// Details are structured, code-specific fields (documented per code).
	// Values are JSON-marshalable.
	Details map[string]any
	// Time is the local unix-ms time the notice was raised.
	Time int64
}

func (Notice) isEvent() {}

// SlogLevel maps the notice's stream level onto a slog threshold. A notice has
// no debug level (info is its floor), so the mapping is info/warn/error.
func (n Notice) SlogLevel() slog.Level {
	switch n.Level {
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LogNotice renders one notice to log at its own level, as
// "<message> code=<CODE>". It is a rendering helper only — the DECISION to
// mirror notices into a log (and which logger/threshold) belongs to the
// consumer (the monitor/tui frontends), not to this layer; notices remain
// program output on the event stream. log must be non-nil.
func LogNotice(log *slog.Logger, n Notice) {
	log.LogAttrs(context.Background(), n.SlogLevel(), n.Message, slog.String(LogCodeKey, string(n.Code)))
}

// Channel names. A Subscription's Channel must be one of these.
const (
	ChannelTicker    = "ticker"
	ChannelOrderbook = "orderbook"
	ChannelTrade     = "trade"
	ChannelMyOrder   = "myOrder"
	ChannelMyTrade   = "myTrade"
	ChannelMyAsset   = "myAsset"
)

// IsPrivateChannel reports whether a channel rides the private endpoint and
// requires credentials.
func IsPrivateChannel(channel string) bool {
	switch channel {
	case ChannelMyOrder, ChannelMyTrade, ChannelMyAsset:
		return true
	}
	return false
}
