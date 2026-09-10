// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package monitorcmd implements the `monitor` command — the CLI's one streaming
// command and its experimental JavaScript bot runtime. It runs against the
// clienv.Cmd seam, so cli (and the mcp/tui frontends) dispatch into it without
// it importing cli.
package monitorcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/accountseq"
	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/botapi"
	"github.com/digitalx-official/digitalx-cli/internal/candles"
	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/cli/probe"
	"github.com/digitalx-official/digitalx-cli/internal/clock"
	"github.com/digitalx-official/digitalx-cli/internal/cmdmeta"
	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/jqfilter"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
	"github.com/digitalx-official/digitalx-cli/internal/useragent"
	"github.com/spf13/cobra"
)

// The monitor command is the CLI's one streaming command: it deliberately
// breaks the "exactly one JSON document" stdout rule, replacing it with a
// line-oriented contract — in JSON mode one JSON object per line (data events
// and stream-health notices, interleaved in emission order), in human mode one
// formatted line per event. Everything else (stderr diagnostics, structured
// errors, exit codes) follows the normal contract. Stopping deliberately
// (Ctrl-C, --duration, --max-events) exits 0.
//
// Per-event processing has three modes, each behind a monitorSink
// (monitorsink.go): plain pass-through, the stable --jq jq filter/transform,
// and the experimental JavaScript bot runtime below. runMonitor's loop only
// pumps events into the active sink and watches for a fatal.
//
// The scripting hooks (--init/--where/--on) run in one shared botapi runtime:
// --where filters data events synchronously; --on is the (possibly async)
// action handler, run serially per event AND for every notice; --init warms
// state up front. api.*/db.* are the Promise-based bot API (see
// internal/botapi). An unhandled --on failure is FATAL by design — an action
// script in an unknown state placing real orders should stop, not keep
// firing: an API rejection exits 3, anything else exits 1.

// predicateWarnIntervalMs rate-limits the stderr warnings that can recur at
// frame rate — a per-event --where/--jq evaluation error, or the
// queue-saturation backlog notice — so one buggy field access (or a sustained
// backlog) can't flood stderr.
const predicateWarnIntervalMs = 2000

// rateLimiter is a minimal "allow at most once per interval" guard over an
// injectable clock (caller passes Now-in-ms). It is extracted so the
// queue-saturation diagnostic is unit-testable without the full stream pipeline.
// The first call always allows (gated on `armed`, so even a Now()==0 clock
// fires once); subsequent calls fire only after the interval has elapsed.
type rateLimiter struct {
	intervalMs int64
	lastAt     int64
	armed      bool
}

func newRateLimiter(intervalMs int64) *rateLimiter { return &rateLimiter{intervalMs: intervalMs} }

// allow reports whether a warning should fire at nowMs, recording the time when
// it does. The first call always fires; subsequent calls fire only once the
// interval has elapsed.
func (r *rateLimiter) allow(nowMs int64) bool {
	if r.armed && nowMs-r.lastAt < r.intervalMs {
		return false
	}
	r.lastAt = nowMs
	r.armed = true
	return true
}

// BotDBDefaultName is the script-local database's name under a CLI home —
// deliberately separate from the action journal file (a bot script must never
// touch the journal), and carrying no product name: the directory already says
// whose data it is. LegacyBotDBFileName is the name a home created under the
// earlier product name carries, used for exactly one home (see botDBFileName).
const (
	BotDBDefaultName    = "bot.db"
	LegacyBotDBFileName = "korbit-bot.db"
)

// botDBSidecars are the write-ahead-log files SQLite keeps beside the bot
// database; they belong to it and are removed with it.
var botDBSidecars = []string{"-wal", "-shm"}

// botDBFileName is the bot database's filename inside home: BotDBDefaultName,
// except in a home whose own directory name is the earlier product's
// (config.LegacyLayout), where it is LegacyBotDBFileName so an older `korbit`
// binary still sharing that directory opens the same file.
func botDBFileName(home string) string {
	if config.LegacyLayout(home) {
		return LegacyBotDBFileName
	}
	return BotDBDefaultName
}

// botDBPath is the default bot-database path under home. The directory decides
// the filename (botDBFileName) and nothing is renamed on open, so this is a pure
// path computation. It is called only when the JavaScript runtime needs the
// DEFAULT database: an explicit --db is used verbatim, and a plain streaming run
// never touches the bot database at all.
func botDBPath(home string) string {
	return filepath.Join(home, botDBFileName(home))
}

// BotDBPaths returns every bot-database file that belongs to home: the database
// under BOTH spellings, each with its sidecars, for a caller removing the CLI's
// data (`self uninstall`). Both are listed because a home whose directory was
// renamed without its files still carries the other spelling.
func BotDBPaths(home string) []string {
	var paths []string
	for _, name := range []string{BotDBDefaultName, LegacyBotDBFileName} {
		db := filepath.Join(home, name)
		paths = append(paths, db)
		for _, sidecar := range botDBSidecars {
			paths = append(paths, db+sidecar)
		}
	}
	return paths
}

// monitorQueueCap is the dispatcher's bounded FIFO between the stream session
// and the consumer goroutine. When the consumer (a jq program or the JS
// runtime) falls behind, the queue fills, the dispatcher blocks, the session's
// own buffer fills, and the WebSocket reads stall — the same well-understood
// back-pressure -> reconnect -> REST-backfill recovery the stream layer already
// implements. No conflation, no silent drops; if a sustained-slow consumer ever
// needs to absorb high-frequency data indefinitely, an eviction ring can
// replace this queue at this exact seam.
const monitorQueueCap = 1024

