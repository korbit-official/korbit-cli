// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package tuicmd implements the `tui` command — the interactive, full-screen
// trading terminal (the human sibling of monitor). It runs against the
// clienv.Cmd seam so cli dispatches into it without it importing cli.
package tuicmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/callrec"
	"github.com/korbit-official/korbit-cli/internal/candles"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/monitorcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui"
	"github.com/korbit-official/korbit-cli/internal/useragent"
	"github.com/spf13/cobra"
)

// The tui command is the interactive sibling of monitor: the same stream
// layer underneath, but rendered as a full-screen terminal for a human, with
// order entry. It deliberately has no machine output (--json is rejected;
// agents use monitor), and stopping it deliberately exits 0.

// Run resolves config/key/URLs, starts a stream session, and hands its events
// to the tui package. Order placement/cancellation run through tuiTrader — the
// same validated, journaled ops path as `order place`/`order cancel`. tuiRun is
// the interactive renderer (the Deps.TUIRun test seam; nil = the real tui.Run,
// which needs a TTY) — passed in rather than crossing clienv, since its
// tui.Config signature would drag the charm dependency into the seam.
func Run(cx *clienv.Cmd, cmd *cobra.Command, args []string, tuiRun func(tui.Config) error) error {
	if len(args) > 0 {
		return output.Usagef("tui takes no positional arguments — pass pairs with --symbols (e.g. --symbols btc_krw,eth_krw), or omit it to watch every launched pair")
	}
	var symbols []string
	if raw, _ := cmd.Flags().GetString("symbols"); raw != "" {
		normalized, nerr := cmdmeta.NormalizeValue(cmdmeta.Param{Kind: cmdmeta.KindSymbols}, raw, "--symbols")
		if nerr != nil {
			return nerr
		}
		symbols = strings.Split(normalized, ",")
	}

	public, _ := cmd.Flags().GetBool("public")
	private := !public

	// The tui localizes its chrome, so activate the run's detected display
	// language (resolved and validated centrally at startup from the global
	// --lang / OS locale — see i18n.Resolve). Other surfaces leave it English.
	if err := i18n.Activate(i18n.Detected()); err != nil {
		return err // unreachable: Detected() is always a supported code
	}

	if cx.Modes.JSONMode && !cx.Modes.DryRun {
		return output.Usagef("tui is interactive and has no JSON output — use `korbit-cli monitor` for machine-readable streaming")
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
	// The key in play (the stored key by name, else the default; or inline
	// KORBIT_CLI_API_KEY_* material) sets the host for the whole session, public
	// (--public) included — so a key pinned to a non-prod base never has its
	// public market-data traffic silently routed to prod. Stored keys also supply
	// the default accountSeq; inline credentials have no stored metadata, so those
	// per-key tiers do not apply.
	keyMeta := km.MetaForSelection(sel)
	// Resolve the REST base and its paired WebSocket base together (the private
	// baseTier never crosses the clienv boundary); validate the REST base before
	// surfacing the WS resolution error, preserving the prior precedence.
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
	// Sub-account selection. --account-seq accepts a single value (the classic
	// one-account session), a comma-separated list ("1,2,3" — subscribe several,
	// active on the first), or is omitted (subscribe every account the key may
	// access, active on the key's configured default). The account channels
	// (orders/fills/balances) cover every subscribed sub-account; order entry and
	// the panels follow the active one, switchable in the TUI ('@'). The explicit
	// list is parsed here; the subscribe set and the active account are finalized
	// once the key's allowedAccountSeqs is known (private mode, below).
	accountSeqRaw := ""
	if cmd.Flags().Changed("account-seq") {
		accountSeqRaw, _ = cmd.Flags().GetString("account-seq")
		if strings.TrimSpace(accountSeqRaw) == "" {
			return output.Usagef("--account-seq must not be empty")
		}
		if public {
			// Public market data carries no sub-account, so --account-seq has no
			// channel to attach to and the '@' switcher has nothing to switch —
			// reject the contradiction rather than silently ignore it.
			return output.Usagef("--account-seq selects the sub-accounts for the private account channels and has no effect with --public — drop one of them")
		}
	}
	explicitSeqs, err := accountseq.ParseList(accountSeqRaw, accountseq.Inputs{Param: accountseq.Param(), Label: "--account-seq"})
	if err != nil {
		return err
	}
	// A provisional active account for the dry-run plan and public mode, before
	// any key-info fetch: the first explicit value, else the standard precedence
	// (the key's configured default, else 1/main).
	activeSeq := 0
	if len(explicitSeqs) > 0 {
		activeSeq = explicitSeqs[0]
	} else if activeSeq, err = accountseq.Resolve("", accountseq.Inputs{Default: keyMeta.DefaultAccountSeq, Param: accountseq.Param(), Label: "--account-seq"}); err != nil {
		return err
	}
	// The session's subscribed sub-accounts. Provisionally the explicit list (or
	// the single active account); a private session with --account-seq omitted
	// replaces this with the key's full allowed set (below).
	accountSeqs := explicitSeqs
	if len(accountSeqs) == 0 {
		accountSeqs = []int{activeSeq}
	}

	// Operational logging for the TUI. Under the full-screen alt-screen stderr is
	// unusable, so the TUI emits operational logs only when --log-file routes them
	// to a file; otherwise tuiLog is nil and tuiLogSink is io.Discard, and the
	// session stays silent on stderr (surfacing failures as on-screen toasts).
	tuiLog, tuiLogSink := tuiLogging(cx)

	// The TUI's public market-data reads — the launched-pairs list and the candle
	// chart — journal through one shared recorder under DefaultPolicy, so they show
	// up in `korbit logs` under --debug like every other surface's public reads
	// (the call site no longer decides journaling). Its log-only sink keeps a
	// journal hiccup off the order toast, and it is safe for the chart goroutine to
	// share (callrec mints a per-call recorder). The trader's order calls use a
	// separate recorder with the toast sink, built below.
	publicRec := cx.LogRecorder(home, tuiLog)
	defer publicRec.Close()

	// No --symbols: watch every currently launched pair (public /v2/currencyPairs).
	if len(symbols) == 0 {
		symbols, err = launchedPairs(cx, context.Background(), baseURL, timeoutMs, publicRec)
		if err != nil {
			// Wrap (not Usagef): a fetch failure is a network/API error, so it
			// keeps the underlying classification (exit 1/3) like doctor/monitor
			// — the message still points at --symbols as the manual fallback.
			return fmt.Errorf("could not list launched trading pairs (pass --symbols to choose pairs explicitly): %w", err)
		}
		if len(symbols) == 0 {
			return output.Usagef("no launched trading pairs were returned — pass --symbols to choose pairs to watch")
		}
	}

	publicURL, privateURL := probe.WSURLsFor(wsBaseURL)

	if cx.Modes.DryRun {
		// The plan is offline — no signed key-info fetch. An explicit --account-seq
		// is shown verbatim; with it omitted the real run's account set (the key's
		// allowedAccountSeqs, resolved at startup) is unknowable here, so the account
		// channels are shown WITHOUT a concrete set and a note says where it comes
		// from — rather than fabricating a set a real run would not match.
		planSeqs := accountSeqs
		note := ""
		if private && len(explicitSeqs) == 0 {
			planSeqs = nil
			note = "account channels cover every sub-account this key can access, resolved from the key at startup — pass --account-seq to plan a specific set"
		}
		subs := tuiSubscriptions(symbols, symbols[0], private, planSeqs)
		plan := monitorcmd.PlanDoc(subs, baseURL, publicURL, privateURL, private, "", "", "", "", false, true, false, "")
		plan.Note = note
		return cx.Emit("tui", plan)
	}

	// The real TUI needs a terminal; the injected runner (tests) owns its IO.
	runFn := tuiRun
	if runFn == nil {
		if !writerIsTerminal(cx.IO.Out) {
			return output.Usagef("tui needs an interactive terminal (stdout is not a TTY) — use `korbit-cli monitor` for piped or machine-readable streaming")
		}
		runFn = tui.Run
	}

	// ONE shared server-clock estimate covers the whole session: the WS upgrade
	// signing, REST backfill, and the order-entry client all sign through it, so
	// an EXCEED_TIME_WINDOW resync triggered by any surface fixes signing for all
	// at once. The stream session shares it via its Config.Client (built below).
	localNow := cx.Now
	clk := clock.New(localNow)
	syncer := cx.NewClockSyncer(clk, baseURL, timeoutMs, korbit.SurfaceTUI, "clock")

	// Stream-layer loggers, tagged for filtering (see internal/stream/doc.go
	// "Logging"): component=stream for the connection mechanics, component=stream/
	// state for the materialized state store, and a notice logger (kind=stream_notice).
	// Built only when --log-file diverts logs off the alt-screen; nil otherwise so
	// the TUI stays silent on stderr. The TUI has no --stream-log-level, so notices
	// follow --log-level like the mechanics.
	var streamLog, storeLog, noticeLog *slog.Logger
	if tuiLog != nil {
		streamLog = cx.StreamLogger(cx.LogLevel, stream.LogComponentStream)
		storeLog = cx.StreamLogger(cx.LogLevel, stream.LogComponentState)
		noticeLog = cx.NoticeLogger(cx.LogLevel)
	}

	var trader tui.Trader
	var funding tui.Funding
	var fees func(symbol string, accountSeq int) (tui.FeeRates, error)
	var keyName string
	// streamCreds + rec are set for an authenticated session and threaded into the
	// stream's REST client below: the WS upgrade signs with the creds, and the
	// backfill reads consult the trader's recorder (the stream-backfill surface is
	// policy-exempt, so they are consulted but never journaled). A public-only
	// session leaves both nil.
	var streamCreds *korbit.Credentials
	var rec *callrec.Recorder
	// One signing context shared by the TUI's two clients — the order client and
	// the stream-backfill client — each minted via base.as for its own surface,
	// detail, and logger. baseURL/clock/resync are always present; creds/keyName/
	// rec are filled in below only for a private session (a public session leaves
	// them nil, exactly as the stream client carried them before).
	// --time-sync off opts out of the reactive EXCEED_TIME_WINDOW resync on both
	// the order and the stream-backfill client.
	var streamResync func() error
	if cx.Modes.TimeSync.Reactive() {
		streamResync = syncer.Sync
	}
	base := clienv.ClientSpec{BaseURL: baseURL, Clock: clk, Resync: streamResync, TimeoutMs: timeoutMs}
	if private {
		resolved, rerr := km.ResolveSelection(sel, cx.Getenv)
		if rerr != nil {
			return rerr
		}
		signer, perr := resolved.Signer()
		if perr != nil {
			return perr
		}
		keyName = resolved.Name
		apiKeyID := resolved.APIKeyID
		streamCreds = &korbit.Credentials{APIKeyID: apiKeyID, Signer: signer}
		base.Creds, base.KeyName = streamCreds, keyName
		cx.IO.Notef("korbit-cli: signing as key %q", resolved.Name)

		// Pre-flight the key against /v2/currentKeyInfo before the alt-screen
		// opens, so a definitive key/config problem is reported on the terminal —
		// where the user can act on it and re-run — instead of as a cryptic
		// WS-upgrade or subscription rejection that scrolls past inside the
		// full-screen program and is gone the moment it exits. The one signed read
		// serves two purposes: it gates the session start (preflightVerdict), and —
		// when --account-seq was omitted — it supplies the sub-accounts to
		// subscribe (the key's allowedAccountSeqs).
		keyData, keyErr := tuiKeyInfo(cx, base, publicRec, tuiLog)
		// Finalize the subscribe set and the active account now the allowed set is
		// known: an explicit --account-seq list is used verbatim (active on the
		// first); omitted subscribes the whole allowed set, active on the key's
		// configured default — which must itself be allowed, or this errors before
		// the alt-screen opens.
		activeSeq, accountSeqs, err = resolveTUIAccounts(explicitSeqs, keyMeta.DefaultAccountSeq, allowedAccountSeqs(keyData), keyErr)
		if err != nil {
			return err
		}
		// Startup detail (Debug), emitted only when --log-file diverts logs off the
		// alt-screen: which sub-accounts the session subscribed and which is active.
		if tuiLog != nil {
			tuiLog.Debug("tui sub-accounts resolved", "active", activeSeq, "subscribed", accountSeqs)
		}
		// The start gate proper. It blocks only on the conditions that make the
		// WHOLE private session unusable: a deactivated/expired key, an IP that
		// isn't allowlisted, an outright auth rejection, or a session sub-account
		// this key may not access. Per-endpoint permissions (readOrders/writeOrders/
		// …) are deliberately NOT checked here — a missing one still leaves a usable
		// session, and the TUI surfaces it in-band when the user takes the action
		// that needs it. A transport/network failure is non-fatal: the config may be
		// fine, so the session starts and the resilient stream layer surfaces a
		// persistent outage itself.
		if err := preflightVerdict(keyData, keyErr, accountSeqs, cx.Now(), tuiLog); err != nil {
			return err
		}

		// Order actions are journaled like any write command, through the
		// same single-home recorder/policy (internal/callrec) the endpoint
		// commands use. The mint-time intent row is the hard guarantee — ops
		// aborts the placement (nothing on the wire) if it cannot be written. A
		// post-acceptance write failure cannot fail a full-screen program, so it
		// is surfaced as a warning toast (see journalWarning) instead.
		t := &tuiTrader{keyName: keyName, apiKeyID: apiKeyID, jPath: journal.DefaultPath(home)}
		// tui is a Warn surface: a post-call journal failure is surfaced as a toast
		// (t.recErr) — a full-screen program can't die over it — and logged when
		// --log-file routes operational logs to a file.
		rec = cx.NewRecorder(home, tuiLog, func(_ callrec.FailMode, err error) {
			t.recErr = err
			if tuiLog != nil {
				tuiLog.Warn(clienv.FormatJournalWarn(err))
			}
		})
		defer rec.Close()
		base.Rec = rec
		// The order client records each order call itself (the per-call closure binds
		// it to the active ops operation). The stream's backfill reads (open orders,
		// balances, fills) consult this SAME recorder via the stream client below —
		// the policy exempts the stream-backfill surface, so they are consulted but
		// never journaled.
		client := cx.BuildClient(base.As(korbit.SurfaceTUI, "orders", tuiLog))
		t.api = ops.NewAPI(client)
		t.api.RetryBudgetMs = 5000
		t.api.Stderr = tuiLogSink // --log-file when set, else io.Discard (the UI surfaces results itself)
		t.api.Journal = rec       // callrec.Recorder is the ops.OpJournal seam
		trader = t

		// The funding screen runs through the same ops path on its own client
		// (its own log detail tag), sharing the recorder — so a funding write is
		// journaled exactly like `withdraw request` on the command line, and the
		// money movers inherit their single-shot policy from the ops catalog.
		fclient := cx.BuildClient(base.As(korbit.SurfaceTUI, "funding", tuiLog))
		fapi := ops.NewAPI(fclient)
		fapi.RetryBudgetMs = 5000
		fapi.Stderr = tuiLogSink
		fapi.Journal = rec
		funding = &tuiFunding{api: fapi, keyName: keyName, apiKeyID: apiKeyID}

		// The order panel's fee estimates read a sub-account's trading-fee policy
		// (a signed read on the order client's context); the account is passed per
		// call so the estimate follows an in-session account switch.
		fees = tuiFees(cx, base, tuiLog)
	}

	// The stream's one API handle: REST backfill + WS-handshake signing + the
	// shared clock (read & resync), built through the single client site. Its Creds
	// (set for a private session) sign the upgrade; its recorder consults the
	// journaling policy, which exempts the stream-backfill surface (recovery reads,
	// not actions, so consulted but never journaled). A public session is creds-less.
	streamClient := cx.BuildClient(base.As(korbit.SurfaceStreamBackfill, "", streamLog))

	// The private channels now carry the finalized sub-account set (every
	// subscribed account); public channels ignore it.
	subs := tuiSubscriptions(symbols, symbols[0], private, accountSeqs)
	sess, err := stream.New(stream.Config{
		PublicURL:         publicURL,
		PrivateURL:        privateURL,
		Subscriptions:     subs,
		Client:            streamClient,
		ProactiveTimeSync: cx.Modes.TimeSync.Proactive(),
		Dial:              cx.WSDial,
		UserAgent:         useragent.For(korbit.SurfaceStreamWS, ""),
		Now:               cx.Now,
		Sleep:             cx.Sleep,
		Log:               streamLog,
		// The account channels cover every watched symbol; snapshot-only backfill
		// keeps that from fanning per-symbol order/fill history out over all of
		// them on every reconnect (open orders + balances still re-backfill).
		BackfillSnapshotOnly: true,
		// Lazy open-order snapshots: the myOrder channel stays subscribed to every
		// pair (live updates are account-wide), but the authoritative
		// GET /v2/openOrders snapshot is taken only for the symbols the user is
		// viewing (the active pair by default), backfilled on demand as the focus
		// moves — so launching against hundreds of pairs doesn't fan a per-symbol
		// snapshot out over all of them. The TUI drives the tracked set.
		LazyOpenOrders: private,
	})
	if err != nil {
		return output.Usagef("%v", err)
	}

	// Seed the lazy open-order tracking with the initial active pair under the
	// active account, so the first connect's backfill snapshots it (before Run
	// this only sets the tracked set; a no-op in public mode). The TUI re-points
	// it on switches and the toggle.
	if private {
		sess.SetTrackedOrderScopes([]stream.OrderScope{{AccountSeq: activeSeq, Symbol: symbols[0]}})
	}

	// Pin the order-size preset levels: the built-in defaults are written to
	// config.json on first launch and are authoritative thereafter, so a later
	// program update never silently changes what a size-preset key does. A stored
	// value is already validated by config load (a bad edit fails early), so a
	// non-empty value here needs no re-check. Best-effort: a write failure just
	// means the defaults aren't pinned yet (retried next launch).
	orderLevels := cfg.TUIOrderLevels
	if len(orderLevels) == 0 {
		orderLevels = tui.DefaultOrderLevels()
		if serr := config.SetTUIOrderLevels(home, orderLevels, tuiLog); serr != nil && tuiLog != nil {
			tuiLog.Warn("could not pin tui order levels", "err", serr)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessErr := make(chan error, 1)
	go func() { sessErr <- sess.Run(ctx) }()

	tuiErr := runFn(tui.Config{
		Symbols:     symbols,
		Private:     private,
		AccountSeq:  activeSeq,
		AccountSeqs: accountSeqs,
		Trader:      trader,
		Funding:     funding,
		KeyName:     keyName,
		BaseURL:     baseURL,
		OrderLevels: orderLevels,
		ColorScheme: cfg.TUIColorScheme,
		SaveColorScheme: func(scheme string) error {
			// Called once on exit. A failure is cosmetic (the scheme was already
			// applied for the session), so log it and let the TUI exit cleanly.
			if serr := config.SetTUIColorScheme(home, scheme, tuiLog); serr != nil {
				if tuiLog != nil {
					tuiLog.Warn("could not persist tui color scheme", "err", serr)
				}
				return serr
			}
			return nil
		},
		Events:            sess.Events(),
		StopSession:       cancel,
		SetActiveMarket:   tuiActiveMarket(sess),
		SetOrderbookLevel: tuiOrderbookLevel(sess),
		SetTrackedOrders:  sess.SetTrackedOrderScopes,
		Candles:           tuiCandles(cx, baseURL, timeoutMs, publicRec),
		TickSizePolicy:    tuiTickSizePolicy(cx, baseURL, timeoutMs, publicRec),
		Fees:              fees,
		Out:               cx.IO.Out,
		In:                os.Stdin,
		Now:               cx.Now,
		StoreLog:          storeLog,
		NoticeLog:         noticeLog,
	})
	cancel()
	err = <-sessErr // nil when the stop was deliberate (canceled)
	if tuiErr != nil {
		return tuiErr
	}
	if err != nil {
		// The session died underneath the TUI (the TUI quit when the event
		// stream closed). Same classification as monitor: a rejected upgrade
		// with a Korbit envelope is an API error (exit 3).
		return monitorcmd.RunError(err)
	}
	return nil
}

// tuiLogging returns the TUI's operational logger and its writer sink. The TUI
// owns the full-screen alt-screen, so stderr is unusable for diagnostics: it
// logs only when --log-file (or KORBIT_CLI_LOG_FILE) diverts logs to a file.
// With a file open it returns the centralized logger (cx.Log, writing to the
// file) and that file sink; otherwise (nil, io.Discard), so the TUI stays silent
// on stderr and surfaces failures as on-screen toasts.
func tuiLogging(cx *clienv.Cmd) (*slog.Logger, io.Writer) {
	if !cx.LogToFile {
		return nil, io.Discard
	}
	return cx.Log, cx.LogSink
}

// tuiCandles builds the candle-chart fetch seam (tui.Config.Candles): a public
// read through the candles operation (auto-pages past the server's per-request
// cap), available in both public and private mode. The operation is run on its
// own creds-less ops.API — candles need no signing or clock — and records
// through the shared public-read recorder (journaled only in --debug, per
// DefaultPolicy).
func tuiCandles(cx *clienv.Cmd, baseURL string, timeoutMs int, rec *callrec.Recorder) func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
	tuiLog, logSink := tuiLogging(cx)
	// Public read (candles need no signing or clock) — built through the single
	// client site for a uniform logger/UA/Origin and the one journaling closure.
	client := cx.BuildClient(clienv.ClientSpec{
		Surface:   korbit.SurfaceTUI,
		Detail:    "candles",
		BaseURL:   baseURL,
		TimeoutMs: timeoutMs,
		Rec:       rec,
		Log:       tuiLog,
	})
	// candles run through op.Run, so the operations-ledger seam (Journal, not the
	// adhoc closure) is what records them — journaled only in --debug, per
	// DefaultPolicy. A creds-less client → Resync stays nil (candles never resync).
	api := ops.NewAPI(client)
	api.RetryBudgetMs = 5000
	api.Stderr = logSink // --log-file when set, else io.Discard (the UI surfaces errors itself)
	api.Journal = rec
	op := ops.Find("candles")
	return func(symbol, interval string, limit int, endMs int64) ([]candles.Bar, error) {
		// symbol is a positional on the candles op (not a Param), so validateParams
		// would drop it; normalize and inject it the way the cli's positional path
		// does, or the request goes out with an empty symbol.
		values := map[string]string{
			"interval": interval, "limit": strconv.Itoa(limit),
		}
		// endMs > 0 pages back into history (scroll-back backfill); 0 omits the
		// bound so the endpoint returns up to the current still-open bucket.
		if endMs > 0 {
			values["endTime"] = strconv.FormatInt(endMs, 10)
		}
		params, err := validateParams(op, values)
		if err != nil {
			return nil, err
		}
		sym, err := cmdmeta.NormalizeValue(cmdmeta.Param{Kind: cmdmeta.KindSymbol}, symbol, "<symbol>")
		if err != nil {
			return nil, err
		}
		params["symbol"] = sym
		res, err := op.Run(context.Background(), api, ops.RunInput{
			Values:   params,
			Controls: ops.Controls{Surface: korbit.SurfaceTUI},
		})
		if err != nil {
			return nil, err
		}
		var bars []candles.Bar // carries the REST row's json tags
		if err := json.Unmarshal(res.Data, &bars); err != nil {
			return nil, fmt.Errorf("candles response: %w", err)
		}
		return bars, nil
	}
}

// tuiTickSizePolicy builds the order panel's tick-metadata fetch seam
// (tui.Config.TickSizePolicy): a public read of GET /v2/tickSizePolicy —
// the tick-size bands plus the valid orderbook grouping levels — recorded
// through the shared public-read recorder (journaled only in --debug, per
// DefaultPolicy). The panel caches per symbol and treats a failure as "no
// grid" — best-effort by design.
func tuiTickSizePolicy(cx *clienv.Cmd, baseURL string, timeoutMs int, rec *callrec.Recorder) func(symbol string) (tui.TickPolicy, error) {
	tuiLog, _ := tuiLogging(cx)
	client := cx.BuildClient(clienv.ClientSpec{
		Surface:   korbit.SurfaceTUI,
		Detail:    "ticksize",
		BaseURL:   baseURL,
		TimeoutMs: timeoutMs,
		Rec:       rec,
		Log:       tuiLog,
	})
	raw := rawapi.New(client, tuiLog)
	return func(symbol string) (tui.TickPolicy, error) {
		pols, _, _, err := raw.TickSize(context.Background(), rawapi.TickSizeRequest{Symbol: rawapi.Symbol(symbol)}, korbit.Policy{Idempotent: true})
		if err != nil {
			return tui.TickPolicy{}, err
		}
		if len(pols) == 0 {
			return tui.TickPolicy{}, fmt.Errorf("no tick-size policy returned for %s", symbol)
		}
		p := tui.TickPolicy{
			Bands:  make([]ops.TickBand, 0, len(pols[0].TickSizePolicy)),
			Levels: pols[0].OrderbookLevels,
		}
		for _, b := range pols[0].TickSizePolicy {
			p.Bands = append(p.Bands, ops.TickBand{PriceGte: b.PriceGte, TickSize: b.TickSize})
		}
		return p, nil
	}
}

// tuiFees builds the order panel's fee-rate fetch seam (tui.Config.Fees): a
// signed read of a sub-account's trading-fee policy on its own client context.
// The account is passed per call (not captured), so the estimate follows the
// active account when the user switches. Read-only and idempotent; a failure
// clears the TUI's fetch guard so the next trigger retries — until a policy
// lands, arming an order stays gated (see tui.Config.Fees).
func tuiFees(cx *clienv.Cmd, base clienv.ClientSpec, tuiLog *slog.Logger) func(symbol string, accountSeq int) (tui.FeeRates, error) {
	client := cx.BuildClient(base.As(korbit.SurfaceTUI, "fees", tuiLog))
	raw := rawapi.New(client, tuiLog)
	return func(symbol string, accountSeq int) (tui.FeeRates, error) {
		rows, _, _, err := raw.Fees(context.Background(), rawapi.FeesRequest{Symbol: &symbol, AccountSeq: &accountSeq}, korbit.Policy{Idempotent: true})
		if err != nil {
			return tui.FeeRates{}, err
		}
		for _, f := range rows {
			if f.Symbol == symbol {
				return tui.FeeRates{
					MakerRate: f.MakerFeeRate, TakerRate: f.TakerFeeRate, MaxRate: f.MaxFeeRate,
					BuyFeeCurrency: f.BuyFeeCurrency, SellFeeCurrency: f.SellFeeCurrency,
				}, nil
			}
		}
		return tui.FeeRates{}, fmt.Errorf("no fee policy returned for %s", symbol)
	}
}

// launchedPairs lists every currently launched trading pair from the public
// GET /v2/currencyPairs, sorted. It is the default symbol set when the TUI is
// started without --symbols. Public read (no signing); the L1 client's
// TimeoutMs bounds it. It records through the shared public-read recorder
// (journaled only in --debug, per DefaultPolicy).
func launchedPairs(cx *clienv.Cmd, ctx context.Context, baseURL string, timeoutMs int, rec *callrec.Recorder) ([]string, error) {
	tuiLog, _ := tuiLogging(cx)
	client := cx.BuildClient(clienv.ClientSpec{
		Surface:   korbit.SurfaceTUI,
		Detail:    "pairs",
		BaseURL:   baseURL,
		TimeoutMs: timeoutMs,
		Rec:       rec,
		Log:       tuiLog,
	})
	pairs, _, _, err := rawapi.New(client, tuiLog).Pairs(ctx, rawapi.PairsRequest{}, korbit.Policy{})
	if err != nil {
		return nil, err
	}
	symbols := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.Status == "launched" {
			symbols = append(symbols, p.Symbol)
		}
	}
	sort.Strings(symbols)
	return symbols, nil
}

// keyStatusActivated is the only /v2/currentKeyInfo status a usable key reports;
// anything else (e.g. "deactivated") cannot trade or subscribe.
const keyStatusActivated = "activated"

// tuiKeyInfo signs one GET /v2/currentKeyInfo on the session's signing context
// and returns the raw key-info payload plus the call error, before the TUI
// enters the alt-screen (so any resulting error is printed on the live
// terminal). It feeds both the start gate (preflightVerdict) and the omitted
// --account-seq default (allowedAccountSeqs). RetryPreExec lets a skewed clock
// self-correct once (EXCEED_TIME_WINDOW) against the same estimate the WS upgrade
// will sign with. The read is a diagnostic, journaled only under --debug like the
// TUI's other reads (it shares the public-read recorder).
func tuiKeyInfo(cx *clienv.Cmd, base clienv.ClientSpec, rec *callrec.Recorder, log *slog.Logger) (json.RawMessage, error) {
	spec := base.As(korbit.SurfaceTUI, "preflight", log)
	spec.Rec = rec
	client := cx.BuildClient(spec)
	_, data, _, err := rawapi.New(client, log).Whoami(context.Background(), rawapi.WhoamiRequest{}, korbit.Policy{RetryPreExec: true})
	return data, err
}

// allowedAccountSeqs reads the key's accessible sub-accounts from a
// /v2/currentKeyInfo payload. It returns nil when the field is absent (an older
// server, or a failed fetch that left no data) — distinct from a present but
// empty list, which preflightVerdict treats as "no accessible sub-account".
func allowedAccountSeqs(data json.RawMessage) []int {
	var info struct {
		AllowedAccountSeqs *[]int `json:"allowedAccountSeqs"`
	}
	_ = json.Unmarshal(data, &info)
	if info.AllowedAccountSeqs == nil {
		return nil
	}
	return *info.AllowedAccountSeqs
}

// resolveTUIAccounts decides the session's subscribed sub-accounts and the one
// it starts active on, from the --account-seq selection (explicit; nil when
// omitted), the key's configured default, the key's allowedAccountSeqs (nil when
// the key-info fetch failed or the server didn't report them), and that fetch's
// error.
//
//   - An explicit list is used verbatim, active on the first element;
//     preflightVerdict separately enforces every element is allowed.
//   - Omitted subscribes the whole allowed set, active on the key's configured
//     default (else 1/main). That default must itself be one of the allowed
//     accounts — otherwise the session would start active on a sub-account it
//     cannot subscribe, so this is a ConfigError with the fix.
//   - When the allowed set is unknown, the omission cannot be honored, so a
//     TRANSIENT key-info failure is fatal here (the set is unknowable and
//     narrowing to one account would silently hide the key's others). A
//     ClassFatal failure or a server that simply didn't report the field falls
//     back to the single active account — preflightVerdict then reports the
//     definitive cause, or the resilient stream layer surfaces a subscription
//     rejection itself.
func resolveTUIAccounts(explicit []int, keyDefault string, allowed []int, keyInfoErr error) (active int, subscribed []int, err error) {
	if len(explicit) > 0 {
		return explicit[0], explicit, nil
	}
	active, err = accountseq.Resolve("", accountseq.Inputs{Default: keyDefault, Param: accountseq.Param(), Label: "--account-seq"})
	if err != nil {
		return 0, nil, err
	}
	if len(allowed) == 0 {
		// The allowed set is unknown: the key-info read failed, or the server
		// didn't report the field (older server), or it reported an empty list.
		if keyInfoErr != nil && korbit.Classify(keyInfoErr) != korbit.ClassFatal {
			// Omitted --account-seq means "subscribe every account this key can
			// access", but the read that lists them failed transiently, so the set
			// is unknown. Silently narrowing to the single active account would hide
			// the key's other accounts in a money UI — refuse instead, preserving the
			// failure's classification (exit 1/3, like launchedPairs above). A
			// DEFINITIVE (ClassFatal) rejection falls through to the single-account
			// fallback so preflightVerdict reports its specific cause (deactivated
			// key, IP allowlist, …) on the terminal.
			return 0, nil, omittedAccountSeqUnresolvable(keyInfoErr)
		}
		// A reported empty list is caught by preflightVerdict (active not in []); a
		// ClassFatal fetch error is reported there too. The single active account
		// keeps the session startable otherwise.
		return active, []int{active}, nil
	}
	if !containsInt(allowed, active) {
		return 0, nil, output.Configf(
			"this key's default sub-account %d is not in its allowed accounts %v — set an allowed default with `%s key set-default-account-seq`, or pass --account-seq explicitly",
			active, allowed, progname.Name())
	}
	// Subscribe the whole allowed set, ascending so the switcher lists it
	// predictably (main first). The active default is one of them.
	subscribed = append([]int(nil), allowed...)
	sort.Ints(subscribed)
	return active, subscribed, nil
}

// omittedAccountSeqUnresolvable is the error refusing an omitted-account-seq
// session whose account set could not be read (a transient key-info failure): the
// omission's "every account" cannot be honored without that read. It preserves
// the underlying failure's classification/exit code — attaching the way-forward
// as Guidance on an ApiError (which the emitter renders verbatim, exit 3) or
// wrapping a transport error (exit 1) — so the user sees both the cause and what
// to do, rather than a session that silently narrowed to one account.
func omittedAccountSeqUnresolvable(keyInfoErr error) error {
	const advice = "could not determine which sub-accounts this key can access — retry, or pass --account-seq to start on specific sub-account(s)"
	var api *output.ApiError
	if errors.As(keyInfoErr, &api) {
		cp := *api
		if cp.Guidance == "" {
			cp.Guidance = advice
		} else {
			cp.Guidance += " — " + advice
		}
		return &cp
	}
	return fmt.Errorf("%s (%w)", advice, keyInfoErr)
}

// containsInt reports whether xs contains v.
func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// preflightVerdict decides whether the private TUI session can start from a
// /v2/currentKeyInfo result (the raw unwrapped data plus the call error). Only a
// DEFINITIVE rejection blocks the start: korbit.ClassFatal (a 4xx carrying a
// Korbit envelope code — auth/permission/config — the same class the WS upgrade
// treats as fatal), or an unusable key/account in the returned payload. Both are
// a ConfigError (exit 4) carrying the fix. A transient failure (network, HTTP
// 5xx/429) and an unconverged clock resync (EXCEED_TIME_WINDOW) are NON-fatal
// (nil): the config may be fine, so the session starts and the resilient stream
// layer recovers the outage / resyncs the clock itself — doctor's "could not
// verify" stance, and never STRICTER than the upgrade it precedes. nowMs is the
// SYSTEM clock: the expiry comparison is day-scale, so sub-second skew is
// irrelevant.
func preflightVerdict(data json.RawMessage, err error, accountSeqs []int, nowMs int64, log *slog.Logger) error {
	if err != nil {
		if korbit.Classify(err) != korbit.ClassFatal {
			// Transient (network / 5xx / 429) or a clock resync that didn't
			// converge (EXCEED_TIME_WINDOW): not a definitive key/config problem.
			// Start anyway; the stream layer recovers or resyncs itself.
			if log != nil {
				log.Warn("tui preflight: could not verify key before start — starting anyway", "err", err.Error())
			}
			return nil
		}
		var apiErr *output.ApiError
		if errors.As(err, &apiErr) && probe.IsIPAllowlistCode(apiErr.Code) {
			return output.Configf(
				"this API key is not allowlisted for your current IP address — add your IP to the key's allowlist in the developers portal, or run `%s doctor` to see the address Korbit sees (%s)",
				progname.Name(), apiErr.Code)
		}
		detail := err.Error()
		if errors.As(err, &apiErr) && apiErr.Code != "" {
			detail = apiErr.Code
		}
		return output.Configf(
			"the API rejected the signed key check (%s) — verify the key in the developers portal (status, expiry, IP allowlist), then re-run", detail)
	}
	// The signed read succeeded — inspect the payload for a key/account that
	// cannot run a private session. The optional fields are pointers so an absent
	// field is skipped (defensive), while an explicit empty list is honored:
	// allowedAccountSeqs:[] means no accessible sub-account, which doctor also
	// treats as a problem.
	var info struct {
		Status             string `json:"status"`
		Expiration         *int64 `json:"expiration"`
		AllowedAccountSeqs *[]int `json:"allowedAccountSeqs"`
	}
	_ = json.Unmarshal(data, &info)
	if info.Status != "" && info.Status != keyStatusActivated {
		return output.Configf("this API key is %q (not activated) — reactivate it in the developers portal", info.Status)
	}
	if info.Expiration != nil && *info.Expiration <= nowMs {
		return output.Configf("this API key has expired — re-issue it in the developers portal")
	}
	// EVERY session sub-account must be one the key may access, or the private
	// channel subscribes are rejected once the program is already full-screen.
	if info.AllowedAccountSeqs != nil {
		allowed := make(map[int]bool, len(*info.AllowedAccountSeqs))
		for _, s := range *info.AllowedAccountSeqs {
			allowed[s] = true
		}
		var missing []int
		for _, seq := range accountSeqs {
			if !allowed[seq] {
				missing = append(missing, seq)
			}
		}
		if len(missing) > 0 {
			return output.Configf(
				"sub-account(s) %v are not in this key's allowed accounts %v — pass allowed --account-seq value(s), or update the key's allowed accounts in the developers portal",
				missing, *info.AllowedAccountSeqs)
		}
	}
	return nil
}

// tuiSubscriptions maps the symbol list onto the channel set: all three
// tuiTradeHistory is the recent-trade depth the TUI seeds its trades panel
// with from REST on the first subscribe (see Subscription.TradeHistory).
const tuiTradeHistory = 50

// channel set, plus the account channels when private. Ticker covers EVERY
// watched symbol (it powers the sidebar, which lists them all), but the heavier
// orderbook/trade channels cover only the initial ACTIVE symbol — the TUI moves
// those to the new active symbol on a switch via the stream session's dynamic
// Subscribe/Unsubscribe (the SetActiveMarket seam). The account channels cover
// every symbol so the orders/fills panels are complete; the session runs in
// snapshot-only backfill mode so that breadth doesn't fan REST history out over
// every pair on reconnect.
func tuiSubscriptions(symbols []string, active string, private bool, accountSeqs []int) []stream.Subscription {
	subs := []stream.Subscription{
		{Channel: stream.ChannelTicker, Symbols: symbols},
		{Channel: stream.ChannelOrderbook, Symbols: []string{active}},
		// Seed the trades panel from REST on first subscribe: the live WS
		// snapshot's row count is server-defined (possibly a single trade), so
		// without this the panel starts nearly empty. Best-effort depth, emitted
		// as backfill. The depth is read from the live subscription registry, so
		// it applies to a symbol's FIRST activation; a later switch-back reuses
		// the retained tradeId high-water mark and gap-patches the hole instead.
		{Channel: stream.ChannelTrade, Symbols: []string{active}, TradeHistory: tuiTradeHistory},
	}
	if private {
		// Pin every private channel to the session's sub-accounts. The live
		// account channels are subscribed for ALL of them up front — frames are
		// tagged per account and the volume is the user's own activity — while
		// the per-account REST cost stays scoped: balances re-backfill per seq
		// (one cheap call each), and open-order snapshots follow the tracked
		// scope set (LazyOpenOrders), not this list.
		// Defensive: seqs are always >= 1 from accountseq.Resolve; the guard
		// only fires if tuiSubscriptions is called directly with 0 (tests).
		seqs := make([]int, 0, len(accountSeqs))
		for _, seq := range accountSeqs {
			if seq < 1 {
				seq = accountseq.Main
			}
			seqs = append(seqs, seq)
		}
		subs = append(subs,
			stream.Subscription{Channel: stream.ChannelMyOrder, Symbols: symbols, AccountSeqs: seqs},
			stream.Subscription{Channel: stream.ChannelMyTrade, Symbols: symbols, AccountSeqs: seqs},
			stream.Subscription{Channel: stream.ChannelMyAsset, AccountSeqs: seqs},
		)
	}
	return subs
}

// tuiActiveMarket re-subscribes the active symbol's orderbook + trades when the
// user switches in the sidebar: subscribe the next symbol first (so there is
// never a window with no active book), then drop the previous one. The
// orderbook subscription carries the next symbol's grouping level ("" = the
// raw book), so a switch-back restores the pair's grouping. The trade
// subscription requests the same REST seed depth; it fills the panel on a
// symbol's first activation, while a switch-back gap-patches from the retained
// high-water mark. Best-effort — Subscribe/Unsubscribe only error on a validation or
// no-public-connection condition that can't arise here (valid symbols, ticker
// always holds the public connection open).
func tuiActiveMarket(sess *stream.Session) func(prev, next, nextLevel string) {
	return func(prev, next, nextLevel string) {
		_ = sess.Subscribe(stream.Subscription{Channel: stream.ChannelOrderbook, Symbols: []string{next}, Level: nextLevel})
		_ = sess.Subscribe(stream.Subscription{Channel: stream.ChannelTrade, Symbols: []string{next}, TradeHistory: tuiTradeHistory})
		_ = sess.Unsubscribe(stream.ChannelOrderbook, []string{prev})
		_ = sess.Unsubscribe(stream.ChannelTrade, []string{prev})
	}
}

// tuiOrderbookLevel re-points one symbol's orderbook subscription at a
// grouping level ("" = the raw book), leaving the trade channel alone. The
// wire order is unsubscribe-then-subscribe: both the session's registry and
// the server match an unsubscribe by channel+symbol regardless of level, so
// subscribing the new level first would be undone by the old level's
// unsubscribe. The gap this opens is display-gated by the TUI (the book pane
// shows "loading…" until the new subscription's snapshot lands).
func tuiOrderbookLevel(sess *stream.Session) func(symbol, level string) {
	return func(symbol, level string) {
		_ = sess.Unsubscribe(stream.ChannelOrderbook, []string{symbol})
		_ = sess.Subscribe(stream.Subscription{Channel: stream.ChannelOrderbook, Symbols: []string{symbol}, Level: level})
	}
}

// writerIsTerminal reports whether the writer is an interactive terminal.
func writerIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// tuiTrader is the tui.Trader implementation: the order panel's place and
// the cancel action, run through the exact same machinery as `order place`
// and `order cancel` — spec-driven param validation, the cross-field sizing
// matrix, clientOrderId minting, the action journal's hard guarantee, and the
// L2 ops layer's idempotency-gated retry (place runs the clientOrderId
// reconcile protocol; cancel is idempotent).
type tuiTrader struct {
	api      *ops.API
	jPath    string // the action-journal path, for the warning toast
	keyName  string
	apiKeyID string
	// recErr captures a post-call api_calls journal-write failure during a
	// single (serial) order action: order actions are driven one at a time from
	// the model update loop, so a plain field is safe. It is set by the recorder's
	// onPostFailure sink (the tui surface is Warn), reset before each action, and
	// folded with the operation's post-result ops.Result.JournalErr by journalErr —
	// a full-screen UI can't print-result-and-exit, so a post-acceptance failure
	// becomes a toast, not a non-zero exit.
	recErr error
}

// validateParams normalizes the supplied values against an operation's params
// (the same per-value rules the CLI flags apply), keyed by wire (API) name. A
// missing required param is a usage error; an empty optional value is dropped.
// The ops Operation builds its own typed wire request from this map.
func validateParams(op ops.Operation, values map[string]string) (map[string]string, error) {
	m := map[string]string{}
	for _, p := range op.Meta().Params {
		raw, ok := values[p.API]
		if !ok || raw == "" {
			if p.Required {
				return nil, output.Usagef("--%s is required", p.Flag)
			}
			continue
		}
		v, err := cmdmeta.NormalizeValue(p, raw, "--"+p.Flag)
		if err != nil {
			return nil, err
		}
		m[p.API] = v
	}
	return m, nil
}

func (t *tuiTrader) Place(form tui.OrderForm) (tui.PlaceResult, error) {
	// The sub-account arrives on the form, stamped by the TUI's single dispatch
	// point — never defaulted here (a zero value is a TUI bug, not "main").
	if form.AccountSeq < 1 {
		return tui.PlaceResult{}, output.Usagef("order form carried no sub-account (internal error)")
	}
	op := ops.Find("order", "place")
	values := map[string]string{
		"symbol": form.Symbol, "side": form.Side, "orderType": form.Type,
		"price": form.Price, "qty": form.Qty, "amt": form.Amt, "timeInForce": form.TIF,
		"accountSeq": strconv.Itoa(form.AccountSeq),
	}
	if form.PP {
		values["pp"] = "true"
	}
	params, err := validateParams(op, values)
	if err != nil {
		return tui.PlaceResult{}, err
	}
	if cv := op.Meta().CrossValidate; cv != nil {
		if err := cv(params); err != nil {
			return tui.PlaceResult{}, err
		}
	}
	// Idempotency by default, exactly like the command path: every placement
	// carries a clientOrderId so a resend inside the place reconcile reuses the
	// same id. The TUI model mints it at dispatch (it keys the order's local
	// balance hold on it — see tui.OrderForm.ClientOrderID), so use the form's;
	// mint here only for a caller that didn't. Either way the id is known
	// client-side, so it can be reported even when the response omits it.
	clientOrderID := form.ClientOrderID
	if clientOrderID == "" {
		clientOrderID = ops.MintClientOrderID()
	}
	params["clientOrderId"] = clientOrderID

	// The place Operation writes the mint-time intent row (hard guarantee) and
	// runs the reconcile protocol; a StartOrder failure comes back as a returned
	// error with nothing sent.
	t.recErr = nil
	res, err := op.Run(context.Background(), t.api, ops.RunInput{
		Values:   params,
		Controls: ops.Controls{Surface: korbit.SurfaceTUI},
		KeyName:  t.keyName,
		APIKeyID: t.apiKeyID,
	})
	if err != nil {
		// The order was rejected (or could not be journaled before anything was
		// sent). If a journal write ALSO failed, surface both — a full-screen
		// program can't print a stderr warning the way runEndpoint does.
		return tui.PlaceResult{}, withJournalFailure(err, t.jPath, t.journalErr(res))
	}
	out := tui.PlaceResult{OrderID: topLevelField(res.Data, "orderId"), ClientOrderID: clientOrderID}
	if w := t.journalWarning(res); w != "" {
		// The order IS live; surface the incomplete-history failure as a toast.
		out.Warning = w
	}
	return out, nil
}

func (t *tuiTrader) Cancel(symbol string, orderID int64, accountSeq int) (string, error) {
	// The order's own sub-account, passed explicitly per call (see tui.Trader).
	if accountSeq < 1 {
		return "", output.Usagef("cancel carried no sub-account (internal error)")
	}
	op := ops.Find("order", "cancel")
	params, err := validateParams(op, map[string]string{
		"symbol": symbol, "orderId": fmt.Sprintf("%d", orderID),
		"accountSeq": strconv.Itoa(accountSeq),
	})
	if err != nil {
		return "", err
	}
	if cv := op.Meta().CrossValidate; cv != nil {
		if err := cv(params); err != nil {
			return "", err
		}
	}
	// Cancel is idempotent: the cancel Operation derives the retry/idempotency
	// gate from its OpMeta.Safety.
	t.recErr = nil
	res, err := op.Run(context.Background(), t.api, ops.RunInput{
		Values:   params,
		Controls: ops.Controls{Surface: korbit.SurfaceTUI},
		KeyName:  t.keyName,
		APIKeyID: t.apiKeyID,
	})
	if err != nil {
		return "", withJournalFailure(err, t.jPath, t.journalErr(res))
	}
	return t.journalWarning(res), nil
}

// journalErr returns the out-of-band journal write failure for the last order
// action, preferring the operation's post-result ledger/order-row finish error
// (ops.Result.JournalErr) over the api_calls record error captured in recErr.
// nil when journaling succeeded (or is off).
func (t *tuiTrader) journalErr(res ops.Result) error {
	if res.JournalErr != nil {
		return res.JournalErr
	}
	return t.recErr
}

// journalWarning folds any out-of-band journal write failure from the last
// action into one user-facing message. The action already succeeded on the
// exchange; only the local action history is incomplete.
func (t *tuiTrader) journalWarning(res ops.Result) string {
	err := t.journalErr(res)
	if err == nil {
		return ""
	}
	return fmt.Sprintf("recording it to the action journal at %s failed: %v — your local action history is now incomplete", t.jPath, err)
}

// withJournalFailure folds a journal-write failure into a call error so the TUI
// surfaces both. It flattens to a plain error whose message carries the API
// code+message (output.ApiError.Error() is only the message) plus the journal
// note; when there is no journal failure the original error (and its type, for
// any downstream classification) is returned unchanged.
func withJournalFailure(callErr error, jPath string, logErr error) error {
	if logErr == nil {
		return callErr
	}
	msg := callErr.Error()
	var ae *output.ApiError
	if errors.As(callErr, &ae) && ae.Code != "" {
		msg = ae.Code + " — " + ae.Message
	}
	return fmt.Errorf("%s — also failed to record it to the action journal at %s: %v (local action history is now incomplete)", msg, jPath, logErr)
}

// topLevelField reads a top-level field from a JSON object as a string: a
// string value is unquoted, any other scalar (e.g. a number) is its verbatim
// JSON text. It mirrors ops' internal extractor for reading orderId out of a
// placed order.
func topLevelField(raw json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}
