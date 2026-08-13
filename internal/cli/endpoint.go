// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/callrec"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
	"github.com/korbit-official/korbit-cli/internal/useragent"
	"github.com/spf13/cobra"
)

// cmdPlace is the one command key the cli's imperative logic keys off by string:
// the clientOrderId idempotency/echo and the order-journal row are gated on it.
const cmdPlace = "order place"

// collectParams maps positionals and flags to ordered API params (for signing
// and sending, in declaration order) plus a map (for cross-field validation and
// lookups). The two stay in lock-step. Values are normalized via the cmdmeta
// validation engine, the single home of the per-value rules.
func collectParams(sc surfaceCmd, cmd *cobra.Command, args []string) ([]korbit.KV, map[string]string, error) {
	prog := progname.Name()
	var ordered []korbit.KV
	m := map[string]string{}
	add := func(api, val string) {
		ordered = append(ordered, korbit.KV{Key: api, Value: val})
		m[api] = val
	}

	idx := 0
	for _, ps := range sc.Positionals {
		label := "<" + ps.Name + ">"
		pspec := cmdmeta.Param{Kind: ps.Kind}
		if ps.Variadic {
			vals := args[idx:]
			idx = len(args)
			if len(vals) > 0 {
				v, err := cmdmeta.NormalizeValue(pspec, strings.Join(vals, ","), label)
				if err != nil {
					return nil, nil, err
				}
				add(ps.API, v)
			} else if ps.Required {
				return nil, nil, output.Usagef("%s is required — see `%s %s --help`", label, prog, sc.Key())
			}
			continue
		}
		if idx < len(args) {
			v, err := cmdmeta.NormalizeValue(pspec, args[idx], label)
			if err != nil {
				return nil, nil, err
			}
			idx++
			add(ps.API, v)
		} else if ps.Required {
			return nil, nil, output.Usagef("%s is required — see `%s %s --help`", label, prog, sc.Key())
		}
	}
	if idx < len(args) {
		return nil, nil, output.Usagef("unexpected argument %q — see `%s %s --help`", args[idx], prog, sc.Key())
	}

	for _, p := range sc.Params {
		label := "--" + p.Flag
		if !cmd.Flags().Changed(p.Flag) {
			if p.Default != "" {
				v, err := cmdmeta.NormalizeValue(p, p.Default, label)
				if err != nil {
					return nil, nil, err
				}
				add(p.API, v)
			} else if p.Required {
				return nil, nil, output.Usagef("%s is required — see `%s %s --help`", label, prog, sc.Key())
			}
			continue
		}
		raw := "true"
		if p.Kind != cmdmeta.KindFlag {
			raw, _ = cmd.Flags().GetString(p.Flag)
		}
		if _, dup := m[p.API]; dup {
			return nil, nil, output.Usagef("%q was given both as a positional argument and via %s", p.API, label)
		}
		v, err := cmdmeta.NormalizeValue(p, raw, label)
		if err != nil {
			return nil, nil, err
		}
		add(p.API, v)
	}
	return ordered, m, nil
}

