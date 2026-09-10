// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keymgmtcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/doctorcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
)

// setupResumeResult is a `setup` re-run on an existing but unbound key: it
// resumes by re-printing the registration link.
type setupResumeResult struct {
	Name             string        `json:"name"`
	PublicKey        string        `json:"publicKey"`
	Bound            bool          `json:"bound"`
	Status           string        `json:"status"`
	RegistrationURL  string        `json:"registrationUrl"`
	RegistrationLink string        `json:"registrationLink,omitempty"`
	IPAllowlist      *probe.Report `json:"ipAllowlist,omitempty"`
	Next             []string      `json:"next"`
}

func (r setupResumeResult) FormatText(w io.Writer) {
	headline := i18n.T("Key %q exists but isn't registered yet — finish registering it.", r.Name)
	writeRegistrationGuidance(w, headline, r.Next, r.PublicKey, r.RegistrationLink)
}

// setupDoneResult is a `setup` that ended already configured: just bound by
// `setup --api-key` (status "configured"), a re-run on a key that was already
// bound ("alreadyConfigured"), or a credential supplied inline via the
// environment ("configuredViaEnvironment", Name = keys.InlineDisplayName). Doctor
// carries the complementary health check setup runs, when wired.
type setupDoneResult struct {
	Name     string            `json:"name"`
	Bound    bool              `json:"bound"`
	Status   string            `json:"status"`
	APIKeyID *string           `json:"apiKeyId"`
	Doctor   *doctorcmd.Report `json:"doctor,omitempty"`
	// Next lists the follow-up options for an already-configured key (ways to
	// verify, replace, or create another); empty otherwise.
	Next []string `json:"next,omitempty"`
}

func (r setupDoneResult) FormatText(w io.Writer) {
	fmt.Fprint(w, r.headline())
	if len(r.Next) > 0 {
		fmt.Fprintf(w, "\n\n%s\n%s", i18n.T("Options:"), numbered(r.Next))
	}
	// The complementary health check is part of the result — render it on stdout
	// (advisory; it never changed setup's exit code). This is the SINGLE canonical
	// rendering of the health check for every path, interactive included: the
	// interactive prompt shows only a brief live status (doctorResultLines), so the
	// full checklist and its re-check guidance live here and are never duplicated.
	// inline keys re-check with a plain `doctor`, which the note derives from the
	// configuredViaEnvironment status.
	//
	// When the key is bound but no report is attached, the health check did not
	// complete — the interactive prompt exited (finish-later) while the auto-claim
	// lane's check was still in flight, so `final` was snapshotted before its Doctor
	// was set. The key IS configured, but a blocking issue could be unreported, so
	// never fall silent: point the user at an explicit re-check before trading.
	switch {
	case r.Doctor != nil:
		fmt.Fprint(w, complementaryDoctorNote(r.Doctor, r.Status == "configuredViaEnvironment"))
	case r.Bound:
		fmt.Fprintf(w, "\n\n%s", i18n.T("Health check not completed — verify the key before trading with `%s`.", r.recheckCommand()))
	}
}

// recheckCommand is the `doctor` invocation that re-runs the health check for this
// result: scoped to the stored key by name, or plain `doctor` for an inline
// credential (which has no stored key to name).
func (r setupDoneResult) recheckCommand() string {
	cmd := progname.Name() + " doctor"
	if r.Status != "configuredViaEnvironment" && r.Name != "" {
		cmd += " --key " + r.Name
	}
	return cmd
}

// headline is the one-line summary of an already-configured outcome.
func (r setupDoneResult) headline() string {
	switch {
	case r.Status == "configuredViaEnvironment":
		if r.APIKeyID != nil && *r.APIKeyID != "" {
			return i18n.T("A credential is configured via the environment (apiKeyId %s) — there is no stored key to set up. To create and manage a stored key instead, unset the KORBIT_CLI_API_KEY_* variables.", *r.APIKeyID)
		}
		return i18n.T("A credential is configured via the environment (KORBIT_CLI_API_KEY_*) — there is no stored key to set up. To create and manage a stored key instead, unset those variables.")
	case r.Status == "configured":
		if r.APIKeyID != nil && *r.APIKeyID != "" {
			return i18n.T("Key %q is configured (apiKeyId %s).", r.Name, *r.APIKeyID)
		}
		return i18n.T("Key %q is configured.", r.Name)
	default: // alreadyConfigured
		if r.APIKeyID != nil && *r.APIKeyID != "" {
			return i18n.T("Key %q is already configured (apiKeyId %s).", r.Name, *r.APIKeyID)
		}
		return i18n.T("Key %q is already configured.", r.Name)
	}
}

