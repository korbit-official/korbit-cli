// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// timePath is the public endpoint that returns Korbit's server clock as a
// millisecond epoch in the standard {success,data:{time}} envelope. The CLI
// syncs to *this* clock — the one the signed-request time-window check uses —
// not to UTC, so a corrected timestamp lands in the server's validity window
// even if the server itself is slightly off true time.
const timePath = "/v2/time"

// defaultClockProbes is how many /v2/time round-trips MeasureClockOffset takes
// before picking the cleanest sample. A handful is enough for the min-delay
// filter to discard the queuing/jitter outliers without adding much latency.
const defaultClockProbes = 5

// maxClockProbeTimeoutMs caps each /v2/time round-trip's HTTP timeout for clock
// measurement, never exceeding the caller's own --timeout. Two reasons: a probe
// slower than this yields an offset whose ±RTT/2 uncertainty is already too
// large to sign usefully against, and — since the probes run sequentially — an
// uncapped per-probe timeout would let a black-holed /v2/time stall a proactive
// sync for probes×--timeout (e.g. 5×15s). The effective per-probe timeout is
// min(caller TimeoutMs, this); an unset caller timeout is capped here too.
const maxClockProbeTimeoutMs = 3000

// ClockOffset is the result of measuring the local clock against Korbit's
// server clock. OffsetMs is the best estimate of (serverClock - localClock):
// add it to a local timestamp to get the server's notion of "now". RTTMinMs is
// the smallest round-trip seen across the probes; the estimate's uncertainty is
// about ±RTTMinMs/2 (the unmodelable path asymmetry), so callers that must not
// overshoot the server's tight future bound lean their corrected timestamp into
// the past by that much. Samples is how many probes succeeded.
type ClockOffset struct {
	OffsetMs int64
	RTTMinMs int64
	Samples  int
}

// UncertaintyMs is the half-RTT bound on the offset estimate's error. It is the
// amount a caller should lean a corrected timestamp into the past so that even
// worst-case path asymmetry cannot push the signed timestamp past the server's
// fixed +1s future bound.
func (o ClockOffset) UncertaintyMs() int64 { return o.RTTMinMs / 2 }

// MeasureClockOffset estimates the offset between the local clock (opts.Now) and
// the Korbit server clock by probing /v2/time `probes` times and keeping the
// sample with the smallest round-trip — NTP's minimum-delay filter, which
// rejects the queuing noise that inflates and skews the estimate. For each
// probe it brackets the request with two local readings and compares the server
// time to their midpoint, so symmetric latency cancels out:
//
//	offset_i = serverTime_i - (t0_i + t1_i)/2
//	rtt_i    = t1_i - t0_i
//
// The chosen sample is the one with min rtt. The call reuses opts.Doer, so over
// the default keep-alive client the probes ride one warm connection (no
// per-probe TLS handshake polluting the RTT). It makes an unauthenticated call,
// so Creds may be nil. Returns an error only if every probe failed. Each probe's
// HTTP timeout is min(opts.TimeoutMs, maxClockProbeTimeoutMs) — a slower probe's
// estimate is too uncertain to be worth waiting for, and the cap bounds the
// sequential-probe worst case (see maxClockProbeTimeoutMs).
//
// log (nil = silent) receives Debug telemetry: one line per probe (RTT and
// whether it became the new min-delay pick) and one summary line with the chosen
// offset/RTTmin/uncertainty. It carries no secret — /v2/time is public.
func MeasureClockOffset(opts Options, probes int, log *slog.Logger) (ClockOffset, error) {
	lg := logging.Or(log)
	if probes <= 0 {
		probes = defaultClockProbes
	}
	// Cap each probe's HTTP timeout at maxClockProbeTimeoutMs, never above the
	// caller's --timeout: min(caller, cap), treating an unset (<=0) caller timeout
	// as the package default that the cap then trims. opts is a value copy, so this
	// scopes the tighter timeout to the clock probes without touching the caller.
	if opts.TimeoutMs <= 0 || opts.TimeoutMs > maxClockProbeTimeoutMs {
		opts.TimeoutMs = maxClockProbeTimeoutMs
	}
	best := ClockOffset{}
	have := false
	var lastErr error
	for i := 0; i < probes; i++ {
		t0 := now(opts)
		data, err := Execute(Request{Method: "GET", Path: timePath, Auth: false}, opts)
		t1 := now(opts)
		if err != nil {
			lastErr = err
			lg.Debug("clock probe failed", "probe", i, "err", err.Error())
			continue
		}
		var body struct {
			Time *int64 `json:"time"`
		}
		if json.Unmarshal(data, &body) != nil || body.Time == nil {
			lastErr = fmt.Errorf("GET %s: response was not in the expected {time} shape", timePath)
			lg.Debug("clock probe bad shape", "probe", i)
			continue
		}
		rtt := t1 - t0
		if rtt < 0 {
			rtt = 0
		}
		offset := *body.Time - (t0+t1)/2
		best.Samples++
		picked := !have || rtt < best.RTTMinMs
		if picked {
			best.OffsetMs = offset
			best.RTTMinMs = rtt
			have = true
		}
		lg.Debug("clock probe", "probe", i, "rttMs", rtt, "offsetMs", offset, "min", picked)
	}
	if !have {
		if lastErr != nil {
			return ClockOffset{}, lastErr
		}
		return ClockOffset{}, fmt.Errorf("no clock samples taken")
	}
	lg.Debug("clock offset measured",
		"offsetMs", best.OffsetMs, "rttMinMs", best.RTTMinMs,
		"uncertaintyMs", best.UncertaintyMs(), "samples", best.Samples)
	return best, nil
}
