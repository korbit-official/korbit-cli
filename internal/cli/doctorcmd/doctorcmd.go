// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package doctorcmd implements the `doctor` command: a read-only assessment of
// whether the current key configuration can reach and trade on Korbit, with a
// fix named for every problem. It runs against the clienv.Cmd seam, so the cli
// (and the mcp doctor tool) dispatch into it without it importing cli. The
// Report renders its own human ✓/⚠/✗ checklist via FormatText
// (format.go), implementing textout.TextFormatter.
package doctorcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/keystore"
	"github.com/korbit-official/korbit-cli/internal/netbind"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/spf13/cobra"
)

// Check statuses, in increasing severity. CheckWarn does not flip the overall
// verdict; CheckFail marks a blocking config problem. Report.FormatText
// (format.go) renders these as the ✓/⚠/✗ marks.
const (
	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// networkErrorCheck is the single classifier for a transport/network failure in
// doctor's live checks, so every one reports it the same way: the human summary
// (lead) carries a short "(network error)" tag, and the raw Go error rides as
// separate Context — its own line in the text report, the `context` field in
// --json — rather than inlined into the summary or dropped. lead is the
// check-specific phrase ("could not reach the API…", "<url> — unreachable"); err
// is the underlying transport error (nil-safe). Always a non-fatal CheckWarn: a
// network failure can't verify, but the config itself may be fine.
func networkErrorCheck(name, lead, fix string, err error) Check {
	c := Check{Name: name, Status: CheckWarn, Detail: lead + " " + i18n.T("(network error)"), Fix: fix}
	if err != nil {
		c.Context = err.Error()
	}
	return c
}

// clockSkewWarnMs is the default signed-request validity window (server default
// recvWindow = 5000ms). A local/server clock gap approaching it risks
// EXCEED_TIME_WINDOW rejections, so doctor warns before it bites.
const clockSkewWarnMs = 5000

// Check is one diagnostic line: a name, a status, what was observed, and
// (when not ok) the exact fix.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Context is optional secondary detail shown beneath Detail — e.g. the raw
	// transport error behind a "(network error)" summary. Kept out of Detail so
	// the human summary stays terse while the underlying cause is still recorded
	// (its own line in the text report, the `context` field in --json).
	Context string `json:"context,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Report is the full machine-readable health report. `ok` is true when no
// check failed (warnings are allowed). It is exported so `setup` can embed the
// complementary health check it runs for a just-configured key (see Assess).
type Report struct {
	OK   bool   `json:"ok"`
	Home string `json:"home"`
	// DefaultKeystore is the backend NEW keys go to; each existing key carries
	// its own backend (the keystore checks probe every backend in use).
	DefaultKeystore string `json:"defaultKeystore"`
	Key             string `json:"key,omitempty"`
	BaseURL         string `json:"baseUrl"`
	// WSBaseURL is the WebSocket endpoint the selected key would use (what
	// `monitor` resolves), shown so a check run against the sandbox or a per-key
	// host is never mistaken for production. Doctor probes it best-effort.
	WSBaseURL string  `json:"wsBaseUrl"`
	Checks    []Check `json:"checks"`
}

// Run implements the `doctor` command: a read-only assessment of whether the
// current key configuration can reach and trade on Korbit, with a fix named for
// every problem. ExitSuccess = healthy (warnings allowed), ExitConfig = blocking
// config problem, ExitInternal = only a network failure prevented verification.
func Run(cx *clienv.Cmd, cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q", args[0])
	}
	rep, code, err := Assess(cx, cmd, "")
	if err != nil {
		return err
	}
	return finishDoctorCode(cx, rep, code)
}

// AdvisoryReport runs Assess as an ADVISORY check and always returns a report,
// never an error. A hard fault Assess surfaces as an error (e.g. an unsafe base
// URL, or an unresolvable credential) becomes a single failing check, so a caller
// that embeds the report — `setup`'s complementary health check — shows it as a
// warning instead of dropping it or failing. forceKey pins a specific key (see
// Assess). Used by callers that embed the report instead of emitting it, so they
// behave alike.
func AdvisoryReport(cx *clienv.Cmd, cmd *cobra.Command, forceKey string) *Report {
	rep, _, err := Assess(cx, cmd, forceKey)
	if err != nil {
		return &Report{Checks: []Check{{
			Name:   "doctor",
			Status: CheckFail,
			Detail: i18n.T("could not run the health check: %s", err.Error()),
			Fix:    i18n.T("fix the above, then run `%s`", progname.Name()+" doctor"),
		}}}
	}
	return &rep
}

// Assess computes the doctor report and its exit code (ExitSuccess/ExitInternal/ExitConfig) for the selected
// key WITHOUT emitting it. forceKey, when non-empty, pins the assessment to that
// stored key, bypassing the --key/default/sole selection — `setup` uses it to
// health-check the key it just configured. The returned error is reserved for
// hard faults that aren't a report check (unreadable config, an unsafe base
// URL). Run wraps this and emits; AdvisoryReport wraps it for callers that want
// those faults folded into the report instead of returned.
func Assess(cx *clienv.Cmd, cmd *cobra.Command, forceKey string) (Report, int, error) {
	if forceKey != "" {
		c := *cx
		c.Key = forceKey
		cx = &c
	}
	home, cfg, err := cx.LoadConfig()
	if err != nil {
		return Report{}, output.ExitSuccess, err
	}
	km := cx.KeyManager(home, cfg)
	// Provisional resolution without a per-key override: the key isn't chosen
	// until below, and the early-return reports (no keys, no default) have no key
	// to override from. Once the key is known we recompute with its baseUrl. The
	// WS resolution error is deliberately ignored (best-effort) — doctor still
	// reports/probes whatever endpoint resolved.
	baseURL, wsBaseURL, _ := cx.ResolveURLs(cmd, cfg, "", "")
	// doctor's primary live check is a *signed* request, so apply the same
	// base-URL safety as any private command: reject a malformed URL and refuse
	// to sign over plaintext http to a non-local host (a replayable request).
	if err := probe.ValidateBaseURL(baseURL, true); err != nil {
		return Report{}, output.ExitSuccess, err
	}
	rep := Report{Home: home, DefaultKeystore: cfg.Keystore, BaseURL: baseURL, WSBaseURL: wsBaseURL}
	add := func(name, status, detail, fix string) {
		rep.Checks = append(rep.Checks, Check{Name: name, Status: status, Detail: detail, Fix: fix})
	}

	// Single key-selection front door: a stored key (--key / KORBIT_CLI_KEY, else
	// the default/sole key) OR inline KORBIT_CLI_API_KEY_* material — mutually
	// exclusive.
	sel, err := keys.Select(cx.Key, cx.Getenv)
	if err != nil {
		return Report{}, output.ExitSuccess, err
	}

	// The config-resolution checks record their own report lines and hand back the
	// credential to verify live (nil when none is usable). A non-nil error is a
	// hard fault (an unsafe per-key base URL) that aborts; every other problem is a
	// recorded check with a nil credential, so the tail's key-independent
	// environment checks (clock skew, public IP, WebSocket, outbound family) still
	// run — doctor reports those even before a key is configured.
	resolved, name, err := doctorConfigChecks(cx, cmd, &rep, add, km, cfg, sel, &baseURL, &wsBaseURL)
	if err != nil {
		return Report{}, output.ExitSuccess, err
	}
	code := doctorTail(cx, cmd, &rep, add, home, baseURL, name, resolved, km)
	return assessed(rep, code)
}

// doctorConfigChecks records the config-resolution checks — keystore backends,
// key selection, binding, and private-key soundness (or, for an inline
// credential, its resolution) — and returns the credential to verify live, or
// nil when none is usable (a blocking check has already been recorded). It folds
// the chosen stored key's per-key endpoint override into *baseURL/*wsBaseURL (and
// the report) so the live check targets the URL that key uses. A non-nil error is
// a hard fault (an unsafe per-key base URL, or an unreadable key registry) that
// aborts doctor; every other failure returns a nil credential so the tail's
// key-independent environment checks still run.
func doctorConfigChecks(cx *clienv.Cmd, cmd *cobra.Command, rep *Report, add func(name, status, detail, fix string), km *keys.Manager, cfg config.Config, sel keys.Selection, baseURL, wsBaseURL *string) (*keys.Resolved, string, error) {
	// An inline credential has no keystore/registry to inspect — resolve it and go
	// straight to the live checks against the (already final) base URL.
	if sel.Inline {
		resolved, rerr := km.ResolveSelection(sel, cx.Getenv)
		if rerr != nil {
			add("credential", CheckFail, rerr.Error(), "")
			return nil, "", nil
		}
		rep.Key = resolved.Name
		add("credential", CheckOK, "using inline key material from the environment ($KORBIT_CLI_API_KEY_*)", "")
		return &resolved, resolved.Name, nil
	}

	list, err := km.List()
	if err != nil {
		return nil, "", err
	}
	doctorKeystoreChecks(cfg.Keystore, list, add, cx.Log)

	if len(list) == 0 {
		add("keys", CheckFail, i18n.T("no keys configured"), i18n.T("create one: %s", progname.Name()+" setup"))
		return nil, "", nil
	}
	add("keys", CheckOK, fmt.Sprintf("%d key(s) configured", len(list)), "")

	// Resolve which key to check: the selected stored key (--key / KORBIT_CLI_KEY),
	// else the default, else the sole key. Never silently pick among several (keys
	// may be different accounts).
	explicit := sel.Name
	def, _ := km.DefaultKeyName()
	// Real (non-sandbox) keys are the only candidates for the implicit sole-key
	// fallback: a sandbox key is explicit-only, so doctor never auto-selects it
	// (it would otherwise health-check the local mock instead of production).
	var realKeys []keys.Summary
	for _, s := range list {
		if !s.IsSandbox {
			realKeys = append(realKeys, s)
		}
	}
	name := explicit
	switch {
	case name != "":
		// caller chose one explicitly (any key, including a sandbox one)
	case def != "":
		name = def
	case len(realKeys) == 1:
		name = realKeys[0].Name
	}
	if name == "" {
		detail := i18n.T("multiple keys exist but no default is set")
		if len(realKeys) == 0 {
			detail = i18n.T("no usable default: the only key(s) are sandbox keys, which are explicit-only")
		}
		add("default key", CheckFail, detail,
			i18n.T("pick one: %s (or pass --key <name>)", progname.Name()+" key use <name>"))
		return nil, "", nil
	}
	rep.Key = name
	// Now that the key is chosen, fold in its endpoint override (if any) at the
	// per-key precedence tier and re-validate, so the report and the live check
	// both use the URL this key actually targets. Recomputing with an empty
	// per-key override yields the provisional URLs unchanged.
	kb, _ := km.BaseURLOf(name)
	b, w, _ := cx.ResolveURLs(cmd, cfg, kb, km.MetaWSBaseURL(name))
	if err := probe.ValidateBaseURL(b, true); err != nil {
		return nil, name, err
	}
	*baseURL, *wsBaseURL = b, w
	rep.BaseURL = b
	rep.WSBaseURL = w
	if explicit == "" && def == "" && len(realKeys) == 1 {
		add("default key", CheckWarn,
			i18n.T("no default set; using the only key %q", name),
			i18n.T("make it explicit: %s", progname.Name()+" key use "+name))
	} else {
		add("default key", CheckOK, fmt.Sprintf("using key %q", name), "")
	}

	// Find the chosen key's metadata.
	var sum *keysSummary
	for i := range list {
		if list[i].Name == name {
			s := keysSummary{Name: list[i].Name, Bound: list[i].Bound}
			sum = &s
			break
		}
	}
	if sum == nil {
		add("key exists", CheckFail, i18n.T("unknown key %q", name), i18n.T("list keys: %s", progname.Name()+" key list"))
		return nil, name, nil
	}
	if !sum.Bound {
		add("binding", CheckFail,
			i18n.T("key %q has no API key id bound yet", name),
			i18n.T("register its public key, then run `%s` (binds and health-checks; or `%s`)",
				progname.Name()+" setup --name "+name+" --api-key <KEY_ID>",
				progname.Name()+" key bind "+name+" --api-key <KEY_ID>"))
		return nil, name, nil
	}
	add("binding", CheckOK, "API key id is bound", "")

	resolved, err := km.Resolve(name)
	if err != nil {
		// Resolve fails here only for a config problem — the private key missing
		// from the keystore, OR (since the signer seam moved PEM parsing into
		// keys.Resolve) a stored PEM that won't parse. Both are ConfigErrors
		// carrying their own recovery hint; surface as a blocking check, not a
		// raw error.
		add("private key", CheckFail, err.Error(), "")
		return nil, name, nil
	}
	add("private key", CheckOK, fmt.Sprintf("present in the %s keystore", resolved.Keystore), "")
	return &resolved, name, nil
}

// assessed stamps rep.OK from the exit code (healthy unless a check failed → ExitConfig) and
// returns the Assess triple, so every return path reports a consistent verdict —
// including the report `setup` embeds, which never reaches finishDoctorCode.
func assessed(rep Report, code int) (Report, int, error) {
	rep.OK = code != output.ExitConfig
	return rep, code, nil
}

// doctorTail runs the live diagnostics and returns the exit code (ExitSuccess
// healthy, ExitConfig a blocking check failed, ExitInternal only a network
// failure prevented verification), mutating *rep in place. It probes the public
// IP once (shared by the signed allowlist diagnosis and the standalone public-IP
// check), runs the signed verification only when a credential resolved, and
// ALWAYS runs the key-independent environment checks — so doctor reports clock
// skew, public IP, WebSocket reachability, and the outbound-family restriction
// even before a key is configured. baseURL is the REST endpoint this credential
// (or, keyless, the default resolution) targets.
func doctorTail(cx *clienv.Cmd, cmd *cobra.Command, rep *Report, add func(name, status, detail, fix string), home, baseURL, name string, resolved *keys.Resolved, km *keys.Manager) int {
	// Public IP as Korbit's production API sees us — probed once and reused both
	// for the IP-allowlist diagnosis (signed path) and the standalone "public IP"
	// check (environment path).
	iprep := probe.IPs(cx.IPProbe, probe.ProdBaseURL, probe.DefaultTimeoutMs, cx.Family.Networks())

	// networkBlocked is set when a network failure (not an API rejection)
	// prevented the signed verification — that maps to exit 1, not 4.
	networkBlocked := false
	if resolved != nil {
		networkBlocked = doctorSigned(cx, cmd, rep, add, home, baseURL, name, *resolved, km, iprep)
	}
	doctorEnv(cx, cmd, rep, add, baseURL, name, iprep)

	for _, ch := range rep.Checks {
		if ch.Status == CheckFail {
			return output.ExitConfig
		}
	}
	if networkBlocked {
		return output.ExitInternal
	}
	return output.ExitSuccess
}

// doctorSigned runs the credential-dependent live verification: a signed whoami
// (trading capability, expiry, status), the default-accountSeq permission check,
// and — on an IP-allowlist rejection — the per-family replay diagnosis. It
// returns true when a network failure (not an API rejection) blocked the whoami,
// which the tail maps to ExitInternal. iprep is the already-probed public IP,
// reused for the allowlist fix. baseURL is the REST endpoint this credential
// targets, already refined for a stored key's per-key host. Shared by the
// stored-key path and the inline (KORBIT_CLI_API_KEY_*) path, so both verify
// identically.
func doctorSigned(cx *clienv.Cmd, cmd *cobra.Command, rep *Report, add func(name, status, detail, fix string), home, baseURL, name string, resolved keys.Resolved, km *keys.Manager, iprep probe.Report) bool {
	signer, err := resolved.Signer()
	if err != nil {
		// Distinct check name from "private key" above: Resolve already confirmed
		// the stored material is present and parseable, so a Signer() failure is a
		// separate (near-impossible) fault — never a contradictory pair.
		add("signer", CheckFail, err.Error(), i18n.T("re-create the key: %s, then %s", progname.Name()+" key remove "+name, progname.Name()+" key add "+name))
		return false
	}
	creds := &apiclient.Credentials{APIKeyID: resolved.APIKeyID, Signer: signer}
	// doctor honors --time-sync exactly like every other signed command: one shared
	// clock + syncer, a proactive measure for --time-sync on, and the reactive
	// resync hook (auto/on) so an EXCEED_TIME_WINDOW rejection is corrected and the
	// whoami retried rather than masking the whole key diagnosis. --time-sync off
	// wires no resync, so a skewed clock surfaces raw. The clock-skew CHECK measures
	// independently against the RAW clock (doctorClockCheck -> cx.MeasureOffset), so
	// the drift is still reported even when correction lets the whoami through. The
	// "doctor" surface is never recorded by DefaultPolicy, so the recorder's DB is
	// never opened; routing through it keeps the seam uniform.
	clk := clock.New(cx.Now)
	syncer := cx.NewClockSyncer(clk, baseURL, doctorTimeout(cmd), apiclient.SurfaceDoctor, "clock")
	var resync func() error
	if cx.Modes.TimeSync.Reactive() {
		resync = syncer.Sync
	}
	if cx.Modes.TimeSync.Proactive() {
		if err := syncer.Sync(); err != nil && cx.Log != nil {
			cx.Log.Warn(fmt.Sprintf("--time-sync could not reach %s/v2/time (%v); signing with the local clock", baseURL, err))
		}
	}
	client := cx.BuildClient(clienv.ClientSpec{
		Surface:   apiclient.SurfaceDoctor,
		BaseURL:   baseURL,
		Creds:     creds,
		KeyName:   name,
		Clock:     clk,
		Resync:    resync,
		TimeoutMs: doctorTimeout(cmd),
		Rec:       cx.LogRecorder(home, cx.Log),
		Log:       cx.Log,
	})

	networkBlocked := false
	// The whoami is an idempotent GET, so retry transient network/5xx within the
	// --retry-timeout budget, plus the --time-sync-gated EXCEED_TIME_WINDOW
	// corrective resync (a read is always safe to resend). An IP-allowlist
	// rejection and other 4xx still surface on the first attempt.
	data, _, err := client.Do(context.Background(), apiclient.Call{Method: "GET", Path: "/v2/currentKeyInfo", Auth: true}, apiclient.Policy{Idempotent: true, BudgetMs: doctorRetryBudget(cmd)})
	switch {
	case err == nil:
		doctorWhoamiOK(cx, rep, data, add)
		// Verify the key's effective default sub-account is in the allowed list.
		doctorAccountSeqCheck(km, name, resolved.Inline, data, add)
	default:
		var apiErr *output.ApiError
		if errors.As(err, &apiErr) {
			detail := apiErr.Code
			if detail == "" {
				detail = apiErr.Message
			}
			if probe.IsIPAllowlistCode(apiErr.Code) {
				// The default connection's source IP isn't allowlisted. Replay the
				// signed request over each family to isolate which one Korbit accepts
				// and give a precise fix — the "ip allowlist" check carries the cure.
				add("live whoami", CheckFail, i18n.T("API rejected the signed request: %s", detail),
					i18n.T("your IP isn't allowlisted for this key — see the 'ip allowlist' check below"))
				// Use the fast per-family probe timeout so a black-holed IPv6 route
				// can't stall doctor for the full request timeout after the verdict
				// is already known; an explicit --timeout still overrides it.
				famTimeout := probe.DefaultTimeoutMs
				if cmd.Flags().Changed("timeout") {
					famTimeout = doctorTimeout(cmd)
				}
				doctorDiagnoseAllowlist(cx, client, iprep, famTimeout, add)
			} else {
				add("live whoami", CheckFail, i18n.T("API rejected the signed request: %s", detail),
					i18n.T("check the key in the developers portal (permissions, expiry, IP allowlist)"))
			}
		} else {
			// Transport/network failure: can't verify, but the config may be fine.
			networkBlocked = true
			rep.Checks = append(rep.Checks, networkErrorCheck("live whoami",
				i18n.T("could not reach the API to verify the key"),
				i18n.T("retry when connectivity is back"), err))
		}
	}
	return networkBlocked
}

// doctorEnv runs the key-independent environment diagnostics: the outbound-family
// restriction, the public IP (for the key's allowlist), server clock skew, and
// WebSocket reachability. None needs a credential, so the tail runs it on every
// path — including before any key is configured. iprep is the already-probed
// public IP; name is the selected key ("" when none) for the WebSocket pin-it fix.
func doctorEnv(cx *clienv.Cmd, cmd *cobra.Command, rep *Report, add func(name, status, detail, fix string), baseURL, name string, iprep probe.Report) {
	// Surface the active outbound restriction so the report reflects the
	// configuration it tested (doctor is a diagnostic for the current config).
	doctorNetworkConfig(cx, add)

	// Public IP for the allowlist — always reported.
	if iprep.Any() {
		add("public IP", CheckOK,
			"Korbit sees you from: "+strings.Join(iprep.Allowlist, ", "),
			"ensure these are in the key's IP allowlist")
	} else {
		add("public IP", CheckWarn, i18n.T("could not determine your public IP"), i18n.T("check your network connection; then `%s`", progname.Name()+" ip"))
	}

	// Clock skew vs the server (pre-empts EXCEED_TIME_WINDOW).
	clockConfirmedOK := doctorClockCheck(cx, baseURL, cmd, add)

	// OS time-sync configuration — opt-in via --diagnose-clock, read-only. Only
	// then does doctor inspect OS services / the registry / spawn a probe; the
	// default run never does. It explains WHY the clock above drifted; all its
	// lines are OK/warn, so it can't change the exit code. clockConfirmedOK lets
	// it suppress "force a resync" guidance when the skew check already passed.
	if diagnoseClockRequested(cmd) {
		doctorClockConfigCheck(add, doctorTimeout(cmd), clockConfirmedOK)
	}

	// WebSocket reachability for `monitor` (best-effort, unsigned, non-fatal).
	doctorWSCheck(cx, rep, name, cmd, add)
}

// doctorKeystoreChecks reports the keystore situation: the default backend for
// new keys, plus a probe of every backend an existing key's record points at
// (keys may live in different backends). Each unreachable backend gets its own
// warning naming the keys it strands, with the recovery command as the fix.
// All probes are read-only — doctor never pops an OS keychain prompt. (The CLI
// home is shown in the report header, not repeated in these details.)
func doctorKeystoreChecks(defaultBackend string, list []keys.Summary, add func(name, status, detail, fix string), log *slog.Logger) {
	prog := progname.Name()
	problems := false
	if err := keystore.Available(defaultBackend, log); err != nil {
		problems = true
		add("keystore", CheckWarn,
			i18n.T("the default keystore for new keys (%q) is not available here: %s", defaultBackend, err.Error()),
			i18n.T("point new keys at the file keystore: %s", prog+" keystore default file"))
	}
	byBackend := map[string][]string{}
	var backends []string
	for _, s := range list {
		if _, ok := byBackend[s.Keystore]; !ok {
			backends = append(backends, s.Keystore)
		}
		byBackend[s.Keystore] = append(byBackend[s.Keystore], s.Name)
	}
	sort.Strings(backends)
	for _, b := range backends {
		if err := keystore.Available(b, log); err != nil {
			// Signing with these keys will fail downstream; flag it early with the
			// way out (per-key recovery into the file keystore).
			problems = true
			add("keystore", CheckWarn,
				i18n.T("the %s keystore holding key(s) %s is not available here: %s", b, strings.Join(byBackend[b], ", "), err.Error()),
				i18n.T("recover them into the file keystore: %s", prog+" keystore migrate file "+strings.Join(byBackend[b], " ")))
		}
	}
	if !problems {
		add("keystore", CheckOK,
			fmt.Sprintf("default keystore for new keys %q; every keystore in use is available", defaultBackend), "")
	}
}

// keysSummary is the slim subset of keys.Summary doctor needs (name + bound),
// kept local so doctor doesn't depend on the keys package's exported shape.
type keysSummary struct {
	Name  string
	Bound bool
}

// doctorNetworkConfig warns when outbound is restricted to a single IP family
// (--family, or a single-family --bind), since doctor then diagnoses only that
// family. The default dualstack checks both, so it adds nothing then.
func doctorNetworkConfig(cx *clienv.Cmd, add func(name, status, detail, fix string)) {
	if cx.Family == netbind.FamilyDual {
		return
	}
	fam := probe.FamiliesLabel(cx.Family.Networks()) // "IPv4" or "IPv6"
	add("network", CheckWarn,
		i18n.T("outbound restricted to %s — doctor checked only that family", fam),
		i18n.T("for a full both-families check, run without --family or --bind"))
}

// ipFamilies are the TCP families doctor replays a signed request over to
// diagnose an IP-allowlist rejection, each with its human label.
var ipFamilies = []struct {
	network string
	label   string
}{
	{"tcp4", "IPv4"},
	{"tcp6", "IPv6"},
}

// doctorDiagnoseAllowlist runs after the live whoami was rejected for an
// IP-allowlist reason. It replays the signed currentKeyInfo request over each
// TCP family (forcing IPv4, then IPv6) to learn which one Korbit's allowlist
// accepts:
//   - if a family is accepted, the key is allowlisted for that family only; the
//     successful response also carries the key's configured `whitelist`, so the
//     fix can name exactly what to add to cover the rejected family (or restrict
//     connections to the working one via `--family`);
//   - if neither is accepted, it points the user to the portal with the IP
//     entries (from the /v2/ip probe) to add.
//
// It always adds one "ip allowlist" check and never changes the exit code on its
// own — the failing whoami already set the verdict.
func doctorDiagnoseAllowlist(cx *clienv.Cmd, base *apiclient.Client, iprep probe.Report, timeoutMs int, add func(name, status, detail, fix string)) {
	call := apiclient.Call{Method: "GET", Path: "/v2/currentKeyInfo", Auth: true}
	var accepted, rejected []string
	whitelist := ""
	for _, f := range ipFamilies {
		// Honor --family: never replay over a family the user excluded.
		if !slices.Contains(cx.Family.Networks(), f.network) {
			continue
		}
		// A cheap per-family Client view of the same key/base/clock, pinned to one
		// TCP family. The recorder stays the doctor one (never records).
		c := *base
		c.Doer = cx.FamilyDoer(f.network, timeoutMs)
		c.TimeoutMs = timeoutMs
		data, _, err := c.Do(context.Background(), call, apiclient.Policy{})
		switch {
		case err == nil:
			accepted = append(accepted, f.label)
			if whitelist == "" {
				var info struct {
					Whitelist string `json:"whitelist"`
				}
				if json.Unmarshal(data, &info) == nil {
					whitelist = info.Whitelist
				}
			}
		default:
			var apiErr *output.ApiError
			if errors.As(err, &apiErr) && probe.IsIPAllowlistCode(apiErr.Code) {
				rejected = append(rejected, f.label)
			}
			// A family that fails to dial (no connectivity) or errors for some
			// other reason is neither accepted nor an allowlist rejection — it is
			// uninformative and left out of both lists.
		}
	}

	switch {
	case len(accepted) > 0 && len(rejected) > 0:
		detail := i18n.T("this key is allowlisted over %s but its %s connection is rejected",
			strings.Join(accepted, "/"), strings.Join(rejected, "/"))
		if whitelist != "" {
			detail += i18n.T("; configured allowlist: %s", whitelist)
		}
		add("ip allowlist", CheckWarn, detail, allowlistFamilyFix(rejected, accepted, iprep))
	case len(accepted) > 0:
		// Each family succeeds when forced individually — the default connection's
		// rejection didn't reproduce (a transient block, or the IP just changed).
		detail := i18n.T("the signed request is accepted over %s when each family is forced", strings.Join(accepted, "/"))
		if whitelist != "" {
			detail += i18n.T("; configured allowlist: %s", whitelist)
		}
		add("ip allowlist", CheckWarn, detail,
			i18n.T("re-run `%s`; if it still fails, confirm the IP from `%s` is in the allowlist", progname.Name()+" doctor", progname.Name()+" ip"))
	case len(rejected) > 0:
		add("ip allowlist", CheckFail,
			i18n.T("your %s connection is not allowlisted for this key", strings.Join(rejected, "/")),
			allowlistAddFix(iprep))
	default:
		add("ip allowlist", CheckWarn,
			i18n.T("could not replay the signed request over %s to isolate the allowlist problem (network error)", probe.FamiliesLabel(cx.Family.Networks())),
			i18n.T("retry when connectivity is back; then add the IP from `%s` to the key's allowlist", progname.Name()+" ip"))
	}
}

// allowlistFamilyFix builds the fix when one family is accepted and another is
// rejected: add the rejected family's allowlist entry, or restrict connections
// to the accepted family (`--family` pins the outbound IP family).
func allowlistFamilyFix(rejected, accepted []string, iprep probe.Report) string {
	acceptedLabel := strings.Join(accepted, "/")
	familyHint := familyFlagHint(accepted)
	var entries []string
	for _, label := range rejected {
		if e := iprep.EntryFor(label); e != "" {
			entries = append(entries, fmt.Sprintf("%s (%s)", e, label))
		}
	}
	if len(entries) > 0 {
		return i18n.T("add %s to this key's IP allowlist at %s, or connect over %s only (%s)",
			strings.Join(entries, ", "), probe.PortalURL, acceptedLabel, familyHint)
	}
	return i18n.T("add your %s address to this key's IP allowlist at %s, or connect over %s only (%s)",
		strings.Join(rejected, "/"), probe.PortalURL, acceptedLabel, familyHint)
}

// familyFlagHint renders the `--family` invocation that pins the outbound IP
// family to the accepted one(s), e.g. "`--family ipv4`".
func familyFlagHint(accepted []string) string {
	vals := make([]string, 0, len(accepted))
	for _, label := range accepted {
		vals = append(vals, "--family "+strings.ToLower(label))
	}
	return "`" + strings.Join(vals, "` or `") + "`"
}

// allowlistAddFix lists the ready-to-paste entries (from the /v2/ip probe) to add
// at the portal when no family is allowlisted.
func allowlistAddFix(iprep probe.Report) string {
	if len(iprep.Allowlist) > 0 {
		return i18n.T("go to %s and add these to the key's IP allowlist: %s",
			probe.PortalURL, strings.Join(iprep.Allowlist, ", "))
	}
	return i18n.T("go to %s and add your public IP to the key's allowlist (run `%s` to find it)", probe.PortalURL, progname.Name()+" ip")
}

// doctorWhoamiOK records the success-path whoami checks: trading capability
// (writeOrders present) and expiry.
func doctorWhoamiOK(cx *clienv.Cmd, rep *Report, data json.RawMessage, add func(name, status, detail, fix string)) {
	var info struct {
		Type        string   `json:"type"`
		Status      string   `json:"status"`
		Permissions []string `json:"permissions"`
		Expiration  *int64   `json:"expiration"`
	}
	_ = json.Unmarshal(data, &info)

	perms := strings.Join(info.Permissions, ", ")
	hasWrite := false
	for _, p := range info.Permissions {
		if p == "writeOrders" {
			hasWrite = true
		}
	}
	detail := fmt.Sprintf("key is live (type %s", info.Type)
	if perms != "" {
		detail += ", permissions: " + perms
	}
	detail += ")"
	if hasWrite {
		add("live whoami", CheckOK, detail, "")
	} else {
		add("live whoami", CheckWarn, detail+" — "+i18n.T("cannot place orders"),
			i18n.T("grant writeOrders in the developers portal if this key should trade"))
	}

	// A successful whoami can still describe a deactivated key (the spec
	// documents status activated | deactivated) — that key cannot trade, so it
	// must fail the report rather than read as healthy.
	if info.Status != "" && info.Status != "activated" {
		add("key status", CheckFail, i18n.T("key status is %q (not activated)", info.Status),
			i18n.T("reactivate the key in the developers portal"))
	}

	if info.Expiration != nil {
		nowMs := cx.Now()
		switch {
		case *info.Expiration <= nowMs:
			add("expiry", CheckFail, i18n.T("the API key has expired"), i18n.T("re-issue the key in the developers portal"))
		case *info.Expiration-nowMs <= 7*24*60*60*1000:
			add("expiry", CheckWarn, i18n.T("the API key expires within 7 days"), i18n.T("plan to re-issue it in the developers portal"))
		default:
			add("expiry", CheckOK, "not expiring soon", "")
		}
	}
}

// doctorAccountSeqCheck verifies the key's effective default accountSeq is in
// the allowedAccountSeqs list returned by /v2/currentKeyInfo. A mismatch is a
// warning: the key can still be used with an explicit allowed --account-seq.
func doctorAccountSeqCheck(km *keys.Manager, keyName string, inline bool, whoamiData json.RawMessage, add func(name, status, detail, fix string)) {
	acctSeqDefault := km.MetaDefaultAccountSeq(keyName)
	if acctSeqDefault == "" {
		acctSeqDefault = accountseq.MainString()
	}
	var info struct {
		AllowedAccountSeqs *[]int `json:"allowedAccountSeqs"`
	}
	_ = json.Unmarshal(whoamiData, &info)
	if info.AllowedAccountSeqs == nil {
		// Defensive for older/malformed fixtures: the live API always returns the
		// field, and an explicit [] is handled below as "no account permission".
		return
	}
	target, _ := strconv.Atoi(acctSeqDefault)
	for _, s := range *info.AllowedAccountSeqs {
		if s == target {
			add("default accountSeq", CheckOK, fmt.Sprintf("sub-account %s is in the key's allowed list", acctSeqDefault), "")
			return
		}
	}
	fix := i18n.T("pass an allowed --account-seq for account-scoped commands, or update the key's allowed accounts in the developers portal")
	if !inline {
		fix = i18n.T("change it: %s, pass an allowed --account-seq, or update the key's allowed accounts in the developers portal", progname.Name()+" key set-default-account-seq "+keyName+" <accountSeq>")
	}
	add("default accountSeq", CheckWarn,
		i18n.T("sub-account %s is not in the key's allowedAccountSeqs %s", acctSeqDefault, fmt.Sprint(*info.AllowedAccountSeqs)),
		fix)
}

// doctorClockCheck probes /v2/time several times and compares the server clock
// to the local one using the min-delay sample, so round-trip latency isn't
// mistaken for drift. The fix is --time-sync on (or a real NTP fix), which
// auto-corrects the signing timestamp in either direction — a clock AHEAD of the
// server can only be fixed that way (the server's future bound is a fixed 1s),
// and a clock BEHIND is corrected the same way.
// It returns clockConfirmedOK: true only when the skew was measured and is
// within tolerance. A failed measurement or any drift returns false, so the
// opt-in OS diagnosis can withhold its "force a resync" guidance when the clock
// is already known good (see doctorClockConfigCheck).
func doctorClockCheck(cx *clienv.Cmd, baseURL string, cmd *cobra.Command, add func(name, status, detail, fix string)) (clockConfirmedOK bool) {
	off, err := cx.MeasureOffset(baseURL, doctorTimeout(cmd))
	if err != nil {
		add("clock skew", CheckWarn, i18n.T("could not fetch server time to compare"), i18n.T("retry when connectivity is back"))
		return false
	}
	skew := off.OffsetMs
	if skew < 0 {
		skew = -skew
	}
	// The remedy is the same in either direction (--time-sync on or NTP); only the
	// direction word differs. offset = serverClock - localClock, so a negative
	// offset means the local clock runs ahead of the server.
	dir := i18n.T("BEHIND")
	if off.OffsetMs < 0 {
		dir = i18n.T("AHEAD of")
	}
	dirFix := i18n.T("your clock is %s the server; pass --time-sync on to auto-correct the signing timestamp, or sync your clock (NTP)", dir)
	// When the deeper OS diagnosis wasn't requested, point at it — it explains why
	// the clock drifted and names the exact fix commands for this machine.
	if !diagnoseClockRequested(cmd) {
		dirFix += i18n.T("; run `%s` to inspect this machine's time-sync service", progname.Name()+" doctor --diagnose-clock")
	}
	switch {
	case skew >= clockSkewWarnMs:
		add("clock skew", CheckWarn,
			i18n.T("local clock differs from the server by ~%dms (>= the %dms default recvWindow; rtt ~%dms)", skew, clockSkewWarnMs, off.RTTMinMs),
			dirFix)
		return false
	case skew >= clockSkewWarnMs/2:
		add("clock skew", CheckWarn,
			i18n.T("local clock differs from the server by ~%dms (approaching the %dms default recvWindow; rtt ~%dms)", skew, clockSkewWarnMs, off.RTTMinMs),
			dirFix)
		return false
	default:
		add("clock skew", CheckOK, fmt.Sprintf("within ~%dms of the server (rtt ~%dms)", skew, off.RTTMinMs), "")
		return true
	}
}

// doctorWSCheck dials the public WebSocket endpoint (best-effort, unsigned) so a
// wrong or unreachable WS host — a bad derivation or a stale per-key wsBaseUrl —
// is caught here rather than only when `monitor` later fails to connect. It is
// never fatal: the signed REST path is what gates trading, and the signed WS
// upgrade shares that REST auth path, so a passing whoami already implies WS auth
// works; this only confirms the host answers. It skips silently when no dialer is
// set (the resolved default is the real network dialer, so this guards an
// explicitly nil-injected one). wsBaseURL is the resolved endpoint; name is the
// key, for the pin-it fix.
func doctorWSCheck(cx *clienv.Cmd, rep *Report, name string, cmd *cobra.Command, add func(name, status, detail, fix string)) {
	wsBaseURL := rep.WSBaseURL
	if wsBaseURL == "" || cx.WSDial == nil {
		return
	}
	// Use the fast probe timeout so a black-holed WS host can't stall doctor for the
	// full request timeout; an explicit --timeout still overrides.
	wsTimeout := probe.DefaultTimeoutMs
	if cmd.Flags().Changed("timeout") {
		wsTimeout = doctorTimeout(cmd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(wsTimeout)*time.Millisecond)
	defer cancel()
	c := probe.WS(ctx, cx.WSDial, wsBaseURL)
	if c.Reachable {
		add("websocket", CheckOK, fmt.Sprintf("%s — %s", wsBaseURL, c.Detail), "")
		return
	}
	// The pin-it fix targets a specific key's per-key WS override; with no key
	// selected yet there is nothing to pin, so point at connectivity instead.
	fix := i18n.T("the WebSocket host `%s` would use is unreachable; check your network connection", progname.Name()+" monitor")
	if name != "" {
		fix = i18n.T("the WebSocket host `%s` would use is unreachable; if the derived URL is wrong, fix it: %s",
			progname.Name()+" monitor", progname.Name()+" key set-base-url "+name+" <rest-url> --ws-base-url <wss-url>")
	}
	// !Reachable means the dial failed at the transport level (a rejected upgrade
	// still counts as reachable, taking the OK branch above), so this is always a
	// network error — reported the same way as the live whoami's.
	rep.Checks = append(rep.Checks, networkErrorCheck("websocket",
		fmt.Sprintf("%s — %s", wsBaseURL, i18n.T("unreachable")), fix, c.Err))
}

// doctorTimeout returns the API-call timeout for doctor's live checks, honoring
// --timeout when set (defaults to the standard 15s request timeout). The value
// rides the cobra command's --timeout flag (a persistent global), the same
// source the cli binds elsewhere.
func doctorTimeout(cmd *cobra.Command) int {
	if cmd.Flags().Changed("timeout") {
		if raw, err := cmd.Flags().GetString("timeout"); err == nil {
			if ms, perr := clienv.ParseRange(raw, "--timeout", 1, 600000); perr == nil {
				return ms
			}
		}
	}
	return 15000
}

// doctorRetryBudget returns the inter-retry sleep budget for doctor's idempotent
// whoami, honoring --retry-timeout when set (defaults to the endpoint path's 5s).
// Rides the cobra command's --retry-timeout flag (a persistent global), the same
// source the cli binds elsewhere.
func doctorRetryBudget(cmd *cobra.Command) int {
	if cmd.Flags().Changed("retry-timeout") {
		if raw, err := cmd.Flags().GetString("retry-timeout"); err == nil {
			if ms, perr := clienv.ParseRange(raw, "--retry-timeout", 0, 600000); perr == nil {
				return ms
			}
		}
	}
	return 5000
}

// diagnoseClockRequested reports whether --diagnose-clock was passed, gating the
// opt-in OS time-sync diagnosis. The flag lives on the `doctor` leaf; callers
// that embed the report through a command without it (setup, the mcp doctor
// tool) get a GetBool error, which reads as false — so the OS diagnosis is
// exclusive to an explicit `doctor --diagnose-clock`.
func diagnoseClockRequested(cmd *cobra.Command) bool {
	v, err := cmd.Flags().GetBool("diagnose-clock")
	return err == nil && v
}

// finishDoctorCode emits the report and returns the given exit code without an
// error envelope — the report itself is the output. Report implements
// textout.TextFormatter (see format.go), so the emitter renders the ✓/⚠/✗
// checklist in human mode and marshals the same struct in --json mode.
func finishDoctorCode(cx *clienv.Cmd, rep Report, code int) error {
	rep.OK = code != output.ExitConfig
	if err := cx.Emit("doctor", rep); err != nil {
		return err
	}
	if code == output.ExitSuccess {
		return nil
	}
	return clienv.ExitError{Code: code}
}