// defaultKeyName is the name `setup` gives a key when --name is omitted, and the
// single source of that literal: guidance that decides whether `setup` can target
// a key without --name compares against it (re-running `setup` with no --name
// resolves to this name). It is NOT the default-key concept — which key is the
// default is keys.Manager's DefaultKey, queried via IsDefault / DefaultKeyName.
const defaultKeyName = "default"

// The `setup` top-level command: generate (and optionally bind) a key, guide the
// human through portal registration, and — once a key is bound — run a
// complementary `doctor` health check. It shares the key-creation primitives
// (registrationLink/newKeyGuidance/emitNewKey) with `key add` in keycmd.go; only
// the setup-specific flow lives here.

// RunSetup implements the `setup` command (key name defaults to "default"). Every
// path that ends already-configured runs the complementary health check (see
// runSetupDoctor). The full behavior matrix:
//
//	situation                            result
//	-----------------------------------  ------------------------------------------
//	setup (new key)                      generate + registration link, no doctor
//	setup (unbound key exists)           resume (re-print link), no doctor
//	setup (bound key exists)             already configured + doctor
//	setup --wait (new or unbound key)    print link, poll, then bind + doctor on
//	                                     claim (headless; resume doc on stop/timeout)
//	setup --api-key (new key)            error — register the public key first
//	setup --api-key (unbound key)        bind + doctor
//	setup --api-key (bound, same id)     no-op (already configured) + doctor
//	setup --api-key (bound, diff id)     error — setup never rebinds
//	setup --api-key SANDBOX_…            error — not created via setup
//	inline KORBIT_CLI_API_KEY_* set      report env credential + doctor; --api-key errors
//
// The awaiting rows (new/unbound, no --api-key/--wait) finish per the wired path:
// the on-TTY interactive prompt, else print-and-exit. --wait selects the headless
// poll instead and is a no-op on an already-finished (bound/inline) key.
func RunSetup(flags map[string]string, positionals []string, ctx KeyContext) error {
	if err := clienv.RequireNoArgs(positionals); err != nil {
		return err
	}
	apiKey := strings.TrimSpace(flags["api-key"])
	// An inline credential supplied via the environment (KORBIT_CLI_API_KEY_*) means
	// the credential already exists, with no keystore involved — there is nothing
	// for setup to create. Report the active credential and health-check it, the
	// same "already configured + doctor" shape as a bound stored key. --api-key has
	// no meaning here.
	if ctx.Getenv != nil && keys.InlineCredsPresent(ctx.Getenv) {
		if apiKey != "" {
			return output.Usagef("--api-key can't be combined with an inline credential (KORBIT_CLI_API_KEY_*); unset the inline variables to manage a stored key")
		}
		return emitInlineSetup(ctx)
	}
	name := flags["name"]
	if name == "" {
		name = defaultKeyName
	}
	// setup generates a fresh keypair, so it can never produce a sandbox key (the
	// sandbox verifies against its own seeded key) — a sandbox api-key id here is
	// always a mistake. Reject it rather than create a stranded/default key; import
	// the sandbox key with `key add --from-pem-file ... --api-key ...` instead.
	if apiKey != "" && strings.HasPrefix(apiKey, keys.SandboxAPIKeyPrefix) {
		return output.Usagef("setup does not create sandbox keys; import it with `%s key add %s --from-pem-file <pem> --api-key %s`", progname.Name(), name, apiKey)
	}
	// Re-running setup for an existing key is not an error: resume (re-print
	// the registration link for an unbound key) or report it's already set up.
	// Show returns an error only when the key is missing (proceed to create) or
	// keys.json is unreadable (Add re-surfaces that below).
	if s, err := ctx.KM.Show(name); err == nil {
		if apiKey != "" {
			switch {
			case !s.Bound:
				// --api-key on an existing UNBOUND key finishes setup: the public key
				// has been registered since the first `setup`, so bind the issued id
				// now and health-check it.
				if err := ctx.KM.Bind(name, apiKey); err != nil {
					return err
				}
				return emitSetupBound(ctx, name, apiKey)
			case s.APIKeyID == nil || *s.APIKeyID != apiKey:
				// Already bound to a DIFFERENT id. setup never rebinds a key (binding is
				// account-sensitive); say how to replace it instead of silently keeping
				// the old id.
				return output.Usagef("key %q is already bound to a different apiKeyId — setup does not rebind. To replace it: `%s key remove %s`, then `%s setup --name %s --api-key %s`",
					name, progname.Name(), name, progname.Name(), name, apiKey)
			}
			// Bound to the same id already — fall through to the already-configured
			// report (an idempotent no-op, leaving the binding untouched).
		}
		return emitExistingSetup(ctx, s)
	}
	// Generating a NEW key: --api-key makes no sense here. A freshly generated
	// public key isn't registered yet, so any id supplied with it was issued for a
	// different key. Generate first, register the public key, then bind:
	// `setup --name <name> --api-key <KEY_ID>`.
	if apiKey != "" {
		return output.Usagef("--api-key needs an already-generated key whose public key you have registered; run `%s setup --name %s` first, register the public key, then `%s setup --name %s --api-key %s`",
			progname.Name(), name, progname.Name(), name, apiKey)
	}
	backend, err := keystoreFlag(flags)
	if err != nil {
		return err
	}
	if err := checkBaseURLFlags(ctx); err != nil {
		return err
	}
	r, err := ctx.KM.Add(name, "", backend)
	if err != nil {
		return err
	}
	// Pin the new key to --base-url when given (first-create only — re-running
	// setup on an existing key takes the resume/configured paths above and never
	// reaches here).
	if err := applyKeyBaseURL(ctx, name); err != nil {
		return err
	}
	headline := i18n.T("Generated ED25519 key %q — private key stored in the %s keystore.", r.Name, r.Keystore)
	emitAwaiting := newKeyAwaiting(ctx, r)
	if handled, err := awaitClaim(ctx, r.Name, r.PublicKey, headline, emitAwaiting); handled {
		return err
	}
	return emitAwaiting()
}