func (rt *runtime) runEndpoint(sc surfaceCmd, cmd *cobra.Command, args []string) error {
	commandKey := sc.Key()
	needsAuth := sc.Auth != nil
	isPlace := commandKey == cmdPlace

	ordered, params, err := collectParams(sc, cmd, args)
	if err != nil {
		return err
	}

	// Idempotency by default: a real order placement always carries a
	// clientOrderId. A --dry-run does NOT mint one, though: the minted id would be
	// thrown away (a real send mints a fresh one), so echoing it in the plan would
	// only mislead a reader into thinking that exact id will be used. An
	// explicitly supplied --client-order-id is already in params and still shows.
	if isPlace && !rt.dryRun {
		if _, ok := params["clientOrderId"]; !ok {
			id := ops.MintClientOrderID()
			ordered = append(ordered, korbit.KV{Key: "clientOrderId", Value: id})
			params["clientOrderId"] = id
		}
	}

	home, cfg, err := rt.LoadConfig()
	if err != nil {
		return err
	}

	// Resolve which key is in play (the single key-selection front door:
	// --key / KORBIT_CLI_KEY else the default, OR inline KORBIT_CLI_API_KEY_*
	// material — mutually exclusive, enforced here so a contradictory invocation
	// fails before any work) and peek its stored metadata — endpoint override plus
	// defaultAccountSeq, no secret access. The key's host is honored for every
	// command, signed or not: a key pinned to a non-prod host (e.g. a sandbox)
	// routes its public market-data calls there too, never silently to prod. An
	// inline credential has no stored metadata, so the per-key tiers do not apply.
	sel, err := keys.Select(rt.key, rt.deps.Getenv)
	if err != nil {
		return err
	}
	km := rt.KeyManager(home, cfg)
	meta := km.MetaForSelection(sel)

	// Resolve accountSeq after the key is known so the per-key default applies.
	_, hadAccountSeq := params[accountseq.APIName]
	appliedAccountSeq, err := accountseq.Ensure(sc.Params, params, meta.DefaultAccountSeq)
	if err != nil {
		return err
	}
	if appliedAccountSeq && !hadAccountSeq {
		ordered = append(ordered, korbit.KV{Key: accountseq.APIName, Value: params[accountseq.APIName]})
	}

	// Cross-field validation runs once, after accountSeq is resolved into params,
	// so it sees the complete set — the same order the bot and MCP frontends use
	// (Ensure then CrossValidate).
	if cv := sc.op.Meta().CrossValidate; cv != nil {
		if err := cv(params); err != nil {
			return err
		}
	}

	baseURL, _ := rt.resolveBaseURL(cmd, cfg, meta.BaseURL)
	if err := probe.ValidateBaseURL(baseURL, needsAuth); err != nil {
		return err
	}

	timeoutMs := 15000
	if cmd.Flags().Changed("timeout") {
		if timeoutMs, err = parseRange(rt.timeout, "--timeout", 1, 600000); err != nil {
			return err
		}
	}
	// Auto-retry budget for idempotent calls (0 disables). Default 5s.
	retryBudgetMs := 5000
	if cmd.Flags().Changed("retry-timeout") {
		if retryBudgetMs, err = parseRange(rt.retryTimeout, "--retry-timeout", 0, 600000); err != nil {
			return err
		}
	}

	if rt.dryRun {
		// Show the request without signing or sending; credentials are not
		// touched, so dry runs work before any key is set up.
		doc := dryRunDoc{
			DryRun:  true,
			Request: dryRunRequest{sc.Method, sc.Path, baseURL, needsAuth, orderedObject(ordered)},
		}
		if isPlace {
			// Customer-protection preflight: fetch PUBLIC market data (orderbook +
			// tick size) at the SAME resolved baseURL the real order would use (so a
			// sandbox key checks its own book, never prod) and add a SIMULATED
			// outcome plus advisory warnings — a market order that would sweep to a
			// bad price, a limit price far from market, a post-only that would be
			// rejected, etc. It signs nothing. Best-effort: when the data can't be
			// fetched (offline, an untradable symbol, or — in --debug — an unwritable
			// journal) the plan is still emitted, with a skip note.
			//
			// The reads go through the normal journal-backed recorder, so they follow
			// the same policy as every other call: public calls are journaled only in
			// --debug (a normal run opens no DB, so the dry-run still works before any
			// key is set up and on a read-only home), which is exactly what lets a
			// `--debug` session troubleshoot the preflight from `korbit logs`.
			sim, ws, skipErr := rt.preplaceCheck(home, baseURL, params, timeoutMs)
			if skipErr != nil {
				doc.ChecksSkipped = fmt.Sprintf("market simulation/safety checks skipped: %v", skipErr)
			} else {
				doc.Simulation = &sim
				doc.Warnings = ws
			}
		}
		return rt.Emit("dry-run", doc)
	}

	log := rt.logger()

	// journalRecErr captures a post-call api_calls journal-write failure under the
	// cli's Fail policy (via the recorder's onPostFailure sink below), surfaced
	// fatally AFTER the API result; the operations-ledger Fail error rides
	// res.JournalErr — both joined and surfaced together.
	var journalRecErr error

	// The journal-backed recorder is the single home of journaling policy
	// (internal/callrec): the L1 client gates the first send on its Ready and
	// writes one Record after, so the policy — writes always journaled, reads only
	// in --debug, a post-success failure fatal — lives in callrec.DefaultPolicy
	// rather than inline here. The DB is opened lazily (only when a call actually
	// records), so a read-only command without --debug never touches a read-only
	// home, and KORBIT_CLI_NO_JOURNAL short-circuits before any open.
	rec := rt.NewRecorder(home, log, func(mode callrec.FailMode, err error) {
		if mode == callrec.Fail {
			journalRecErr = err
			return
		}
		log.Warn(formatJournalWarn(err))
	})
	defer rec.Close()

	// One shared server-clock estimate for this command, measured through the
	// central Syncer. Until measured it is the bare local clock (offset 0);
	// signing is corrected reactively on an EXCEED_TIME_WINDOW rejection (the
	// idempotent calls via the client's internal Resync+Clock correction, the
	// place protocol via ops.Resync — both wired to this one Syncer).
	localNow := rt.localNow()
	clk := clock.New(localNow)
	syncer := rt.NewClockSyncer(clk, baseURL, timeoutMs, korbit.SurfaceCLI, "clock")

	var creds *korbit.Credentials
	var keyName, apiKeyID string
	if needsAuth {
		resolved, err := km.ResolveSelection(sel, rt.deps.Getenv)
		if err != nil {
			return err
		}
		signer, err := resolved.Signer()
		if err != nil {
			return err
		}
		keyName, apiKeyID = resolved.Name, resolved.APIKeyID
		creds = &korbit.Credentials{APIKeyID: resolved.APIKeyID, Signer: signer}
		// Always-shown safety disclosure (which key/account is acting), not a
		// level-gated log and not part of the stdout result.
		rt.io.Notef("korbit-cli: signing as key %q", resolved.Name)
	}

	// --time-sync on (proactive): measure the server clock once before the first
	// send, so even a single-shot money mover (which an EXCEED_TIME_WINDOW would
	// fail cleanly without resending) signs in the server's window on the first
	// try. Best-effort: an unreachable /v2/time falls back to the local clock and
	// the reactive correction. The widened recvWindow is applied automatically —
	// the client reads it from the now-measured shared clock at sign time, so
	// nothing is copied here.
	timeSync := rt.timeSyncMode()
	if needsAuth && timeSync.Proactive() {
		if err := syncer.Sync(); err != nil {
			log.Warn(fmt.Sprintf("--time-sync could not reach %s/v2/time (%v); signing with the local clock", baseURL, err))
		} else {
			log.Info(fmt.Sprintf("time-sync: server clock %+dms vs local; signing with a corrected timestamp", clk.Offset()))
		}
	}

	// Debug-level diagnostics: whether they print is the logger's decision (the
	// resolved --log-level / --debug threshold), so the call is unconditional and
	// the handler drops it below threshold. The per-request recvWindow (the clock's
	// auto-widened window) is logged by the wire layer's "http send" line.
	log.Debug(fmt.Sprintf("%s %s -> %s (auth=%t, timeout=%dms, retryBudget=%dms)",
		sc.Method, sc.Path, baseURL, needsAuth, timeoutMs, retryBudgetMs))

	// One shared resync for both corrective paths: the L1 ladder's
	// EXCEED_TIME_WINDOW retry (built into the client from Clock+Resync, for the
	// idempotent GET / idempotent-write calls) and ops.Resync (the single-shot
	// place protocol's one corrective resync+resend — EXCEED_TIME_WINDOW is
	// provably pre-execution, so resending the same clientOrderId is safe). Public
	// calls get no auto-correct (resync nil), and so does --time-sync off (the
	// caller opted out of all clock correction).
	var resync func() error
	if needsAuth && timeSync.Reactive() {
		resync = syncer.Sync
	}

	// The single korbit.Client construction site (rt.BuildClient): it records each
	// call itself via the uniform per-call closure (during an ops Operation the
	// call lands under the operation with its sequence; off-operation it records
	// standalone), so there is no recorder wrapper here. The cli's Fail policy is
	// delivered by the recorder's onPostFailure sink above into journalRecErr,
	// surfaced fatally AFTER the result; the operations-ledger finish error rides
	// Result.JournalErr — both joined below.
	client := rt.BuildClient(clienv.ClientSpec{
		Surface:   korbit.SurfaceCLI,
		Detail:    commandKey,
		BaseURL:   baseURL,
		Creds:     creds,
		KeyName:   keyName,
		Clock:     clk,
		Resync:    resync,
		TimeoutMs: timeoutMs,
		Rec:       rec,
		Log:       log,
	})

	// Every CLI endpoint command runs through the L2 ops layer, so the CLI, the
	// monitor's bot runtime, and the mcp server share ONE set of guarantees: the
	// place operation runs the clientOrderId reconcile protocol and returns the
	// full fetched order (unless --no-reconcile opts out via Controls.SkipReconcile);
	// history/fills/candles/funding page transparently and flag truncation;
	// everything else is a safety-derived passthrough (idempotent reads/writes
	// auto-retry, money-movers single-shot). The recorder is the ops.OpJournal:
	// each Operation.Run begins an operation (the persist decision + the pre-send
	// operations-row open happen there), threads the handle through ctx, and the
	// client's per-call closure binds each call to it.
	// NewAPI derives the clock/resync/sleep/log seams from the client itself, so
	// the CLI sets only the ops-level policy. Stderr is left nil (io.Discard): ops
	// returns its truncation/ack notes in Result.Note, which the CLI surfaces below
	// in its own note idiom.
	api := ops.NewAPI(client)
	api.RetryBudgetMs = retryBudgetMs
	api.Journal = rec

	startedAt := rt.deps.Now()
	// --no-reconcile maps to Controls.SkipReconcile, which only the place
	// operation honors (it sends once and returns the raw accept ack, classifying
	// an ambiguous failure as UNKNOWN); every other operation ignores it.
	res, callErr := sc.op.Run(context.Background(), api, ops.RunInput{
		Values:   params,
		Controls: ops.Controls{SkipReconcile: rt.noReconcile, Surface: korbit.SurfaceCLI},
		KeyName:  keyName,
		APIKeyID: apiKeyID,
	})
	finishedAt := rt.deps.Now()

	outcome := "ok"
	if callErr != nil {
		var ae *output.ApiError
		if errors.As(callErr, &ae) {
			outcome = fmt.Sprintf("HTTP %d %s", ae.HTTPStatus, ae.Code)
		} else {
			outcome = "transport error"
		}
	}
	log.Debug(fmt.Sprintf("completed in %dms after %d attempt(s): %s", finishedAt-startedAt, res.Attempts, outcome))

	// The api_calls write failure and the operations-ledger finish failure are
	// independent journal errors — surface both (the cli surface's Fail policy).
	logErr := errors.Join(journalRecErr, res.JournalErr)

	if rec.Opened() && logErr == nil {
		log.Debug(fmt.Sprintf("recorded to action journal %s", rec.Path()))
	}

	if callErr != nil {
		// Error-path guidance (duplicate/UNKNOWN placement) has no success stdout
		// document to carry it. For API errors, the structured error envelope renders
		// only the inner ApiError, so attach the protocol guidance to it (error.guidance
		// + the human error line) — a JSON/stdout-only consumer then never has to parse
		// a free-floating stderr note. For transport/internal errors, the wrapped error
		// message already includes the guidance, so it surfaces through err.Error().
		var apiErr *output.ApiError
		if res.Note != "" && errors.As(callErr, &apiErr) {
			apiErr.Guidance = res.Note
		}
		// The call already failed; note any journal failure but surface the real
		// (API/network) error so its exit-code classification is preserved.
		if logErr != nil {
			log.Warn(fmt.Sprintf("failed to write to the action journal: %v", logErr))
		}
		return callErr
	}

	result := res.Data
	if isPlace {
		// Echo the clientOrderId in the result (the full fetched order already
		// carries it; the bare accept ack from --no-reconcile may not). spliceField
		// is idempotent — it drops any existing occurrence and re-adds it last.
		result = spliceField(res.Data, "clientOrderId", params["clientOrderId"])
		if res.Note != "" {
			// Eventual-consistency fallback: the placement landed but the full order
			// couldn't be read back, so res.Data is the accept ack (orderId only, no
			// fill state), not the full order. Mark it IN-BAND so a --json consumer
			// can tell deterministically that this is not the full order rather than
			// guessing from missing fields. Mirrors the bot API's acknowledgmentOnly
			// flag (botapi/overrides.go). res.Note is set on a place result only on
			// this success-path fallback — the duplicate / UNKNOWN paths return an
			// error and never reach this emit.
			result = spliceRawField(result, "acknowledgmentOnly", json.RawMessage("true"))
			result = spliceField(result, "note", res.Note)
		}
	}
	if res.Truncated {
		// The paged list endpoints (history/fills/candles/funding) couldn't fully
		// cover the window. Carry that signal IN-BAND for --json consumers reading
		// only stdout — the human note on stderr alone is invisible to a script.
		// The complete (common) case keeps the bare-array shape; only an incomplete
		// result is wrapped. The human formatters unwrap it (see listView).
		result = wrapTruncated(result, res.Note)
	}
	emitErr := rt.Emit(commandKey, result)
	if logErr != nil {
		// The call succeeded and its result is already on stdout; surface the
		// journal-write failure loudly rather than let the action history go
		// silently incomplete.
		return fmt.Errorf("the API call succeeded (result above) but recording it to the action journal at %s failed: %w — your local action history is now incomplete", rec.Path(), logErr)
	}
	return emitErr
}