// Run parses the channel flags, builds the streaming session and the script
// runtime, and pumps events until the session ends or a stop condition is
// reached.
func Run(cx *clienv.Cmd, cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q — monitor takes channel flags, see `%s monitor --help`", args[0], progname.Name())
	}

	// The JavaScript bot runtime (--where/--on/--init and its --db/--max-concurrency
	// helpers) is an experimental, not-yet-stable surface — gate it behind
	// --enable-experimental / DIGITALX_CLI_ENABLE_EXPERIMENTAL so it cannot be reached
	// by accident. This is the REAL enforcement; hiding these flags from plain
	// `--help` (their Experimental bit in the spec) is only cosmetic, so the check
	// here must stand on its own. Plain streaming (channel flags, --duration,
	// --max-events, --no-backfill) needs no opt-in.
	if !cx.Modes.Experimental {
		for _, f := range []string{"where", "on", "init", "db", "max-concurrency", "stateful"} {
			if cmd.Flags().Changed(f) {
				return output.Usagef("--%s is part of monitor's experimental JavaScript bot runtime, which is not yet stable — pass --enable-experimental (or set DIGITALX_CLI_ENABLE_EXPERIMENTAL=1) to use it", f)
			}
		}
	}

	home, cfg, err := cx.LoadConfig()
	if err != nil {
		return err
	}
	sel, err := keys.Select(cx.Key, cx.Getenv)
	if err != nil {
		return err
	}
	km := cx.KeyManager(home, cfg)
	// One metadata peek for the whole session: base/ws host overrides and the
	// default sub-account. Zero for an inline credential (no stored metadata).
	keyMeta := km.MetaForSelection(sel)

	subs, cp, err := monitorSubscriptions(cmd, keyMeta.DefaultAccountSeq)
	if err != nil {
		return err
	}

	where, _ := cmd.Flags().GetString("where")
	onSrc, _ := cmd.Flags().GetString("on")
	initSrc, _ := cmd.Flags().GetString("init")
	stateful, _ := cmd.Flags().GetBool("stateful")
	jsActive := where != "" || onSrc != ""
	if initSrc != "" && !jsActive {
		return output.Usagef("--init has no effect without --where or --on")
	}

	// --jq is the stable filter/transform path; --where/--on/--init are the
	// experimental JavaScript bot runtime. They are mutually exclusive: jq does
	// not act, and running two filter engines in one command would be confusing.
	jqSrc, _ := cmd.Flags().GetString("jq")
	var jqProg *jqfilter.Program
	if jqSrc != "" {
		for _, f := range []string{"where", "on", "init", "db", "max-concurrency", "stateful"} {
			if cmd.Flags().Changed(f) {
				return output.Usagef("--jq cannot be combined with --%s — --jq is the filter/transform path; --where/--on/--init are the experimental JavaScript bot runtime", f)
			}
		}
		if jqProg, err = jqfilter.Compile(jqSrc); err != nil {
			return output.Usagef("%v", err)
		}
	}
	// state.* is only reachable from a JS hook, so --stateful with no script does
	// nothing — refuse it rather than silently build (and feed) a Store nobody
	// reads. Checked after the --jq exclusion above so `--jq --stateful` reports
	// the real conflict (two filter engines) rather than this. (--init alone never
	// qualifies — it is rejected just above — so the fix names --where/--on only.)
	if stateful && !jsActive {
		return output.Usagef("--stateful needs a script — add --where or --on; state.* is unreachable without one")
	}
	maxEvents := 0
	if cmd.Flags().Changed("max-events") {
		raw, _ := cmd.Flags().GetString("max-events")
		if maxEvents, err = clienv.ParseRange(raw, "--max-events", 1, 1_000_000_000); err != nil {
			return err
		}
	}
	var duration time.Duration
	if cmd.Flags().Changed("duration") {
		raw, _ := cmd.Flags().GetString("duration")
		if duration, err = clienv.ParseDuration(raw, "--duration", time.Millisecond, 7*24*time.Hour); err != nil {
			return err
		}
	}
	maxConcurrency := 8
	if cmd.Flags().Changed("max-concurrency") {
		raw, _ := cmd.Flags().GetString("max-concurrency")
		if maxConcurrency, err = clienv.ParseRange(raw, "--max-concurrency", 1, 64); err != nil {
			return err
		}
	}
	noBackfill := false
	if v, _ := cmd.Flags().GetBool("no-backfill"); v {
		noBackfill = true
	}
	noReconnect, _ := cmd.Flags().GetBool("no-reconnect")
	// Stream-layer logging (see internal/stream/doc.go "Logging" for the full
	// contract). Every record shares the operational logger's destination (the
	// --log-file file, else stderr) and is tagged so a reader can filter:
	//   - connection MECHANICS -> component=stream                  (stream.Config.Log)
	//   - state reconcile       -> component=stream/state            (the bot's state store)
	//   - reliability NOTICES   -> component=stream kind=stream_notice code=<CODE>
	// Mechanics + state are ordinary operational diagnostics and follow --log-level
	// (Debug-level call sites, so silent unless --log-level debug). The NOTICES are
	// already emitted on stdout (every sink prints them), so logging them is a
	// separate OPT-IN: --stream-log-level is INDEPENDENT of --log-level and
	// defaults to off — set it to also mirror notices into the log (the --log-file
	// file, else stderr), e.g. when --jq/--max-events narrows stdout.
	// streamLog/stateLog are built here (before stream.New) since the session and
	// the bot runtime need them.
	streamLog := cx.StreamLogger(cx.LogLevel, stream.LogComponentStream)
	stateLog := cx.StreamLogger(cx.LogLevel, stream.LogComponentState)
	noticeLevel := logging.LevelOff // notices live on stdout; the log mirror is opt-in
	if cmd.Flags().Changed("stream-log-level") {
		raw, _ := cmd.Flags().GetString("stream-log-level")
		lvl, ok := logging.ParseLevel(raw)
		if !ok {
			return output.Usagef("--stream-log-level: %q is not a level — use debug, info, warn, error, or off", raw)
		}
		noticeLevel = lvl
	}
	noticeLog := cx.NoticeLogger(noticeLevel)

	private := false
	for _, s := range subs {
		if stream.IsPrivateChannel(s.Channel) {
			private = true
		}
	}

	// --db is validated here so a bad value is a usage error before any work,
	// but the DEFAULT path is resolved only where the JavaScript runtime actually
	// opens it (below): a plain streaming run has no business touching the bot
	// database at all.
	dbOverride := ""
	if cmd.Flags().Changed("db") {
		raw, _ := cmd.Flags().GetString("db")
		if strings.TrimSpace(raw) == "" {
			return output.Usagef("--db must not be empty")
		}
		dbOverride = raw
	}
	// The key in play (the stored key by name, else the default; or inline
	// DIGITALX_CLI_API_KEY_* material) sets the host for the whole session — public
	// stream + backfill included — so a key pinned to a non-prod base never has
	// its public traffic silently routed to prod. An inline credential has no
	// stored host, so the per-key tier does not apply to it.
	// Resolve the REST base and its paired WebSocket base in one step (the private
	// baseTier never crosses the clienv boundary). Validate the REST base BEFORE
	// surfacing the WS resolution error, preserving the precedence the two
	// separate resolve/validate calls had: bad --base-url > bad --ws-base-url
	// resolution > unsigned/plaintext WS rejection.
	baseURL, wsBaseURL, wsErr := cx.ResolveURLs(cmd, cfg, keyMeta.BaseURL, keyMeta.WSBaseURL)
	if err := probe.ValidateBaseURL(baseURL, private); err != nil {
		return err
	}
	if wsErr != nil {
		return wsErr
	}
	if err := probe.ValidateWSBaseURL(wsBaseURL, "--ws-base-url", private); err != nil {
		return err
	}
	timeoutMs := 15000
	if cmd.Flags().Changed("timeout") {
		raw, _ := cmd.Flags().GetString("timeout")
		if timeoutMs, err = clienv.ParseRange(raw, "--timeout", 1, 600000); err != nil {
			return err
		}
	}
	retryBudgetMs := 5000
	if cmd.Flags().Changed("retry-timeout") {
		raw, _ := cmd.Flags().GetString("retry-timeout")
		if retryBudgetMs, err = clienv.ParseRange(raw, "--retry-timeout", 0, 600000); err != nil {
			return err
		}
	}

	publicURL, privateURL := probe.WSURLsFor(wsBaseURL)

	if cx.Modes.DryRun {
		streamLogPlan, _ := cmd.Flags().GetString("stream-log-level")
		doc := PlanDoc(subs, baseURL, publicURL, privateURL, private, where, initSrc, onSrc, jqSrc, stateful, !noBackfill, noReconnect, streamLogPlan)
		if cp.active() {
			// The candle channel is CLI-derived, not a wire subscription; it is
			// listed alongside them (with its intervals) because that is how the
			// user asked for it. The trade subscription it rides on is in subs —
			// marked implicit when it exists only to feed the synthesizer, so a
			// plan reader does not expect trade lines that will never be emitted.
			if cp.implicitTrades {
				for i := range doc.Subscriptions {
					if doc.Subscriptions[i].Channel == stream.ChannelTrade {
						doc.Subscriptions[i].Implicit = true
					}
				}
			}
			doc.Subscriptions = append(doc.Subscriptions, PlanSub{
				Channel: candles.Channel, Symbols: cp.symbols, Intervals: cp.intervals, History: cp.history,
			})
		}
		return cx.Emit("monitor", doc)
	}

	// Pre-stream diagnostics run single-threaded, before any streaming goroutine
	// starts; once streaming begins the same centralized logger is reused as mlog.
	log := cx.Log

	// Credentials are resolved EAGERLY whenever JS is active: a bot may
	// subscribe only public channels yet still place orders. For private
	// channels a resolution failure is fatal (as always); for a public-only
	// subscription it degrades — the monitor runs and authenticated api.*
	// methods throw the resolution error.
	var streamCreds *apiclient.Credentials
	var keyName, apiKeyID, credsErr string
	if private || jsActive {
		resolved, kerr := km.ResolveSelection(sel, cx.Getenv)
		if kerr != nil {
			if private {
				return kerr
			}
			credsErr = kerr.Error()
		} else {
			signer, perr := resolved.Signer()
			if perr != nil {
				if private {
					return perr
				}
				credsErr = perr.Error()
			} else {
				streamCreds = &apiclient.Credentials{APIKeyID: resolved.APIKeyID, Signer: signer}
				keyName, apiKeyID = resolved.Name, resolved.APIKeyID
				// Always-shown safety disclosure (which key/account the bot signs
				// with), not a level-gated log and not part of the stdout stream.
				cx.IO.Notef("%s: signing as key %q", progname.Name(), resolved.Name)
			}
		}
	}
	// Never sign over plaintext http to a non-local host. Private channels
	// were already rejected above (probe.ValidateBaseURL); for a public-only
	// subscription, drop the credentials instead of refusing to stream.
	if streamCreds != nil && !private {
		if err := probe.ValidateBaseURL(baseURL, true); err != nil {
			log.Warn(fmt.Sprintf("%v — authenticated api.* calls are disabled", err))
			credsErr = "credentials are not sent over plaintext http to a non-local host"
			streamCreds = nil
			keyName, apiKeyID = "", ""
		}
	}

	// Ctrl-C / SIGTERM stop the monitor cleanly (exit 0): for a monitor,
	// "the user ended it" is success, not failure. Installed before --init
	// runs, so a long (or runaway) warm-up script is interruptible too.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// ONE shared server-clock estimate covers the whole monitor: the WS upgrade
	// signing, REST backfill, AND the JS api.* calls all sign through it, so
	// an EXCEED_TIME_WINDOW resync triggered by any surface fixes signing for
	// all of them at once. The stream session shares it via its Config.Client
	// (built below); the monitor's own JS-call client shares the same *clock.State.
	localNow := cx.Now
	clk := clock.New(localNow)
	syncer := cx.NewClockSyncer(clk, baseURL, timeoutMs, apiclient.SurfaceMonitor, "clock")

	mlog := cx.Log
	// One journal-backed recorder (internal/callrec) for the whole monitor, built
	// before the session so the stream's backfill reads route through the SAME
	// single-home journaling policy as the JS api.* calls. The policy's surface
	// rows decide what is recorded: "stream-backfill" is exempt (recovery reads,
	// not actions — consulted but never journaled), while a JS call (surface
	// "monitor") is journaled when it is a write, or any call in --debug, warning
	// rather than failing the session on a post-call write error. The DB opens
	// lazily, so a session that journals nothing never touches the file.
	rec := cx.LogRecorder(home, mlog)
	defer rec.Close()

	// The stream's one API handle (the single client builder): REST backfill +
	// public-trade gap patching, the private WS-handshake signing (its Creds, set
	// when a key resolved), and the shared clock (read + resync via the process
	// Syncer, so an EXCEED_TIME_WINDOW resync on any surface — WS upgrade, backfill,
	// the JS client below — fixes signing for all). Its recorder consults the same
	// policy as the JS calls; the "stream-backfill" surface is exempt, so backfill
	// reads are consulted yet not journaled (recovery reads, not actions).
	// One signing context (base URL, creds, shared clock + resync, recorder)
	// shared by the monitor's two clients — the stream-backfill client here and
	// the JS-call client below — each minted via base.as for its own surface,
	// detail, and logger. The shared fields are written once so the two cannot
	// drift in baseURL/creds/clock/resync/recorder.
	// --time-sync off opts out of the reactive EXCEED_TIME_WINDOW resync on every
	// surface this client signs (WS upgrade, backfill, the JS client).
	var streamResync func() error
	if cx.Modes.TimeSync.Reactive() {
		streamResync = syncer.Sync
	}
	base := clienv.ClientSpec{
		BaseURL:   baseURL,
		Creds:     streamCreds,
		KeyName:   keyName,
		Clock:     clk,
		Resync:    streamResync,
		TimeoutMs: timeoutMs,
		Rec:       rec,
	}
	streamClient := cx.BuildClient(base.As(apiclient.SurfaceStreamBackfill, "", streamLog))

	// The candle synthesizer (--candles) derives the candle channel from the
	// trade stream, seeded from REST via the candles operation (which auto-pages
	// past the server's per-request cap). Its seed client shares the one signing
	// context (clock, recorder) on the stream-backfill surface — recovery reads,
	// consulted by the journaling policy but never journaled.
	var synth *candles.Synth
	if cp.active() {
		seedClient := cx.BuildClient(base.As(apiclient.SurfaceStreamBackfill, "candles", streamLog))
		api := ops.NewAPI(seedClient)
		api.RetryBudgetMs = retryBudgetMs
		api.Journal = rec
		candlesOp := ops.Find("candles")
		fetch := func(fctx context.Context, symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
			// Values are pre-validated (symbols by the --symbols kind, intervals
			// against the canonical enum, the limit computed internally), so the
			// map is built directly in wire (API) names.
			values := map[string]string{
				"symbol": symbol, "interval": interval, "limit": strconv.Itoa(limit),
			}
			if endMs > 0 {
				values["endTime"] = strconv.FormatInt(endMs, 10)
			}
			res, ferr := candlesOp.Run(fctx, api, ops.RunInput{
				Values:   values,
				Controls: ops.Controls{Surface: apiclient.SurfaceStreamBackfill},
			})
			if ferr != nil {
				return nil, ferr
			}
			var rows []candles.Bar
			if uerr := json.Unmarshal(res.Data, &rows); uerr != nil {
				return nil, fmt.Errorf("candles response: %w", uerr)
			}
			return rows, nil
		}
		if synth, err = candles.NewSynth(candles.Config{
			Symbols: cp.symbols, Intervals: cp.intervals, History: cp.history,
			Fetch: fetch, ServerNow: seedClient.ServerNowMs, Now: cx.Now, Log: streamLog,
		}); err != nil {
			return output.Usagef("%v", err)
		}
	}

	sess, err := stream.New(stream.Config{
		PublicURL:         publicURL,
		PrivateURL:        privateURL,
		Subscriptions:     subs,
		Client:            streamClient,
		ProactiveTimeSync: cx.Modes.TimeSync.Proactive(),
		Log:               streamLog,
		DisableBackfill:   noBackfill,
		NoReconnect:       noReconnect,
		Dial:              cx.WSDial,
		UserAgent:         useragent.For(apiclient.SurfaceStreamWS, ""),
		Now:               cx.Now,
		Sleep:             cx.Sleep,
	})
	if err != nil {
		return output.Usagef("%v", err)
	}

	// Streaming-phase side-channel output on stderr (the bot's console.log,
	// warning/honesty notes) shares the one locked stderr sink (cx.IO.Err). The
	// operational logger (cx.Log) writes to the same sink when logs go to stderr
	// (so log lines interleave atomically with notifications on the one fd), or
	// the --log-file file when diverted there.
	var bot *botapi.Runtime
	if jsActive {
		// The monitor's own L1 client for JS api.* calls: same BaseURL/Doer/
		// timeout as the WS/backfill side, signing through the SHARED clock so
		// one resync fixes every surface. Creds are attached only when a key
		// resolved (else authenticated methods degrade via CredsErr in botapi).
		jsClient := cx.BuildClient(base.As(apiclient.SurfaceMonitor, "botapi", mlog))
		api := ops.NewAPI(jsClient)
		api.RetryBudgetMs = retryBudgetMs
		api.Stderr = cx.IO.Err
		api.Journal = rec
		dbPath := dbOverride
		if dbPath == "" {
			dbPath = botDBPath(home)
		}
		bot, err = botapi.New(botapi.Options{
			Where:          where,
			On:             onSrc,
			Init:           initSrc,
			API:            api,
			Surface:        apiclient.SurfaceMonitor,
			KeyName:        keyName,
			APIKeyID:       apiKeyID,
			KeyManager:     km,
			Inline:         sel.Inline,
			CredsErr:       credsErr,
			DBPath:         dbPath,
			NoFsync:        cx.Modes.NoFsync,
			Stateful:       stateful,
			AccountSeqs:    privateAccountSeqs(subs),
			MaxConcurrency: maxConcurrency,
			ServerNow:      jsClient.ServerNowMs,
			Now:            cx.Now,
			Sleep:          cx.Sleep,
			Stderr:         cx.IO.Err,
			Log:            stateLog,
			SignalCtx:      sigCtx,
		})
		if err != nil {
			if sigCtx.Err() != nil {
				return nil // the user aborted during --init — a clean stop
			}
			var ae *output.ApiError
			if errors.As(err, &ae) {
				return err // an API rejection during --init keeps exit 3
			}
			return output.Usagef("%v", err)
		}
		defer bot.Close()
	}

	// --duration bounds the streaming phase (it starts after --init, which is
	// bounded only by Ctrl-C — REST warm-up takes as long as it takes).
	ctx := sigCtx
	if duration > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, duration)
		defer cancelTimeout()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The streaming-phase shutdown context interrupts the script VM: with I/O
	// off the loop, only a pure-CPU runaway in --where/--on can stall it, and
	// this lets Ctrl-C/SIGTERM/--duration unwind it. (--init was already
	// bounded by the bare signal context inside botapi.New.)
	if bot != nil {
		bot.WatchInterrupt(ctx)
	}

	// The synthesizer's seed fetches are bounded by the streaming context; the
	// seeds themselves are kicked by each symbol's trade subscribe snapshot
	// (see the candles package doc's seed-ordering rationale).
	if synth != nil {
		synth.Start(ctx)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- sess.Run(ctx) }()

	// Dispatcher: drains the session immediately (the session never stalls on
	// JS) into the bounded FIFO the controller consumes. See monitorQueueCap
	// for the back-pressure story; this is also the seam a future eviction
	// ring would replace.
	//
	// Back-pressure is by design but otherwise invisible to the user until the
	// reconnect storm it eventually triggers. So before each blocking send,
	// probe for a full queue (a non-blocking send with a default) and, when the
	// script has fallen behind, emit a RATE-LIMITED warning through the
	// diagnostic logger so the cause is named early. The warn guard uses the
	// injectable clock so it is testable.
	queue := make(chan stream.Event, monitorQueueCap)
	go func() {
		defer close(queue)
		backlogWarn := newRateLimiter(predicateWarnIntervalMs)
		for ev := range sess.Events() {
			select {
			case queue <- ev:
			default:
				// The FIFO is full: the script is not keeping up. Warn (rate
				// limited), then do the normal blocking send — back-pressure
				// into reconnect + REST backfill is still the recovery.
				if backlogWarn.allow(cx.Now()) {
					mlog.Warn("processing is falling behind the event stream (buffer full) — events are back-pressuring; a sustained backlog will force a reconnect")
				}
				queue <- ev
			}
		}
	}()

	// --jq is a JSON transform, so it implies JSON output (data lines and
	// notices both render as one JSON object per line).
	printer := monitorPrinter{io: cx.IO, log: mlog, jsonMode: cx.Modes.JSONMode || jqProg != nil}
	em := &emitter{
		maxEvents: maxEvents,
		ctx:       ctx,
		cancel:    cancel,
		warn:      newRateLimiter(predicateWarnIntervalMs),
		log:       mlog,
		now:       cx.Now,
	}
	// One sink per mode (plain pass-through, jq filter/transform, or the JS
	// bot) owns what each event becomes; this loop only pumps events and
	// watches for a fatal. plain/jq sinks have a nil fatal() channel, so that
	// select case never fires for them.
	var sink monitorSink
	switch {
	case jqProg != nil:
		sink = &jqSink{p: printer, em: em, prog: jqProg}
	case bot != nil:
		sink = &botSink{p: printer, em: em, bot: bot, where: where, onSrc: onSrc}
	default:
		sink = &plainSink{p: printer, em: em}
	}

	// deliver routes one event (from the session or the candle synthesizer)
	// into the sink. A notice is mirrored into the stream logger at its level
	// FIRST — outside the --jq filter and the --on handler, so a reliability
	// signal is captured even when those drop it from stdout. noticeLog's
	// threshold (--stream-log-level, off by default) decides whether it lands;
	// its destination follows --log-file.
	deliver := func(ev stream.Event) {
		switch e := ev.(type) {
		case stream.Notice:
			stream.LogNotice(noticeLog, e)
			sink.onNotice(e)
		case stream.Data:
			sink.onData(e)
		}
	}

	// The candle synthesizer's two extra wake-ups: finished seed fetches, and a
	// 1s tick that fires due seed retries and finalizes quiet-market buckets on
	// time. Both channels are nil without --candles, so those cases never fire.
	var seedResults <-chan candles.SeedResult
	var candleTick <-chan time.Time
	if synth != nil {
		seedResults = synth.Results()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		candleTick = ticker.C
	}