// emitExistingSetup handles a `setup` re-run when the named key already exists,
// instead of erroring: an unbound key resumes (re-prints the registration link
// from its stored public key), and a bound key reports setup is already complete
// with the options to verify, replace, or create another key. Both exit 0.
func emitExistingSetup(ctx KeyContext, s keys.SummaryWithPublic) error {
	if s.Bound {
		replace := i18n.T("Replace it: %s, then %s", progname.Name()+" key remove "+s.Name, progname.Name()+" key add "+s.Name)
		if s.Type == keys.TypeHMACSHA256 {
			replace = i18n.T("Replace its HMAC credential: %s, then %s",
				progname.Name()+" key remove "+s.Name,
				fmt.Sprintf("%s key add %s --type %s --api-key <KEY_ID> --secret-file <file>", progname.Name(), s.Name, keys.TypeHMACSHA256))
		}
		// The complementary doctor below already verifies the key; the options
		// (rendered on stdout from the result's Next) are only the ways to change it.
		res := setupDoneResult{
			Name: s.Name, Bound: true, Status: "alreadyConfigured", APIKeyID: s.APIKeyID,
			Next: []string{
				replace,
				i18n.T("Create a different key: %s", progname.Name()+" setup --name <other-name>"),
			},
		}
		res.Doctor = runSetupDoctor(ctx, s.Name)
		return ctx.emit(res)
	}

	var allowlist []string
	if ctx.IP != nil {
		allowlist = ctx.IP.Allowlist
	}
	headline := i18n.T("Key %q already exists but isn't bound yet — finish registering it.", s.Name)
	link, steps := registrationLinkAndSteps(ctx.portalURL(), s.Name, s.PublicKey, allowlist, ctx.linkPerms())
	emitAwaiting := func() error {
		return ctx.emit(setupResumeResult{
			Name: s.Name, PublicKey: s.PublicKey, Bound: false, Status: "awaitingRegistration",
			RegistrationURL: ctx.portalURL(), RegistrationLink: link, IPAllowlist: ctx.IP, Next: steps,
		})
	}
	if handled, err := awaitClaim(ctx, s.Name, s.PublicKey, headline, emitAwaiting); handled {
		return err
	}
	return emitAwaiting()
}