// dryRunDoc is the --dry-run plan emitted for any endpoint command. Simulation,
// Warnings, and ChecksSkipped are populated only for `order place` (the
// customer-protection preflight); all are omitted otherwise, so a plain
// command's plan is unchanged.
type dryRunDoc struct {
	DryRun        bool                 `json:"dryRun"`
	Request       dryRunRequest        `json:"request"`
	Simulation    *ops.PlaceSimulation `json:"simulation,omitempty"`
	Warnings      []ops.PlaceWarning   `json:"warnings,omitempty"`
	ChecksSkipped string               `json:"checksSkipped,omitempty"`
}

type dryRunRequest struct {
	Method  string          `json:"method"`
	Path    string          `json:"path"`
	BaseURL string          `json:"baseUrl"`
	Auth    bool            `json:"auth"`
	Params  json.RawMessage `json:"params"`
}

// FormatText renders the --dry-run plan for human output (textout.TextFormatter);
// --json marshals the dryRunDoc struct. The params stay a json.RawMessage so
// OrderedKV can preserve the exact insertion order the request would send.
func (d dryRunDoc) FormatText(w io.Writer) {
	// The banner is shared by every command's dry-run. For order place the
	// preflight fetches public market data (so "no request sent" would be wrong);
	// for every other command nothing is sent at all.
	banner := "DRY RUN — no request sent"
	switch {
	case d.Simulation != nil:
		banner = "DRY RUN — order NOT sent (public market data was fetched for the simulation/checks below)"
	case d.ChecksSkipped != "":
		banner = "DRY RUN — order NOT sent (market data unavailable; see note below)"
	}
	auth := "public"
	if d.Request.Auth {
		auth = "signed"
	}
	fmt.Fprintf(w, "%s\n  %s %s%s  (auth: %s)", banner, d.Request.Method, d.Request.BaseURL, d.Request.Path, auth)
	if kv, ok := textout.OrderedKV(d.Request.Params); ok && len(kv) > 0 {
		fmt.Fprint(w, "\n  params:\n")
		fmt.Fprint(w, textout.IndentLines(textout.KVBlock(kv), "    "))
	}
	if sim := d.Simulation; sim != nil {
		fmt.Fprint(w, "\n\n  SIMULATION (estimate only — nothing placed):")
		rows := [][2]string{
			{"best bid / ask", sim.BestBid + " / " + sim.BestAsk},
			{"mid", sim.Mid},
		}
		if sim.Notional != "" {
			rows = append(rows, [2]string{"notional" + quoteUnit(sim.QuoteCurrency), sim.Notional})
		}
		if sim.EstPegPrice != "" {
			rows = append(rows, [2]string{"est. peg price", sim.EstPegPrice})
		}
		rows = append(rows, [2]string{"marketable", textout.YesNo(sim.Marketable)})
		if sim.EstFilledQty != "" {
			rows = append(rows, [2]string{"est. filled qty", sim.EstFilledQty})
		}
		if sim.EstFilledQuote != "" {
			rows = append(rows, [2]string{"est. filled" + quoteUnit(sim.QuoteCurrency), sim.EstFilledQuote})
		}
		if sim.EstAvgFillPrice != "" {
			rows = append(rows, [2]string{"est. avg fill price", sim.EstAvgFillPrice})
		}
		if sim.EstWorstFillPrice != "" {
			rows = append(rows, [2]string{"est. worst fill price", sim.EstWorstFillPrice})
		}
		if sim.EstSlippagePct != "" {
			rows = append(rows, [2]string{"est. slippage", sim.EstSlippagePct})
		}
		rows = append(rows, [2]string{"fully filled", textout.YesNo(sim.FullyFilled)})
		if sim.EstRemainingQty != "" {
			rows = append(rows, [2]string{"est. remaining qty", sim.EstRemainingQty})
		}
		if sim.Disposition != "" {
			rows = append(rows, [2]string{"remainder", sim.Disposition})
		}
		fmt.Fprint(w, "\n")
		fmt.Fprint(w, textout.IndentLines(textout.KVBlock(rows), "    "))
	}
	if len(d.Warnings) > 0 {
		fmt.Fprintf(w, "\n\n  ⚠ %d warning(s):", len(d.Warnings))
		for _, warn := range d.Warnings {
			fmt.Fprintf(w, "\n    • [%s] %s", warn.Code, warn.Message)
		}
	}
	if d.ChecksSkipped != "" {
		fmt.Fprintf(w, "\n\n  note: %s", d.ChecksSkipped)
	}
}