loop:
	for {
		select {
		case ferr := <-sink.fatal():
			sink.recordFatal(ferr)
		case res := <-seedResults:
			for _, dev := range synth.Apply(res) {
				deliver(dev)
			}
		case <-candleTick:
			for _, dev := range synth.Tick() {
				deliver(dev)
			}
		case ev, ok := <-queue:
			if !ok {
				break loop
			}
			// The synthesizer sees every session event first (trades fold into
			// candles, notices drive re-seeds), and its derived events are
			// delivered AFTER the source event, so emission order stays causal.
			var derived []stream.Event
			if synth != nil {
				switch e := ev.(type) {
				case stream.Notice:
					derived = synth.OnNotice(e)
				case stream.Data:
					derived = synth.OnData(e)
				}
			}
			// A trade subscription that exists only to feed the synthesizer is
			// suppressed end to end (output, --jq, --where/--on, --stateful): the
			// user asked for candles, and sees them as a channel of their own.
			suppressed := false
			if d, isData := ev.(stream.Data); isData && cp.implicitTrades && d.Channel == stream.ChannelTrade {
				suppressed = true
			}
			if !suppressed {
				deliver(ev)
			}
			for _, dev := range derived {
				deliver(dev)
			}
		}
	}
	err = <-runErr
	// A rejection that landed as the stream closed must still be fatal.
	select {
	case ferr := <-sink.fatal():
		sink.recordFatal(ferr)
	default:
	}

	if fe := sink.fatalErr(); fe != nil {
		return fe // EmitError classifies: ApiError -> 3, anything else -> 1
	}
	if err != nil && ctx.Err() == nil {
		return RunError(err)
	}
	return nil
}