// emitSetupBound reports a `setup --api-key` that finished an existing unbound key
// by binding the id registered since the first `setup`.
func emitSetupBound(ctx KeyContext, name, apiKey string) error {
	id := apiKey
	res := setupDoneResult{Name: name, Bound: true, Status: "configured", APIKeyID: &id}
	res.Doctor = runSetupDoctor(ctx, name)
	return ctx.emit(res)
}

// emitInlineSetup handles `setup` when the credential is supplied inline via the
// environment (KORBIT_CLI_API_KEY_*): there is no stored key to create, so it
// reports the active credential and runs the complementary health check against it
// (the empty key name routes doctor to its inline path). It is the inline twin of
// the bound "already configured + doctor" report.
func emitInlineSetup(ctx KeyContext) error {
	// Confirm the inline credential actually resolves before reporting it
	// configured: a partial or unparsable KORBIT_CLI_API_KEY_* set is a config
	// failure (as it is for doctor and every signed command), not an advisory
	// doctor warning on a result that claims success.
	sel, err := keys.Select("", ctx.Getenv)
	if err != nil {
		return err
	}
	resolved, err := ctx.KM.ResolveSelection(sel, ctx.Getenv)
	if err != nil {
		return err
	}
	res := setupDoneResult{Name: keys.InlineDisplayName, Bound: true, Status: "configuredViaEnvironment"}
	if id := resolved.APIKeyID; id != "" {
		res.APIKeyID = &id
	}
	res.Doctor = runSetupDoctor(ctx, "")
	return ctx.emit(res)
}

// awaitClaim drives the awaiting-registration outcome for a generated-but-unbound
// key, picking the wired finishing path: the on-TTY interactive prompt, else the
// headless `--wait` poll, else handled=false so the caller prints-and-exits. The
// two id sources are mutually exclusive by construction — root wires RunInteractive
// XOR WaitForClaim — so at most one path runs.
//
// emitAwaiting writes the awaiting/resume result document — which carries the
// whole registration guidance (link, steps, public key) in its FormatText — to
// stdout. The caller's print-and-exit fallback emits it when no finishing path is
// wired.
func awaitClaim(ctx KeyContext, name, publicPEM, headline string, emitAwaiting func() error) (bool, error) {
	if handled, err := awaitingInteractive(ctx, name, publicPEM, headline, emitAwaiting); handled {
		return true, err
	}
	return awaitClaimHeadless(ctx, name, emitAwaiting)
}