// quoteUnit renders a quote-denominated row label's unit suffix — " (KRW)" for a
// KRW-quoted pair, " (USDT)" for a USDT-quoted one, and nothing when the symbol
// carries no quote currency. The currency comes from the pair, never assumed.
func quoteUnit(quoteCurrency string) string {
	if quoteCurrency == "" {
		return ""
	}
	return " (" + strings.ToUpper(quoteCurrency) + ")"
}

// preplaceCheck runs the order-place customer-protection preflight: it builds a
// creds-less public client at the resolved baseURL (so the per-key/env/flag host
// is honored — a sandbox key checks its own orderbook) and asks the ops layer
// to simulate the order and analyze it against live market data. It signs
// nothing. A non-nil error means the analysis was skipped (market data
// unreachable, or — in --debug — an unwritable journal) — the caller still emits
// its plan.
//
// The public reads go through the shared journal-backed recorder under the same
// "cli" policy as every other command: public calls are journaled only in
// --debug (a normal run opens no DB), so the preflight shows up in `korbit logs`
// for a --debug troubleshooting session, and a normal dry-run still touches
// nothing. A post-write failure is non-fatal here (a preview shouldn't die over
// it) — it is surfaced as a stderr warning.
func (rt *runtime) preplaceCheck(home, baseURL string, params map[string]string, timeoutMs int) (ops.PlaceSimulation, []ops.PlaceWarning, error) {
	log := rt.logger()
	// A post-write failure during a preview is never fatal (the simulation is still
	// good), so both modes just warn here.
	rec := rt.NewRecorder(home, log, func(_ callrec.FailMode, err error) {
		log.Warn(fmt.Sprintf("failed to record the dry-run market-data read to the action journal: %v", err))
	})
	defer rec.Close()

	// A creds-less public client; PrePlaceCheck runs no ops Operation, so the
	// client's per-call closure records each read as a standalone (adhoc) row under
	// the same policy (journaled only in --debug). No clock/resync — public reads.
	client := rt.BuildClient(clienv.ClientSpec{
		Surface:   korbit.SurfaceCLI,
		Detail:    cmdPlace,
		BaseURL:   baseURL,
		TimeoutMs: timeoutMs,
		Rec:       rec,
		Log:       log,
	})
	return ops.PrePlaceCheck(context.Background(), rawapi.New(client, log), params)
}

