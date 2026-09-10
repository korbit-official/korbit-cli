// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/callrec"
	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/clock"
	"github.com/digitalx-official/digitalx-cli/internal/journal"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/useragent"
)

// NewClockSyncer builds the process-wide clock Syncer for a frontend: one shared
// State measured against baseURL through the injected Doer, with single-flight
// and the default cooldown. surface/detail label the /v2/time probe's
// User-Agent. The server-clock offset is server-wide (key-independent), so a
// single Syncer measured against one base URL corrects signing for every key and
// every surface that shares its State.
func (rt *runtime) NewClockSyncer(state *clock.State, baseURL string, timeoutMs int, surface, detail string) *clock.Syncer {
	log := rt.surfaceLogger(surface)
	measure := func() (int64, int64, error) {
		off, err := apiclient.MeasureClockOffset(apiclient.Options{
			BaseURL:   baseURL,
			Doer:      rt.deps.Doer,
			TimeoutMs: timeoutMs,
			Now:       rt.deps.Now,
			UserAgent: useragent.For(surface, detail),
		}, 0, log)
		if err != nil {
			return 0, 0, err
		}
		return off.OffsetMs, off.UncertaintyMs(), nil
	}
	s := clock.NewSyncer(state, measure, rt.localNow(), clock.DefaultCoolDownMs)
	s.Log = log
	return s
}

// localNow is the raw local clock (unix ms), falling back to the wall clock when
// no test seam is injected.
func (rt *runtime) localNow() func() int64 {
	if rt.deps.Now != nil {
		return rt.deps.Now
	}
	return func() int64 { return time.Now().UnixMilli() }
}

// BuildClient is the cli's single apiclient.Client construction site (the
// clienv.Backend capability). Every surface goes through it, so each client
// uniformly gets its logger, retry-trace Observe, the one per-call journaling
// closure, Origin, User-Agent, shared clock, and resync hook. The per-call
// closure is the same everywhere: during an ops Operation the handle threaded
// through ctx records the call under the operation (or no-ops for a non-recording
// operation); off-operation (an adhoc read like a dry-run preflight) the surface
// recorder records it standalone. A nil recorder — or a nil handle with no
// surface recorder — leaves the call un-journaled.
func (rt *runtime) BuildClient(spec clienv.ClientSpec) *apiclient.Client {
	apiKeyID := ""
	if spec.Creds != nil {
		apiKeyID = spec.Creds.APIKeyID
	}
	c := &apiclient.Client{
		BaseURL:   spec.BaseURL,
		Doer:      rt.deps.Doer,
		Creds:     spec.Creds,
		Clock:     spec.Clock,
		Resync:    spec.Resync,
		TimeoutMs: spec.TimeoutMs,
		Sleep:     rt.deps.Sleep,
		Origin:    apiclient.Origin{Surface: spec.Surface, Detail: spec.Detail},
		UserAgent: useragent.For(spec.Surface, spec.Detail),
		KeyName:   spec.KeyName,
		APIKeyID:  apiKeyID,
		Log:       spec.Log,
	}
	// Retry-decision traces at Debug (gated by the logger's level); the retry
	// layer calls Observe only when it actually retries, so wiring it is cheap.
	// Skipped when the surface keeps no logger (the alt-screen tui without --log-file).
	if spec.Log != nil {
		log := spec.Log
		c.Observe = func(msg string) { log.Debug("retry: " + msg) }
	}
	if spec.Rec != nil {
		rec := spec.Rec
		c.NewRecorder = func(ctx context.Context, call apiclient.Call) apiclient.Recorder {
			if h := ops.HandleFromContext(ctx); h != nil {
				return h.ForCall(string(orderedObject(call.Params)))
			}
			return rec.ForCall(string(orderedObject(call.Params)))
		}
	}
	return c
}

// NewRecorder builds the journal-backed recorder every surface shares: the one
// journal path, the disabled/no-fsync env reads, the single DefaultPolicy, and
// the clock — so the only thing a surface supplies is its own diagnostic logger
// and its post-call-failure sink (the policy's FailMode decides Fail vs Warn;
// the sink decides how to surface it). It is the single callrec.New construction
// site for the cli, the recorder analogue of BuildClient.
func (rt *runtime) NewRecorder(home string, log *slog.Logger, onPostFailure func(callrec.FailMode, error)) *callrec.Recorder {
	rec := callrec.New(
		journal.DefaultPath(home),
		journal.Disabled(rt.deps.Getenv),
		rt.noFsyncMode(),
		callrec.DefaultPolicy(rt.debugMode()),
		rt.deps.Now,
		onPostFailure,
	)
	rec.Log = log
	return rec
}

// LogRecorder is NewRecorder with the common sink: a post-call journal-write
// failure is logged and the command continues. It fits every surface whose
// journal failures are non-fatal diagnostics — doctor, monitor, mcp, and the
// tui's public reads. A surface that must surface the failure differently builds
// its own sink through NewRecorder: runEndpoint captures a Fail into a fatal
// exit, the dry-run preflight frames a read-specific message, and the tui trader
// raises an on-screen toast. A nil log resolves to a no-op logger.
func (rt *runtime) LogRecorder(home string, log *slog.Logger) *callrec.Recorder {
	return rt.NewRecorder(home, log, func(_ callrec.FailMode, err error) {
		logging.Or(log).Warn(formatJournalWarn(err))
	})
}