// awaitClaimHeadless runs the non-interactive `setup --wait` path: block polling
// for the registered key and bind + health-check it automatically once it appears
// — no prompt, no manual paste. It is the headless twin of the interactive
// auto-claim lane (same poll → verify → bind → doctor core) without the bubbletea
// UI or the manual-paste race, so it needs no mutex. handled is false when the
// path isn't wired (WaitForClaim unset or no poller), leaving the caller's
// print-and-exit unchanged.
//
// Output discipline (stdout = result, stderr = progress/warnings):
//   - The awaiting document (with the link, in its FormatText) is emitted on stdout
//     UP FRONT, before the blocking poll, so an agent streaming the output relays
//     the link immediately while this keeps polling.
//   - Success emits a SECOND stdout document, the configured result. So a completed
//     wait writes two JSON documents (awaiting, then configured) — the last is the
//     outcome.
//   - A stop (claim conflict, IP/status gate) or the wait window elapsing leaves the
//     awaiting document standing as the outcome (exit 0, the key stays resumable)
//     and only adds a warning on stderr — no second document.
//   - Ctrl-C/kill emits nothing further, matching the non-interactive abort.
func awaitClaimHeadless(ctx KeyContext, name string, emitAwaiting func() error) (bool, error) {
	if !ctx.WaitForClaim || ctx.PollClaim == nil {
		return false, nil
	}
	// Emit the awaiting/link document on stdout first so the link is relayed before
	// the poll blocks.
	if err := emitAwaiting(); err != nil {
		return true, err
	}
	ctx.IO.Notef("Waiting for you to finish registering the key — this returns automatically once you do. Press Ctrl-C to stop; the key is saved, resume later with `%s setup --wait` or `%s setup --api-key <KEY_ID>`.", progname.Name(), progname.Name())

	pctx := context.Background()
	if ctx.WaitTimeout > 0 {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(pctx, ctx.WaitTimeout)
		defer cancel()
	}
	id, stopCode, err := ctx.PollClaim(pctx, name, func(s string) { ctx.IO.Note(s) })
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			// The wait window elapsed: the awaiting document already stands; warn on
			// stderr how to resume.
			ctx.IO.Notef("Still not registered after %s — the key is saved. Resume with `%s setup --wait` or `%s setup --api-key <KEY_ID>`.", ctx.WaitTimeout, progname.Name(), progname.Name())
			return true, nil
		case errors.Is(err, context.Canceled):
			// Session aborted. (Only reachable if pctx is cancelable — the CLI's SIGINT
			// hard-kills the process before this, so today this fires only under a
			// signal-aware caller/test.) Emit nothing more, like the non-interactive abort.
			return true, nil
		default:
			// A real fault (e.g. the key can't sign, an unreadable keystore): surface it
			// rather than silently exiting 0 — unlike the interactive lane there is no
			// manual-paste path here to fall back to.
			return true, err
		}
	}
	if id == "" {
		// Polling stopped without a claim (conflict, IP/status gate, …): the awaiting
		// document stands; explain why on stderr.
		ctx.IO.Note(ctx.claimStopNotice(stopCode))
		return true, nil
	}
	// Tolerant pre-bind whoami, exactly as the interactive auto lane: ride out the
	// just-registered KEY_NOT_FOUND window; a hard rejection (deactivated/expired,
	// or this IP isn't allowed) means we must not bind — warn and leave the awaiting
	// document standing.
	if ctx.VerifyAPIKey != nil {
		if verr := ctx.VerifyAPIKey(pctx, name, id, true); verr != nil {
			var apiErr *output.ApiError
			if errors.As(verr, &apiErr) && hardClaimReject(apiErr.Code) {
				ctx.IO.Note(ctx.claimStopNotice(apiErr.Code))
				return true, nil
			}
		}
	}
	if err := ctx.KM.Bind(name, id); err != nil {
		return true, err
	}
	res := setupDoneResult{Name: name, Bound: true, Status: "configured", APIKeyID: &id}
	res.Doctor = runSetupDoctor(ctx, name)
	return true, ctx.emit(res)
}