// dataBotEvent maps a stream data event onto the script's event shape.
func dataBotEvent(e stream.Data) botapi.Event {
	return botapi.Event{
		Type: "data", Channel: e.Channel, Symbol: e.Symbol, Origin: string(e.Origin),
		ServerTime: e.ServerTime, Source: e.Source, AccountSeq: e.AccountSeq, Payload: e.Payload,
	}
}

// noticeBotEvent maps a notice onto the script's event shape: channel
// 'notice', payload = the notice object — load-bearing for safety bots ("on
// DISCONNECTED or DATA_GAP, cancel my open orders"). --where never sees
// notices; --on always does.
func noticeBotEvent(n stream.Notice) botapi.Event {
	payload, err := json.Marshal(monitorNoticeLine{
		Type: "notice", Code: string(n.Code), Level: string(n.Level),
		Message: n.Message, Details: n.Details, Time: n.Time,
	})
	if err != nil {
		payload = []byte(fmt.Sprintf(`{"type":"notice","code":%q}`, string(n.Code)))
	}
	return botapi.Event{Type: "notice", Channel: "notice", ServerTime: n.Time, Payload: payload}
}

// candlePlan is the parsed --candles configuration: the synthesized candle
// channel's symbols/intervals and seed depth, plus whether the trade
// subscription exists only to feed the synthesizer (in which case its lines
// are suppressed from output, so the user sees the candle channel as if it
// were a subscription of its own).
type candlePlan struct {
	symbols        []string
	intervals      []string
	history        int
	implicitTrades bool
}