// wrapTruncated wraps a list result's array in an envelope that carries the
// truncation signal in-band: {"data": <array>, "truncated": true, "note": ...}.
// The array bytes pass through verbatim (field order data, truncated, note). It
// is applied only to an incomplete result, so a complete list keeps its bare-array
// shape; the human formatters unwrap it via listView. If data is not a JSON array
// it is still wrapped (the signal matters more than the shape), but in practice
// only the array-returning paged endpoints set Truncated.
func wrapTruncated(data json.RawMessage, note string) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"data":`)
	b.Write(bytes.TrimSpace(data))
	b.WriteString(`,"truncated":true`)
	if note != "" {
		nb, _ := json.Marshal(note)
		b.WriteString(`,"note":`)
		b.Write(nb)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// measureOffset probes the server clock (/v2/time) over the request base URL,
// returning the full measurement (offset + min RTT) for callers that need the
// direction/magnitude, such as doctor's clock-skew diagnosis. The signing path
// goes through the Syncer (newClockSyncer) instead.
func (rt *runtime) MeasureOffset(baseURL string, timeoutMs int) (korbit.ClockOffset, error) {
	mopts := korbit.Options{BaseURL: baseURL, TimeoutMs: timeoutMs, Doer: rt.deps.Doer, Now: rt.deps.Now, UserAgent: useragent.For(korbit.SurfaceCLI, "clock")}
	return korbit.MeasureClockOffset(mopts, 0, rt.logger()) // 0 => the library default probe count
}

// orderedObject renders an ordered list of string params as a JSON object,
// preserving order (unlike a Go map).
func orderedObject(kvs []korbit.KV) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range kvs {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(kv.Key)
		val, _ := json.Marshal(kv.Value)
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// spliceField sets a STRING field on a JSON object, preserving the existing
// field order and placing the field last — replacing any existing top-level
// occurrence rather than appending a duplicate key (mirrors {...obj, key: val}).
func spliceField(raw json.RawMessage, key, value string) json.RawMessage {
	vb, _ := json.Marshal(value)
	return spliceRawField(raw, key, vb)
}

// spliceRawField is spliceField with a pre-encoded JSON value (so a non-string
// field — e.g. a boolean flag — can be added). value must be valid JSON. If raw
// is not a JSON object it is returned unchanged. It walks the object via a token
// stream so values containing braces/quotes and nested structures are handled
// correctly.
func spliceRawField(raw json.RawMessage, key string, value json.RawMessage) json.RawMessage {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return raw
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return raw
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return raw
		}
		k, ok := kt.(string)
		if !ok {
			return raw
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return raw
		}
		if k == key {
			continue // drop an existing occurrence; we re-add it last
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(bytes.TrimSpace(v))
		b.WriteByte(',')
	}
	kb, _ := json.Marshal(key)
	b.Write(kb)
	b.WriteByte(':')
	b.Write(bytes.TrimSpace(value))
	b.WriteByte('}')
	return b.Bytes()
}

// baseTier identifies which precedence tier supplied the resolved REST base
// URL. WebSocket-URL resolution uses it to decide whether a stored per-key or
// config WebSocket override applies (it does only when its own tier also won the
// REST resolution — otherwise the WebSocket URL is derived from the resolved
// REST URL so a higher REST override can't be paired with a stale stored host).
type baseTier int

const (
	tierProdDefault baseTier = iota
	tierConfig
	tierPerKey
	tierEnv
	tierFlag
)

// resolveBaseURL applies the base-URL precedence shared by every command, from
// highest to lowest: --base-url flag > KORBIT_CLI_BASE_URL env > the signing
// key's own baseUrl (keyBaseURL, "" when not applicable) > config.json baseUrl >
// prod. Per-invocation overrides (flag/env) thus always win over anything
// stored; the global config.json default is the lowest configured tier, above
// only the built-in production host. It also returns the tier that won, for
// WebSocket-URL resolution.
func (rt *runtime) resolveBaseURL(cmd *cobra.Command, cfg config.Config, keyBaseURL string) (string, baseTier) {
	baseURL, tier := probe.ProdBaseURL, tierProdDefault
	switch {
	case cmd.Flags().Changed("base-url"):
		baseURL, tier = rt.baseURL, tierFlag
	case rt.deps.Getenv("KORBIT_CLI_BASE_URL") != "":
		baseURL, tier = rt.deps.Getenv("KORBIT_CLI_BASE_URL"), tierEnv
	case keyBaseURL != "":
		baseURL, tier = keyBaseURL, tierPerKey
	case cfg.BaseURL != "":
		baseURL, tier = cfg.BaseURL, tierConfig
	}
	return strings.TrimRight(baseURL, "/"), tier
}

// resolveWSBaseURL resolves the effective WebSocket base URL with a precedence
// that mirrors resolveBaseURL: --ws-base-url flag > KORBIT_CLI_WS_BASE_URL env >
// the per-key wsBaseUrl (only when the REST base also resolved from the per-key
// tier) > config.json wsBaseUrl (only when REST resolved from config or the prod
// default) > derived from the resolved REST base URL. keyWSBaseURL is the
// signing key's stored WebSocket override ("" when none or not applicable).
func (rt *runtime) resolveWSBaseURL(cmd *cobra.Command, cfg config.Config, keyWSBaseURL, restBaseURL string, restTier baseTier) (string, error) {
	switch {
	case cmd.Flags().Changed("ws-base-url"):
		return strings.TrimRight(rt.wsBaseURL, "/"), nil
	case rt.deps.Getenv("KORBIT_CLI_WS_BASE_URL") != "":
		return strings.TrimRight(rt.deps.Getenv("KORBIT_CLI_WS_BASE_URL"), "/"), nil
	case restTier == tierPerKey && keyWSBaseURL != "":
		return strings.TrimRight(keyWSBaseURL, "/"), nil
	case restTier <= tierConfig && cfg.WSBaseURL != "":
		return strings.TrimRight(cfg.WSBaseURL, "/"), nil
	}
	return probe.DeriveWSBaseURL(restBaseURL), nil
}

// ResolveBaseURL is the clienv.Backend REST-only base-URL resolver: it discards
// the precedence tier (an internal detail of WS pairing) and returns just the URL.
func (rt *runtime) ResolveBaseURL(cmd *cobra.Command, cfg config.Config, keyBaseURL string) string {
	u, _ := rt.resolveBaseURL(cmd, cfg, keyBaseURL)
	return u
}

// ResolveURLs resolves the REST base URL and its paired WebSocket base URL in one
// step (the clienv.Backend capability for the streaming and diagnostic commands),
// keeping the precedence tier private to this package.
func (rt *runtime) ResolveURLs(cmd *cobra.Command, cfg config.Config, keyBaseURL, keyWSBaseURL string) (string, string, error) {
	rest, tier := rt.resolveBaseURL(cmd, cfg, keyBaseURL)
	ws, err := rt.resolveWSBaseURL(cmd, cfg, keyWSBaseURL, rest, tier)
	return rest, ws, err
}