// awaitingInteractive runs the on-TTY setup prompt for a key that is generated
// but not yet bound: it shows the registration guidance, reads the issued key id,
// binds it, and runs the health check — all in one session — then emits the
// configured result to stdout. handled is false when no interactive runner is
// wired (the caller falls back to print-and-exit); when true, the (possibly nil)
// error is the command's result. emitAwaiting runs only if the user quits before
// pasting an id ("finish later"): the prompt is gone, so the awaiting document is
// emitted on stdout (its FormatText carries the link/steps/public key, so the
// generated key stays on the record and resumable with `setup --api-key`).
func awaitingInteractive(ctx KeyContext, name, publicPEM, headline string, emitAwaiting func() error) (bool, error) {
	if ctx.RunInteractive == nil {
		return false, nil
	}
	var allowlist []string
	if ctx.IP != nil {
		allowlist = ctx.IP.Allowlist
	}
	link, steps := registrationLinkAndSteps(ctx.portalURL(), name, publicPEM, allowlist, ctx.linkPerms())
	intro := interactiveIntro(headline, publicPEM, link, steps)

	// Two paths can finish setup concurrently: the user pasting the issued id, and
	// the background auto-claim poll detecting it. bindAndReport is the single
	// terminal action both share; mu serializes three flags. done makes the first
	// caller win and the second a no-op (both bind the SAME id for the SAME key, so
	// losing is harmless). sessionEnded, set by the caller the instant the dialog
	// closes, forbids any later bind — without it the background lane could bind
	// after the caller has already decided to emit the "finish later" resume
	// document, leaving the keystore bound behind a result that says it isn't.
	// final is the configured result the winner publishes (read after the UI closes).
	var (
		mu           sync.Mutex
		done         bool
		sessionEnded bool
		final        *setupDoneResult
	)
	// bindAndReport is identical for both lanes: each verifies the id with a
	// pre-bind whoami first (below), so this only persists the binding and runs the
	// health check. Under the lock it refuses to bind once another lane has won
	// (done) or the dialog has closed (sessionEnded), then binds and publishes
	// final — so a non-nil final is observed atomically with done, and the caller
	// can never emit the resume document over a key this lane bound. The slow health
	// check runs OUTSIDE the lock and only enriches final.Doctor afterwards, so the
	// losing path and the post-UI read never block behind it.
	bindAndReport := func(token string) (lines []string, won bool, err error) {
		mu.Lock()
		if done || sessionEnded {
			mu.Unlock()
			return nil, false, nil
		}
		if err := ctx.KM.Bind(name, token); err != nil {
			mu.Unlock()
			return nil, false, err
		}
		id := token
		res := setupDoneResult{Name: name, Bound: true, Status: "configured", APIKeyID: &id}
		done = true
		final = &res
		mu.Unlock()

		var rep *doctorcmd.Report
		if ctx.Doctor != nil {
			rep = ctx.Doctor(name)
		}
		mu.Lock()
		final.Doctor = rep
		mu.Unlock()
		return doctorResultLines(rep), true, nil
	}

	submit := func(token string) ([]string, bool, error) {
		token = strings.TrimSpace(token)
		switch {
		case token == "":
			return nil, false, errors.New(i18n.T("enter the issued key id"))
		case strings.HasPrefix(token, keys.SandboxAPIKeyPrefix):
			return nil, false, errors.New(i18n.T("that looks like a sandbox key id — setup does not create sandbox keys"))
		case apiclient.LooksLikeEd25519PrivateKey(token):
			return nil, false, errors.New(i18n.T("that's your private key — never paste private key material; paste the key id the portal issued instead"))
		case apiclient.LooksLikeEd25519PublicKey(token):
			return nil, false, errors.New(i18n.T("that looks like your public key, not the issued key id — paste the key id the portal gave you"))
		}
		// Validate the candidate id with a signed whoami BEFORE persisting it, so an
		// id the server rejects (KEY_NOT_FOUND, signature mismatch) is never written
		// to the keystore. One-shot (wait=false): a rejection is retryable — the
		// prompt stays open.
		if ctx.VerifyAPIKey != nil {
			if err := ctx.VerifyAPIKey(context.Background(), name, token, false); err != nil {
				return nil, false, verifyError(err)
			}
		}
		lines, won, err := bindAndReport(token)
		if err != nil {
			return nil, false, err
		}
		if !won {
			// Auto-claim won the race and is finishing the bind; it will drive the
			// terminal result (with the document set first). Stay non-terminal so the
			// poll's Done — not this paste — ends the session.
			return []string{"", i18n.T("Key detected — finishing up…")}, false, nil
		}
		return lines, true, nil
	}

	// autoClaim is wired only when a poller is available; nil leaves manual paste
	// as the only path.
	var autoClaim func(context.Context) <-chan ClaimUpdate
	if ctx.PollClaim != nil {
		autoClaim = func(pctx context.Context) <-chan ClaimUpdate {
			ch := make(chan ClaimUpdate, 1)
			go func() {
				defer close(ch)
				send := func(u ClaimUpdate) bool {
					select {
					case ch <- u:
						return true
					case <-pctx.Done():
						return false
					}
				}
				id, stopCode, err := ctx.PollClaim(pctx, name, func(s string) { send(ClaimUpdate{Status: s}) })
				if err != nil {
					// ctx canceled (session ending) or unexpected fault: nothing to show.
					return
				}
				if id == "" {
					// Polling stopped without a claim (conflict, IP/status gate, …): no
					// id to bind, so fall back to manual with a notice that explains why.
					send(ClaimUpdate{Stop: true, Status: ctx.claimStopNotice(stopCode)})
					return
				}
				// Same pre-bind whoami the paste lane runs, but tolerant (wait=true):
				// the just-registered key may not be active yet, so ride out
				// KEY_NOT_FOUND within a short budget. A hard rejection (the key was
				// deactivated/expired, or this IP isn't allowed) means we must not
				// bind — stop and fall back to manual, pre-filling the id we already
				// retrieved. A soft failure (timeout, transient network) proceeds
				// to bind, exactly as the manual lane binds a verified id.
				if ctx.VerifyAPIKey != nil {
					send(ClaimUpdate{Status: "Waiting for key…"})
					if verr := ctx.VerifyAPIKey(pctx, name, id, true); verr != nil {
						var apiErr *output.ApiError
						if errors.As(verr, &apiErr) && hardClaimReject(apiErr.Code) {
							send(ClaimUpdate{Stop: true, Status: ctx.claimStopNotice(apiErr.Code), Prefill: id})
							return
						}
					}
				}
				// The session may have ended (Esc/Ctrl-C) while we were verifying — the
				// caller cancels pctx on any dialog exit. Cancellation is terminal: never
				// bind behind the user's back, or the keystore would end up bound while
				// the caller emits the "finish later" resume document. This is the one
				// case the soft verify failure above must NOT ride through to a bind.
				if pctx.Err() != nil {
					return
				}
				lines, won, berr := bindAndReport(id)
				if berr != nil {
					send(ClaimUpdate{Stop: true, Status: fmt.Sprintf("Couldn't finish automatically (%v) — paste the issued key id below.", berr), Prefill: id})
					return
				}
				if won {
					send(ClaimUpdate{Done: true, Result: lines})
				}
				// !won: the paste path already finished; the session is ending.
			}()
			return ch
		}
	}

	canceled, err := ctx.RunInteractive(intro, link, submit, autoClaim)
	if err != nil {
		return true, err
	}
	// The dialog has closed. Seal the session and snapshot the result in one locked
	// step: a lane still racing inside bindAndReport either already published final
	// (we emit it as configured) or now observes sessionEnded and no-ops (we emit the
	// resume fallback) — it can never bind after we've committed to the fallback.
	// Copy final by value so the winner's slow doctor enrichment can't race the emit.
	mu.Lock()
	sessionEnded = true
	var f *setupDoneResult
	if final != nil {
		cp := *final
		f = &cp
	}
	mu.Unlock()
	if canceled {
		// Ctrl-C: a deliberate abort. Emit nothing, the same as the non-interactive
		// SIGINT abort. Any key bound before the abort stays; re-running setup is
		// idempotent.
		return true, nil
	}
	if f != nil {
		return true, ctx.emit(*f)
	}
	// "Finish later" (Esc): emit the awaiting document (its FormatText carries the
	// resume guidance) so the generated key is on the record and resumable.
	return true, emitAwaiting()
}