// active reports whether --candles was requested.
func (cp candlePlan) active() bool { return len(cp.intervals) > 0 }

// monitorSubscriptions maps the shared --symbols list plus the boolean channel
// flags onto stream subscriptions, and parses the --candles plan. Every
// enabled symbol channel subscribes the same --symbols set (the common case),
// validated with the same rules as endpoint commands; --my-assets is
// account-wide and takes no symbols. --candles subscribes the trade channel
// under the hood when --trades is absent (the synthesizer folds trades into
// candles), marked implicit so the raw trade lines are suppressed.
func monitorSubscriptions(cmd *cobra.Command, acctSeqDefault string) ([]stream.Subscription, candlePlan, error) {
	var cp candlePlan
	var symbols []string
	if cmd.Flags().Changed("symbols") {
		raw, _ := cmd.Flags().GetString("symbols")
		v, err := cmdmeta.NormalizeValue(cmdmeta.Param{Kind: cmdmeta.KindSymbols}, raw, "--symbols")
		if err != nil {
			return nil, cp, err
		}
		symbols = strings.Split(v, ",")
	}

	channelFlags := []struct {
		flag    string
		channel string
	}{
		{"ticker", stream.ChannelTicker},
		{"orderbook", stream.ChannelOrderbook},
		{"trades", stream.ChannelTrade},
		{"my-orders", stream.ChannelMyOrder},
		{"my-trades", stream.ChannelMyTrade},
	}
	var subs []stream.Subscription
	needSymbols := false // any subscribed channel that is scoped to symbols
	for _, cf := range channelFlags {
		if on, _ := cmd.Flags().GetBool(cf.flag); !on {
			continue
		}
		needSymbols = true
		sub := stream.Subscription{Channel: cf.channel, Symbols: symbols}
		if cf.channel == stream.ChannelTrade && cmd.Flags().Changed("trade-history") {
			raw, _ := cmd.Flags().GetString("trade-history")
			n, err := clienv.ParseRange(raw, "--trade-history", 1, 500)
			if err != nil {
				return nil, cp, err
			}
			sub.TradeHistory = n
		}
		subs = append(subs, sub)
	}
	if cmd.Flags().Changed("candles") {
		raw, _ := cmd.Flags().GetString("candles")
		ivs, err := parseCandleIntervals(raw)
		if err != nil {
			return nil, cp, err
		}
		if nb, _ := cmd.Flags().GetBool("no-backfill"); nb {
			// The candle channel is DEFINED by its guarantees: the current bucket
			// is seeded from REST (a mid-bucket start cannot be reconstructed from
			// trades alone) and gaps are healed from REST. Without backfill those
			// guarantees cannot be met, so refuse rather than stream wrong bars.
			return nil, cp, output.Usagef("--candles cannot be combined with --no-backfill — candles are seeded and gap-healed from REST; drop one of them")
		}
		cp.intervals = ivs
		cp.symbols = symbols
		needSymbols = true
		// The synthesizer folds public trades; subscribe them under the hood when
		// the user did not, marked implicit so the raw trade lines are suppressed.
		if on, _ := cmd.Flags().GetBool("trades"); !on {
			subs = append(subs, stream.Subscription{Channel: stream.ChannelTrade, Symbols: symbols})
			cp.implicitTrades = true
		}
	}
	if cmd.Flags().Changed("candle-history") {
		if !cp.active() {
			return nil, cp, output.Usagef("--candle-history applies to --candles; add --candles or drop --candle-history")
		}
		raw, _ := cmd.Flags().GetString("candle-history")
		n, err := clienv.ParseRange(raw, "--candle-history", 1, ops.CandlesMaxLimit)
		if err != nil {
			return nil, cp, err
		}
		cp.history = n
	}
	if v, _ := cmd.Flags().GetBool("my-assets"); v {
		subs = append(subs, stream.Subscription{Channel: stream.ChannelMyAsset})
	}
	if len(subs) == 0 {
		return nil, cp, output.Usagef("monitor needs at least one channel: --ticker, --orderbook, --trades, --candles, --my-orders, --my-trades, or --my-assets")
	}
	// The symbol channels need a symbol set; --my-assets alone does not.
	if needSymbols && len(symbols) == 0 {
		return nil, cp, output.Usagef("--symbols is required with --ticker/--orderbook/--trades/--candles/--my-orders/--my-trades — pass e.g. --symbols btc_krw,eth_krw")
	}
	if len(symbols) > 0 && !needSymbols {
		return nil, cp, output.Usagef("--symbols has no effect with only --my-assets (which is account-wide) — add --ticker/--orderbook/--trades/--candles/--my-orders/--my-trades or drop --symbols")
	}
	if cmd.Flags().Changed("trade-history") {
		if on, _ := cmd.Flags().GetBool("trades"); !on {
			return nil, cp, output.Usagef("--trade-history applies to --trades; add --trades or drop --trade-history")
		}
	}
	var explicitSeqs []int
	if cmd.Flags().Changed("account-seq") {
		raw, _ := cmd.Flags().GetString("account-seq")
		var err error
		explicitSeqs, err = parseAccountSeqs(raw)
		if err != nil {
			return nil, cp, err
		}
	}
	seqs, err := accountseq.ResolveList(explicitSeqs, accountseq.Inputs{Default: acctSeqDefault})
	if err != nil {
		return nil, cp, err
	}
	applied := false
	for i := range subs {
		if stream.IsPrivateChannel(subs[i].Channel) {
			subs[i].AccountSeqs = seqs
			applied = true
		}
	}
	if len(explicitSeqs) > 0 && !applied {
		return nil, cp, output.Usagef("--account-seq applies to the private channels; add --my-orders/--my-trades/--my-assets or drop --account-seq")
	}
	return subs, cp, nil
}

// parseCandleIntervals parses the --candles value: a comma-separated list of
// candle intervals, canonical REST enum values ONLY (the same tokens the
// candles endpoint takes — no aliases like "1m"). Duplicates are rejected so a
// typo never silently double-subscribes an interval.
func parseCandleIntervals(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	ivs := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !candles.ValidInterval(p) {
			return nil, output.Usagef("--candles: %q is not a candle interval — use the candles endpoint's values: %s", p, strings.Join(candles.Intervals, ", "))
		}
		if seen[p] {
			return nil, output.Usagef("--candles: %s listed more than once", p)
		}
		seen[p] = true
		ivs = append(ivs, p)
	}
	return ivs, nil
}

// privateAccountSeqs is the deduped set of sub-accounts covered by the private
// subscriptions — what state.ready() rolls its per-account readiness up over.
// Empty when no private channels are subscribed (a public-only session), which
// keeps state.ready().balances/.openOrders false. Every private sub carries the
// same ResolveList-normalized (≥1) seqs, matching the store's readiness keys.
func privateAccountSeqs(subs []stream.Subscription) []int {
	seen := map[int]bool{}
	var out []int
	for _, sub := range subs {
		if !stream.IsPrivateChannel(sub.Channel) {
			continue
		}
		for _, seq := range sub.AccountSeqs {
			if !seen[seq] {
				seen[seq] = true
				out = append(out, seq)
			}
		}
	}
	return out
}