// verifyError turns a candidate-id verification failure into a concise, retryable
// prompt message: an API rejection names the code (e.g. KEY_NOT_FOUND); anything
// else (a network failure) tells the user to retry or fall back to --no-interactive.
func verifyError(err error) error {
	var apiErr *output.ApiError
	if errors.As(err, &apiErr) {
		detail := apiErr.Code
		if detail == "" {
			detail = apiErr.Message
		}
		return errors.New(i18n.T("Korbit rejected this key id (%s) — check it and try again, or press Esc to finish later", detail))
	}
	return errors.New(i18n.T("could not verify the key id (%s) — check connectivity and retry, or press Ctrl-C and bind with `%s`", err.Error(), progname.Name()+" setup --no-interactive --api-key <KEY_ID>"))
}

// interactiveIntro builds the guidance shown above the interactive prompt: the
// headline and the register-the-key lead-in. When a deep link exists the UI renders
// it as its own width-wrapped, clickable block (with copy/QR shortcuts) right below
// this intro, so the intro only leads into it and ends on that lead-in line; the raw
// URL is passed separately to the UI, not embedded here. With no link, it falls back
// to the manual portal step and the public key to paste. It deliberately drops the
// non-interactive "finish with `setup --api-key`" step — the prompt replaces it.
func interactiveIntro(headline, publicPEM, link string, steps []string) []string {
	lines := []string{"", headline, ""}
	if link != "" {
		return append(lines, i18n.T("To register this key, open the link below — review the permissions and IP allowlist, then confirm with MFA:"))
	}
	if len(steps) > 0 {
		lines = append(lines, steps[0])
	}
	return append(lines,
		"", i18n.T("Public key (ED25519) to paste into the developers portal:"), publicPEM,
		"", i18n.T("Then paste the issued key id below — setup binds it and runs a health check."))
}