// parseAccountSeqs parses the --account-seq value: a comma-separated list of
// positive sub-account sequence numbers (e.g. "1,2"). Duplicates are rejected so
// a typo never silently double-subscribes an account.
func parseAccountSeqs(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	seqs := make([]int, 0, len(parts))
	seen := map[int]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, output.Usagef("--account-seq: %q is not a positive sub-account number — pass a comma-separated list like 1,2", p)
		}
		if seen[n] {
			return nil, output.Usagef("--account-seq: %d listed more than once", n)
		}
		seen[n] = true
		seqs = append(seqs, n)
	}
	return seqs, nil
}

// RunError classifies a fatal session error for the exit-code contract:
// a rejected WebSocket handshake carrying a Digital X error envelope is an API
// rejection (exit 3, with the symbolic code preserved); anything else surfaces
// as-is (untyped → exit 1).
func RunError(err error) error {
	var ue *stream.UpgradeError
	if errors.As(err, &ue) {
		return &output.ApiError{Message: err.Error(), HTTPStatus: ue.Status, Code: ue.Code}
	}
	return err
}

// PlanSub is one subscription line of a Plan. The synthesized candle channel
// appears as its own entry carrying Intervals (and History when requested),
// alongside the wire trade subscription that feeds it.
type PlanSub struct {
	Channel     string   `json:"channel"`
	Symbols     []string `json:"symbols,omitempty"`
	AccountSeqs []int    `json:"accountSeqs,omitempty"`
	Intervals   []string `json:"intervals,omitempty"`
	History     int      `json:"history,omitempty"`
	// Implicit marks a subscription made only to feed a derived channel (the
	// trade sub under --candles without --trades): it is real on the wire, but
	// its own data lines are suppressed from output.
	Implicit bool `json:"implicit,omitempty"`
}

// Plan is the --dry-run document: what would be streamed, from where, with which
// recovery and script settings — nothing is dialed. It implements
// textout.TextFormatter (see format.go) so the emitter renders the human plan
// and marshals this struct in --json mode. Reused by the tui's --dry-run.
type Plan struct {
	DryRun         bool      `json:"dryRun"`
	Subscriptions  []PlanSub `json:"subscriptions"`
	PublicURL      string    `json:"wsPublicUrl"`
	PrivateURL     string    `json:"wsPrivateUrl,omitempty"`
	RESTBaseURL    string    `json:"restBaseUrl"`
	Auth           bool      `json:"auth"`
	Backfill       bool      `json:"backfill"`
	Where          string    `json:"where,omitempty"`
	Init           string    `json:"init,omitempty"`
	On             string    `json:"on,omitempty"`
	Jq             string    `json:"jq,omitempty"`
	Stateful       bool      `json:"stateful,omitempty"`
	NoReconnect    bool      `json:"noReconnect,omitempty"`
	StreamLogLevel string    `json:"streamLogLevel,omitempty"`
	// Note is an optional caveat about the plan. Additive and monitor leaves it
	// empty; the tui uses it to say its account coverage only resolves at startup.
	Note string `json:"note,omitempty"`
}

// PlanDoc builds the --dry-run Plan.
func PlanDoc(subs []stream.Subscription, baseURL, publicURL, privateURL string, private bool, where, initSrc, onSrc, jqSrc string, stateful, backfill, noReconnect bool, streamLog string) Plan {
	if publicURL == "" {
		publicURL = stream.DefaultPublicURL
	}
	if privateURL == "" {
		privateURL = stream.DefaultPrivateURL
	}
	plan := Plan{
		DryRun: true, PublicURL: publicURL, RESTBaseURL: baseURL,
		Auth: private, Backfill: backfill, Where: where, Init: initSrc, On: onSrc, Jq: jqSrc,
		Stateful: stateful, NoReconnect: noReconnect, StreamLogLevel: streamLog,
	}
	if private {
		plan.PrivateURL = privateURL
	}
	for _, s := range subs {
		plan.Subscriptions = append(plan.Subscriptions, PlanSub{Channel: s.Channel, Symbols: s.Symbols, AccountSeqs: s.AccountSeqs})
	}
	return plan
}

// monitorPrinter writes one line per event. JSON mode is the machine contract
// (stable field names, payload verbatim); human mode is a timestamped line.
type monitorPrinter struct {
	io       output.IO
	log      *slog.Logger
	jsonMode bool
}

// monitorDataLine is the JSON-mode shape of a data event. Field names are a
// stable contract — extend additively, never rename.
type monitorDataLine struct {
	Type       string `json:"type"` // "data"
	Channel    string `json:"channel"`
	Symbol     string `json:"symbol,omitempty"`
	Origin     string `json:"origin"`
	ServerTime int64  `json:"serverTime"`
	Source     string `json:"source,omitempty"`
	// AccountSeq is the sub-account of a private-channel event (1 = main),
	// omitted for public data and for a private frame the server did not tag
	// with an accountSeq (it does so only when the subscription requested
	// accountSeqs).
	AccountSeq *int            `json:"accountSeq,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// monitorNoticeLine is the JSON-mode shape of a notice. Same stability rule.
type monitorNoticeLine struct {
	Type    string         `json:"type"` // "notice"
	Code    string         `json:"code"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Time    int64          `json:"time"`
}

func (p monitorPrinter) data(d stream.Data) {
	if p.jsonMode {
		p.line(monitorDataLine{
			Type: "data", Channel: d.Channel, Symbol: d.Symbol, Origin: string(d.Origin),
			ServerTime: d.ServerTime, Source: d.Source, AccountSeq: d.AccountSeq, Payload: d.Payload,
		})
		return
	}
	fmt.Fprintf(p.io.Out, "%s %-9s %-10s %s %s\n",
		fmtClock(d.ServerTime), d.Channel, d.Symbol, d.Origin, strings.TrimSpace(string(d.Payload)))
}

func (p monitorPrinter) notice(n stream.Notice) {
	if p.jsonMode {
		p.line(monitorNoticeLine{
			Type: "notice", Code: string(n.Code), Level: string(n.Level),
			Message: n.Message, Details: n.Details, Time: n.Time,
		})
		return
	}
	fmt.Fprintf(p.io.Out, "%s %-5s %s %s\n", fmtClock(n.Time), strings.ToUpper(string(n.Level)), n.Code, n.Message)
}

// line writes one JSON document on one line. A marshal failure cannot
// realistically happen (all fields are marshalable); it is reported on stderr
// rather than silently dropped.
func (p monitorPrinter) line(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		p.log.Warn(fmt.Sprintf("dropping unmarshalable event: %v", err))
		return
	}
	fmt.Fprintf(p.io.Out, "%s\n", b)
}

// raw writes one already-encoded JSON document (a --jq output line) verbatim.
func (p monitorPrinter) raw(b []byte) { fmt.Fprintf(p.io.Out, "%s\n", b) }

// dataLineBytes is the canonical JSON-line encoding of a data event — exactly
// what the printer emits in JSON mode. It is the document --jq runs over, and
// the bytes re-emitted verbatim when a jq filter passes an event unchanged, so
// a filtered event is byte-identical to a non-jq JSON-mode line.
func dataLineBytes(d stream.Data) []byte {
	b, err := json.Marshal(monitorDataLine{
		Type: "data", Channel: d.Channel, Symbol: d.Symbol, Origin: string(d.Origin),
		ServerTime: d.ServerTime, Source: d.Source, AccountSeq: d.AccountSeq, Payload: d.Payload,
	})
	if err != nil {
		// Payload is a json.RawMessage, so marshal only fails on a malformed
		// frame; degrade to a minimal valid line rather than dropping it.
		b = []byte(fmt.Sprintf(`{"type":"data","channel":%q}`, d.Channel))
	}
	return b
}

// noticeLineBytes is the canonical JSON-line encoding of a notice — identical
// to what the printer emits in JSON mode — so --jq runs over notices in the
// same shape an agent sees, and a passed-through notice is byte-identical to a
// non-jq notice line.
func noticeLineBytes(n stream.Notice) []byte {
	b, err := json.Marshal(monitorNoticeLine{
		Type: "notice", Code: string(n.Code), Level: string(n.Level),
		Message: n.Message, Details: n.Details, Time: n.Time,
	})
	if err != nil {
		b = []byte(fmt.Sprintf(`{"type":"notice","code":%q}`, string(n.Code)))
	}
	return b
}

func fmtClock(unixMs int64) string {
	return time.UnixMilli(unixMs).Format("15:04:05.000")
}