// doctorResultLines renders the post-bind health check as a BRIEF live status for
// the interactive prompt — a single pass/warning/blocking summary line, not the
// full checklist. The full checklist and its re-check guidance are the canonical
// stdout result (complementaryDoctorNote), so keeping the prompt to a one-line
// summary means the two surfaces never duplicate the health check. nil report (no
// runner wired) reports a plain success.
func doctorResultLines(rep *doctorcmd.Report) []string {
	if rep == nil {
		return []string{"", i18n.T("Key bound — setup complete.")}
	}
	fails, warns := countChecks(rep)
	switch {
	case fails > 0:
		return []string{"", i18n.T("Key bound — health check found %d blocking issue(s); see details and the re-check command in the setup result.", fails)}
	case warns > 0:
		return []string{"", i18n.T("Key bound — health check passed with %d warning(s); see details in the setup result.", warns)}
	default:
		return []string{"", i18n.T("Key bound — health check passed.")}
	}
}

// countChecks tallies the failing and warning checks in a health-check report.
func countChecks(rep *doctorcmd.Report) (fails, warns int) {
	for _, ch := range rep.Checks {
		switch ch.Status {
		case doctorcmd.CheckFail:
			fails++
		case doctorcmd.CheckWarn:
			warns++
		}
	}
	return fails, warns
}

// runSetupDoctor runs the complementary, advisory health check for a
// just-configured (bound) key when a runner is wired, and returns the report for
// the result's `doctor` field. The report is rendered to stdout as part of the
// result (setupDoneResult.FormatText), and is in the --json document either way.
// It NEVER affects setup's exit code: a failing health check is surfaced as a
// warning, not a setup failure. Returns nil when no runner is wired or the check
// produced nothing.
func runSetupDoctor(ctx KeyContext, name string) *doctorcmd.Report {
	if ctx.Doctor == nil {
		return nil
	}
	return ctx.Doctor(name)
}

// complementaryDoctorNote renders the advisory health-check section appended to
// the setup result on stdout: the doctor checklist, then a closing line that
// frames any problems as warnings (setup has already succeeded). A blocking ✗ is
// reported as something to fix before trading, not as a setup failure.
func complementaryDoctorNote(rep *doctorcmd.Report, inline bool) string {
	var b strings.Builder
	// Blank line first so the health-check section is visually separated from the
	// preceding "configured" headline (and its options), rather than butting up
	// against it.
	b.WriteString("\n\n" + i18n.T("Complementary health check (doctor) — advisory; it does not change setup's result:") + "\n")
	rep.FormatText(&b)
	fails, warns := countChecks(rep)
	switch {
	case fails > 0:
		// --key <name> only when there is a real stored-key name to re-check; an
		// inline credential and a hard-fault report (empty rep.Key) both re-check
		// with a plain `doctor`.
		recheck := progname.Name() + " doctor"
		if rep.Key != "" && !inline {
			recheck += " --key " + rep.Key
		}
		fmt.Fprintf(&b, "\n\n%s", i18n.T("Warning: the health check found %d blocking issue(s) above. Setup completed, but fix these before trading, then re-check with `%s`.", fails, recheck))
	case warns > 0:
		fmt.Fprintf(&b, "\n\n%s", i18n.T("Note: the health check passed with %d warning(s) above.", warns))
	}
	return b.String()
}
