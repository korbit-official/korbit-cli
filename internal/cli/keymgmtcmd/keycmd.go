// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keymgmtcmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/cli/clienv"
	"github.com/digitalx-official/digitalx-cli/internal/cli/doctorcmd"
	"github.com/digitalx-official/digitalx-cli/internal/cli/probe"
	"github.com/digitalx-official/digitalx-cli/internal/cli/textout"
	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/envalias"
	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/spec"
)

// portalCreatePath is the create-form route that accepts prefill query params
// (public_key, permissions, label, whitelist).
const portalCreatePath = "/manage/create"

// permTrading is the default permission bitmask prefilled into the registration
// deep link: all read scopes plus place-orders —
// readBalances(1)|readOrders(2)|writeOrders(4)|readDeposits(8)|readWithdrawals(32)
// = 47. The deposit/withdrawal *write* bits (writeDeposits=16, writeWithdrawals=64)
// are off by default — a plain trading key never requests withdrawal permission.
// `setup`/`key add --with-transfers` opts those bits in (PermAll) for
// users who need `deposit generate`, `withdraw request/cancel`, or the `krw` push
// commands. The human still reviews and adjusts the boxes on the portal before
// confirming.
const (
	permTrading   = 1 | 2 | 4 | 8 | 32          // 47
	permTransfers = 16 | 64                     // writeDeposits | writeWithdrawals
	PermAll       = permTrading | permTransfers // 127
)

// registrationLabel is the default key label prefilled into the registration
// deep link. It namespaces the key in the developers portal so a CLI-issued key
// is recognizable at a glance among any others on the account.
func registrationLabel(name string) string {
	return "dgx-cli: " + name
}

// registrationLink builds the developers-portal create-form deep link that
// prefills ED25519 + the public key (locked there), permissions, label, and —
// when known — the IP allowlist, so the human finishes registration with one
// review and one MFA confirm. It returns "" when the public key can't be encoded
// (the caller then falls back to manual PEM registration).
func registrationLink(portalURL, publicPEM, label string, allowlist []string, perms int) string {
	b64, err := apiclient.PublicSPKIBase64URL(publicPEM)
	if err != nil {
		return ""
	}
	u, err := url.Parse(portalURL + portalCreatePath)
	if err != nil {
		return ""
	}
	q := url.Values{}
	q.Set("public_key", b64)
	q.Set("permissions", strconv.Itoa(perms))
	if label != "" {
		q.Set("label", label)
	}
	if len(allowlist) > 0 {
		q.Set("whitelist", strings.Join(allowlist, ","))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// KeyContext carries the dependencies the builtin key/setup commands need. It is
// exported so both cli root dispatch and the mcp server can drive the exact CLI
// key-creation path: mcp substitutes a buffer-backed IO to capture the JSON
// document for its tool result (see RunKeyCommand).
type KeyContext struct {
	IO       output.IO
	JSONMode bool
	Compact  bool
	Home     string
	KM       *keys.Manager
	// IP, when set (by `setup`), carries the probed public IP(s) so the
	// guidance can name the exact allowlist entries to register.
	IP *probe.Report
	// Perms is the permission bitmask to prefill into the registration deep link.
	// Zero means "unset" — treated as permTrading (the safe default).
	Perms int
	// BaseURL / BaseURLSet carry an explicit --base-url. `setup` (only when it
	// first creates the key) and `key add` persist it as the new key's default
	// endpoint, mirroring `key set-base-url`. Unset leaves the key on the default
	// host.
	BaseURL    string
	BaseURLSet bool
	// WSBaseURL / WSBaseURLSet carry an explicit --ws-base-url for `key
	// set-base-url` and the create commands: when set, it is persisted verbatim;
	// when unset, the WebSocket URL is derived from the REST base URL.
	WSBaseURL    string
	WSBaseURLSet bool
	// Verify, when set, smoke-tests the REST + WebSocket endpoints after `key
	// set-base-url` stores them. Nil when the check is skipped (--no-verify) or
	// not applicable to the command.
	Verify func(restURL, wsBaseURL string) probe.EndpointVerification
	// Getenv reads the process environment; `setup` uses it to detect an inline
	// credential (DIGITALX_CLI_API_KEY_*). May be nil (the check is then skipped).
	Getenv func(string) string
	// Doctor, when set, runs the read-only health check for a just-configured
	// BOUND key and returns its report. It is advisory: setup embeds the report in
	// its result and surfaces problems as warnings, but a failing check NEVER
	// changes setup's exit code. Nil when the check is skipped.
	Doctor func(keyName string) *doctorcmd.Report
	// VerifyAPIKey, when set, signs a read-only whoami for the named key using a
	// CANDIDATE api key id — with no binding persisted — to confirm the id is
	// registered and the key can sign with it. Both setup lanes call it before
	// binding. wait selects the tolerance: false (manual paste) is one-shot — any
	// rejection (KEY_NOT_FOUND, signature mismatch) is returned so the prompt can
	// retry without ever writing a bad id; true (auto-claim) rides out the
	// just-registered KEY_NOT_FOUND window within a short budget and is best-effort
	// (the caller treats a failure as soft and binds anyway). ctx lets a session
	// cancel abort the wait. nil means an id is bound without checking.
	VerifyAPIKey func(ctx context.Context, name, apiKeyID string, wait bool) error
	// RunInteractive, when set, takes over the awaiting-registration outcome of
	// `setup` on a TTY: instead of printing the registration link and exiting, it
	// shows intro (the lead-in prose + the public key when there is no link) and
	// prompts for the issued key id, then calls submit, which binds the key and runs
	// the health check. registrationURL is the portal deep link (empty when none
	// could be built); the UI renders it as its own width-wrapped, clickable block
	// and offers copy/QR shortcuts for it, so it is passed separately rather than
	// baked into intro. submit returns the lines to display, done=true when setup is
	// complete, or a retryable error to show while keeping the prompt open. The UI
	// renders on stderr; the final result document is still emitted to stdout after
	// it closes. canceled=true means the user aborted with Ctrl-C: the caller emits
	// nothing (matching the non-interactive SIGINT abort). Nil disables interactive
	// setup (a pipe, --json, the MCP server, --no-interactive), and setup falls back
	// to print-and-exit.
	//
	// autoClaim, when non-nil, runs concurrently with the paste prompt: the runner
	// starts it, shows its status, and lets it complete setup automatically when it
	// detects the registered key (see ClaimUpdate). nil leaves manual paste as the
	// only path.
	RunInteractive func(intro []string, registrationURL string, submit func(token string) (lines []string, done bool, err error), autoClaim func(ctx context.Context) <-chan ClaimUpdate) (canceled bool, err error)

	// PollClaim, when set, runs the background auto-claim poll for the freshly
	// generated key `name`, signing the keyless GET /v2/keys/claim with the key's
	// not-yet-bound private key. It calls onStatus with a short live status for
	// transient states (e.g. a brief back-off) and returns one of:
	//   - (id, "", nil)       the key was claimed — the caller binds it
	//   - ("", code, nil)     polling stopped without a claim; code is the API error
	//                         code (e.g. KEY_CLAIM_CONFLICT, IP_NOT_ALLOWED,
	//                         KEY_DEACTIVATED), "" for a generic/malformed stop. The
	//                         caller renders the user notice (claimStopNotice).
	//   - ("", "", err)       ctx was canceled (session ending) or an unexpected fault
	// It journals nothing and logs through the standard logger. nil disables
	// auto-claim (setup falls back to manual paste only).
	PollClaim func(ctx context.Context, name string, onStatus func(status string)) (apiKeyID, stopCode string, err error)

	// WaitForClaim enables the non-interactive headless auto-claim path (`setup
	// --wait`): instead of printing the registration link and exiting, setup prints
	// the link, then blocks polling PollClaim until the key is claimed (or
	// WaitTimeout elapses) and binds + health-checks it automatically — no prompt,
	// no manual paste. It reuses the same PollClaim / VerifyAPIKey / Doctor seams as
	// the interactive auto-claim and works with or without a TTY (so an agent driving
	// the CLI never has to copy the issued id back). Ignored unless PollClaim is set.
	WaitForClaim bool
	// WaitTimeout bounds the headless wait; zero means wait indefinitely. Only
	// consulted when WaitForClaim is set.
	WaitTimeout time.Duration
}

// ClaimUpdate is one event from the background auto-claim poll, surfaced by the
// interactive runner; it mirrors setupui.ClaimUpdate so this package stays
// independent of the UI layer (root.go maps between the two). A non-terminal
// update carries only Status. A terminal update sets exactly one of Done
// (success: Result holds the lines to show before the session ends) or Stop
// (polling stopped without a claim; the prompt stays open and Status says why).
// Prefill (Stop only) is the claimed api key id to drop into the empty input, so
// a fall-back after the id was already retrieved doesn't make the user re-find it.
type ClaimUpdate struct {
	Status  string
	Result  []string
	Done    bool
	Stop    bool
	Prefill string
}

// claimStopNotice maps a claim (or post-claim whoami) rejection code to the line
// shown when auto-claim stops. It states only the REASON, lane-neutral: the
// interactive prompt is right below it (its own label says what to paste) and the
// headless --wait path follows it with the resume document, so neither needs an
// action suffix that would be wrong in the other. The IP case names this machine's
// probed public IP(s) (the same set `doctor` and the registration guidance use,
// honoring --family), not the server's text. An empty code yields a generic line.
func (ctx KeyContext) claimStopNotice(code string) string {
	if probe.IsIPAllowlistCode(code) {
		return ctx.ipAllowlistStopNotice()
	}
	switch code {
	case "KEY_CLAIM_CONFLICT":
		return i18n.T("Couldn't auto-detect your key.")
	case "KEY_DEACTIVATED":
		return i18n.T("This API key has been deactivated — auto-claim can't use it.")
	case "KEY_EXPIRED":
		return i18n.T("This API key has expired.")
	case "KEY_NOT_FOUND":
		return i18n.T("The API key wasn't found.")
	}
	if code != "" {
		return i18n.T("Automatic detection failed (%s).", code)
	}
	return i18n.T("Automatic detection isn't available.")
}

// ipAllowlistStopNotice explains an IP-allowlist rejection using the probed
// public IP(s), mirroring doctor's "Digital X sees you from:" guidance so the user
// knows exactly which entries to add. Falls back to a generic line when the IPs
// couldn't be determined.
func (ctx KeyContext) ipAllowlistStopNotice() string {
	var ips []string
	if ctx.IP != nil {
		ips = ctx.IP.Allowlist
	}
	if len(ips) == 0 {
		return i18n.T("Auto-claim was blocked by the key's IP allowlist. Add this machine's public IP to the key's allowlist.")
	}
	return i18n.T("Auto-claim was blocked by the key's IP allowlist. Digital X sees you from: %s — add these to the key's allowlist.", strings.Join(ips, ", "))
}

// hardClaimReject reports whether a post-claim whoami rejection is a permanent
// key-level failure (deactivated/expired/IP) the auto-bind must not paper over —
// it stops and falls back to manual. Transient/cache states (e.g. KEY_NOT_FOUND
// during activation) are handled by the tolerant wait, not here.
func hardClaimReject(code string) bool {
	return code == "KEY_DEACTIVATED" || code == "KEY_EXPIRED" || probe.IsIPAllowlistCode(code)
}

// linkPerms returns the bitmask to prefill, defaulting an unset (zero) value to
// permTrading so callers that don't set ctx.Perms keep the safe default.
func (ctx KeyContext) linkPerms() int {
	if ctx.Perms == 0 {
		return permTrading
	}
	return ctx.Perms
}

// portalURL is the developers-portal base for the registration link and guidance,
// normally probe.PortalURL. DIGITALX_CLI_PORTAL_BASE_URL overrides it (internal
// testing) — deliberately undocumented.
func (ctx KeyContext) portalURL() string {
	if ctx.Getenv != nil {
		if v := strings.TrimSpace(envalias.Lookup(ctx.Getenv, "DIGITALX_CLI_PORTAL_BASE_URL")); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return probe.PortalURL
}

// emit routes a key/setup command result to stdout in the active mode. Human
// mode renders a concise status: each result type self-renders via its
// FormatText (keyout.go, textout.TextFormatter); the step-by-step guidance and
// public key ride stderr. --json emits the full result object. This is the
// self-contained twin of cli's emitMode for the key path — every key/setup
// result is a TextFormatter, so it needs neither cli's endpointFormatters table
// nor a command-key lookup.
func (ctx KeyContext) emit(value any) error {
	if ctx.JSONMode {
		return ctx.IO.EmitJSON(value, ctx.Compact)
	}
	if tf, ok := value.(textout.TextFormatter); ok {
		var b strings.Builder
		tf.FormatText(&b)
		return ctx.IO.EmitText(b.String())
	}
	// Defensive: a result without a formatter falls back to pretty JSON (the same
	// bytes as --json without --compact), matching cli's emitMode last resort.
	return ctx.IO.EmitJSON(value, false)
}

func nextSteps(portalURL, name, link string, allowlist []string, perms int) []string {
	prog := progname.Name()
	finish := prog + " setup --api-key <KEY_ID>"
	if name != defaultKeyName {
		finish = prog + " setup --name " + name + " --api-key <KEY_ID>"
	}
	var register string
	if link != "" {
		register = i18n.T("Open this link to register the key, then review the permissions and IP allowlist and confirm with MFA:") + "\n     " + link
	} else {
		scopes := i18n.T("read + orders for trading, not withdrawal")
		if perms&permTransfers != 0 {
			scopes = i18n.T("read + orders + deposit/withdrawal")
		}
		if len(allowlist) > 0 {
			register = i18n.T("Register the public key at %s as a new API key (type: ED25519). Grant only the permissions you need (%s) and set an IP allowlist to: %s.", portalURL, scopes, strings.Join(allowlist, ", "))
		} else {
			register = i18n.T("Register the public key at %s as a new API key (type: ED25519). Grant only the permissions you need (%s) and set an IP allowlist.", portalURL, scopes)
		}
	}
	return []string{
		register,
		i18n.T("Finish setup with the issued key id — this binds it and runs a health check: %s", finish),
	}
}

// registrationLinkAndSteps computes the data a fresh/unbound key's result carries:
// the portal deep link (empty when one can't be built) and the numbered next steps
// (which lead with that link when present). The human guidance block is rendered
// from these on stdout by writeRegistrationGuidance (keyout.go) — it is the
// command's result, not stderr narration.
func registrationLinkAndSteps(portalURL, name, publicPEM string, allowlist []string, perms int) (link string, steps []string) {
	link = registrationLink(portalURL, publicPEM, registrationLabel(name), allowlist, perms)
	steps = nextSteps(portalURL, name, link, allowlist, perms)
	return link, steps
}

func numbered(steps []string) string {
	var b strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	return strings.TrimRight(b.String(), "\n")
}

// newKeyAwaiting returns the closure that emits the freshly-generated-key result
// document (newKeyResult) to stdout. The whole guidance — link, numbered steps,
// public key — is part of that document and renders on stdout via its FormatText;
// there is no separate stderr guidance. The awaiting paths (print-and-exit,
// interactive "finish later", `setup --wait`) call it when they reach their
// terminal state.
func newKeyAwaiting(ctx KeyContext, r keys.NewKey) func() error {
	var allowlist []string
	if ctx.IP != nil {
		allowlist = ctx.IP.Allowlist
	}
	link, steps := registrationLinkAndSteps(ctx.portalURL(), r.Name, r.PublicKey, allowlist, ctx.linkPerms())
	return func() error {
		return ctx.emit(newKeyResult{
			Name: r.Name, Type: r.Type, Keystore: r.Keystore, PublicKey: r.PublicKey,
			IsDefault: r.IsDefault, Bound: false, Status: "created",
			RegistrationURL: ctx.portalURL(), RegistrationLink: link, IPAllowlist: ctx.IP, Next: steps,
		})
	}
}

func emitNewKey(ctx KeyContext, r keys.NewKey) error {
	return newKeyAwaiting(ctx, r)()
}

// emitNewKeyBound handles `key add ... --api-key`: the key was created (typically
// imported) AND bound in one step, so the public key is already registered and the
// only remaining step is verification. Unlike emitNewKey it prints no registration
// link and does not dump the public key — there is nothing left to register.
func emitNewKeyBound(ctx KeyContext, r keys.NewKey, apiKey string) error {
	prog := progname.Name()
	verify := prog + " whoami"
	// `whoami` with no --key signs as the default key, so only name a key that
	// isn't the default (a bare `whoami` already targets it).
	if !r.IsDefault {
		verify += " --key " + r.Name
	}
	return ctx.emit(boundKeyResult{
		Name: r.Name, Type: r.Type, Keystore: r.Keystore, PublicKey: r.PublicKey,
		APIKeyID: apiKey, IsDefault: r.IsDefault, Bound: true, Status: "created",
		Next: []string{"Verify it works: " + verify},
	})
}

// checkBaseURLFlags validates --base-url/--ws-base-url before a key is created,
// so an invalid value fails up front rather than leaving a created-but-unpinned
// key when SetBaseURL would reject it afterward. Callers run it before KM.Add.
func checkBaseURLFlags(ctx KeyContext) error {
	if ctx.BaseURLSet {
		if err := keys.ValidateBaseURL(ctx.BaseURL); err != nil {
			return err
		}
	}
	if ctx.WSBaseURLSet && ctx.WSBaseURL != "" {
		if err := keys.ValidateWSBaseURL(ctx.WSBaseURL); err != nil {
			return err
		}
		// A WS pin needs a REST host to anchor to (the key's host comes from the
		// REST base URL; the WS URL is derived from or pinned alongside it). Reject
		// the orphan up front — before any key is created — rather than silently
		// dropping it later. The host may come from --base-url or DIGITALX_CLI_BASE_URL
		// (the same precedence applyKeyBaseURL resolves).
		if !ctx.BaseURLSet {
			envBase := ""
			if ctx.Getenv != nil {
				envBase = strings.TrimSpace(envalias.Lookup(ctx.Getenv, "DIGITALX_CLI_BASE_URL"))
			}
			if envBase == "" {
				return output.Usagef("--ws-base-url needs --base-url (or DIGITALX_CLI_BASE_URL) to anchor the key's host")
			}
		}
	}
	return nil
}

// applyKeyBaseURL persists the REST base URL (and WebSocket URL, else derived from
// it) as the default endpoint of a just-created key, mirroring `key set-base-url`.
// The host comes from --base-url, or — when the flag is absent — the
// DIGITALX_CLI_BASE_URL env override, so a key created against an explicit host
// (whether by `setup` first-creating it or by `key add`) is pinned to that host and
// keeps working once the env var is gone. A no-op when neither is set. The flag,
// when present, already won via keyContext. checkBaseURLFlags has validated the
// flag URLs; an env URL is the one already used for this command's calls.
func applyKeyBaseURL(ctx KeyContext, name string) error {
	baseURL, baseSet := ctx.BaseURL, ctx.BaseURLSet
	wsURL, wsSet := ctx.WSBaseURL, ctx.WSBaseURLSet
	if !baseSet && ctx.Getenv != nil {
		if envBase := strings.TrimSpace(envalias.Lookup(ctx.Getenv, "DIGITALX_CLI_BASE_URL")); envBase != "" {
			baseURL, baseSet = strings.TrimRight(envBase, "/"), true
			if envWS := strings.TrimSpace(envalias.Lookup(ctx.Getenv, "DIGITALX_CLI_WS_BASE_URL")); envWS != "" {
				wsURL, wsSet = strings.TrimRight(envWS, "/"), true
			}
		}
	}
	if !baseSet {
		// No host to pin. checkBaseURLFlags has already rejected an orphan
		// --ws-base-url (one without a REST host) as a usage error before any key
		// was created, so reaching here means neither was set: nothing to do.
		return nil
	}
	if !wsSet {
		wsURL = probe.DeriveWSBaseURL(baseURL)
	}
	if err := ctx.KM.SetBaseURL(name, baseURL, wsURL); err != nil {
		return err
	}
	// The pin is part of the happy path — it is reflected in the key's stored base
	// URL (shown by `key list` / `key show`), not narrated to stderr.
	return nil
}

// keystoreFlag validates the --keystore flag of setup / key add: an unknown
// backend name is a usage error here (the user typed it), while availability is
// probed inside Manager.Add (it applies to the config default too).
func keystoreFlag(flags map[string]string) (string, error) {
	ks := flags["keystore"]
	if ks != "" && !config.ValidBackend(ks) {
		return "", output.Usagef("--keystore must be one of: %s", strings.Join(config.Backends, ", "))
	}
	return ks, nil
}

// readSecretFile reads an HMAC-SHA256 shared secret from a file, or from stdin
// when path is "-". Trailing whitespace (a stray editor newline) is trimmed so a
// secret saved to a file signs identically to one typed without it — a Digital X
// secret is a whitespace-free token. The secret is never echoed.
func readSecretFile(path string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", output.Usagef("cannot read --secret-file: %v", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", output.Usagef("--secret-file is empty")
	}
	return secret, nil
}

// requireName returns the single positional key name, or a UsageError.
func requireName(positionals []string, hint string) (string, error) {
	if len(positionals) == 0 {
		return "", output.Usagef("key name is required, e.g. `%s`", hint)
	}
	if len(positionals) > 1 {
		return "", output.Usagef("unexpected argument %q", positionals[1])
	}
	return positionals[0], nil
}

// RunKeyCommand dispatches the builtin `key *` subcommands. flags holds the set
// string-flag values; output goes to stdout, diagnostics to stderr. It is
// exported so the mcp server can drive the same path with a buffer-backed
// KeyContext and capture the emitted JSON document. It handles only `key *`
// (`setup` has its own entry, RunSetup).
func RunKeyCommand(c *spec.Command, flags map[string]string, positionals []string, ctx KeyContext) error {
	if c.ID[0] != "key" {
		return fmt.Errorf("internal: RunKeyCommand received non-key command %q (setup uses RunSetup)", c.Key())
	}
	switch c.Key() {
	case "key add":
		name, err := requireName(positionals, progname.Name()+" key add trading-bot")
		if err != nil {
			return err
		}
		backend, err := keystoreFlag(flags)
		if err != nil {
			return err
		}
		if err := checkBaseURLFlags(ctx); err != nil {
			return err
		}
		apiKey := strings.TrimSpace(flags["api-key"])
		keyType := flags["type"]
		if keyType == "" {
			keyType = keys.TypeEd25519
		}
		switch keyType {
		case keys.TypeHMACSHA256:
			// HMAC keys carry a Digital X-issued shared secret and are born bound (no
			// keypair, no public key). --from-pem-file is an ED25519-only path.
			if flags["from-pem-file"] != "" {
				return output.Usagef("--from-pem-file imports an ED25519 PEM; an hmac-sha256 key takes its secret via --secret-file")
			}
			if apiKey == "" {
				return output.Usagef("--type hmac-sha256 requires --api-key <KEY_ID> (Digital X issues the id and secret together)")
			}
			if strings.HasPrefix(apiKey, keys.SandboxAPIKeyPrefix) {
				if err := keys.AssertSandboxKeyName(name); err != nil {
					return err
				}
			}
			secretPath := flags["secret-file"]
			if secretPath == "" {
				return output.Usagef("--type hmac-sha256 requires --secret-file <file> (the shared secret; '-' reads stdin)")
			}
			secret, err := readSecretFile(secretPath)
			if err != nil {
				return err
			}
			r, err := ctx.KM.AddHMAC(name, secret, apiKey, backend)
			if err != nil {
				return err
			}
			if err := applyKeyBaseURL(ctx, name); err != nil {
				return err
			}
			return emitNewKeyBound(ctx, r, apiKey)

		case keys.TypeEd25519:
			if flags["secret-file"] != "" {
				return output.Usagef("--secret-file is for --type hmac-sha256; an ED25519 key imports a PEM with --from-pem-file")
			}
			var privatePEM string
			if path := flags["from-pem-file"]; path != "" {
				raw, err := os.ReadFile(path)
				if err != nil {
					return output.Usagef("cannot read --from-pem-file: %v", err)
				}
				privatePEM = string(raw)
			}
			// A sandbox api-key id would make this a sandbox key, whose name must
			// advertise the sandbox. Check before Add (this path creates then binds in
			// two steps) so a non-conforming name fails cleanly instead of leaving an
			// unbound key behind when the bind below is refused.
			if strings.HasPrefix(apiKey, keys.SandboxAPIKeyPrefix) {
				if err := keys.AssertSandboxKeyName(name); err != nil {
					return err
				}
			}
			r, err := ctx.KM.Add(name, privatePEM, backend)
			if err != nil {
				return err
			}
			if err := applyKeyBaseURL(ctx, name); err != nil {
				return err
			}
			// --api-key binds the portal-issued id at creation, so the import case
			// (an already-registered key) is one command. The key exists now, so a
			// bind failure surfaces to the user with the key already created.
			if apiKey != "" {
				if err := ctx.KM.Bind(name, apiKey); err != nil {
					return err
				}
				return emitNewKeyBound(ctx, r, apiKey)
			}
			return emitNewKey(ctx, r)

		default:
			return output.Usagef("--type must be one of: %s, %s", keys.TypeEd25519, keys.TypeHMACSHA256)
		}

	case "key rename":
		if len(positionals) < 2 {
			return output.Usagef("two names are required, e.g. `%s key rename old-bot new-bot`", progname.Name())
		}
		if len(positionals) > 2 {
			return output.Usagef("unexpected argument %q", positionals[2])
		}
		oldName, newName := positionals[0], positionals[1]
		result, err := ctx.KM.Rename(oldName, newName)
		if err != nil {
			return err
		}
		return ctx.emit(renameView{result})

	case "key bind":
		name, err := requireName(positionals, progname.Name()+" key bind trading-bot --api-key <KEY_ID>")
		if err != nil {
			return err
		}
		apiKey := strings.TrimSpace(flags["api-key"])
		if apiKey == "" {
			return output.Usagef("--api-key <KEY_ID> is required (the id issued by the developers portal)")
		}
		if err := ctx.KM.Bind(name, apiKey); err != nil {
			return err
		}
		return ctx.emit(keyBindResult{Name: name, APIKeyID: apiKey, Bound: true})

	case "key list":
		if err := clienv.RequireNoArgs(positionals); err != nil {
			return err
		}
		def, err := ctx.KM.DefaultKeyName()
		if err != nil {
			return err
		}
		list, err := ctx.KM.List()
		if err != nil {
			return err
		}
		var defPtr *string
		if def != "" {
			defPtr = &def
		}
		return ctx.emit(keyListResult{
			Home: ctx.Home, DefaultKeystore: ctx.KM.DefaultBackend(), DefaultKey: defPtr, Keys: list,
		})

	case "key show":
		name, err := requireName(positionals, progname.Name()+" key show trading-bot")
		if err != nil {
			return err
		}
		s, err := ctx.KM.Show(name)
		if err != nil {
			return err
		}
		return ctx.emit(showView{s})

	case "key use":
		name, err := requireName(positionals, progname.Name()+" key use trading-bot")
		if err != nil {
			return err
		}
		if err := ctx.KM.Use(name); err != nil {
			return err
		}
		return ctx.emit(keyUseResult{DefaultKey: name})

	case "key remove":
		name, err := requireName(positionals, progname.Name()+" key remove old-bot")
		if err != nil {
			return err
		}
		result, err := ctx.KM.Remove(name, flags["force"] == "true")
		if err != nil {
			return err
		}
		var next []string
		if result.DefaultKey == nil {
			if list, _ := ctx.KM.List(); len(list) > 0 {
				next = []string{fmt.Sprintf("Pick a new default key: %s key use <name>", progname.Name())}
			}
		}
		return ctx.emit(removeView{RemoveResult: result, Next: next})

	case "key set-base-url":
		if len(positionals) == 0 {
			return output.Usagef("key name is required, e.g. `%s key set-base-url trading-bot https://api.digitalx.miraeasset.com`", progname.Name())
		}
		if len(positionals) > 2 {
			return output.Usagef("unexpected argument %q", positionals[2])
		}
		name := positionals[0]
		clear := flags["clear"] == "true"
		var url string
		if len(positionals) == 2 {
			url = positionals[1]
		}
		switch {
		case clear && url != "":
			return output.Usagef("pass either a base URL or --clear, not both")
		case !clear && url == "":
			return output.Usagef("provide a base URL (e.g. https://api.digitalx.miraeasset.com) or --clear to revert this key to the default")
		case clear:
			if err := ctx.KM.ClearBaseURL(name); err != nil {
				return err
			}
			return ctx.emit(setBaseURLResult{Name: name})
		default:
			// The WebSocket URL is the explicit --ws-base-url when given, else
			// derived from the REST URL so the persisted pair stays consistent.
			wsURL := ctx.WSBaseURL
			if !ctx.WSBaseURLSet {
				wsURL = probe.DeriveWSBaseURL(url)
			}
			if err := ctx.KM.SetBaseURL(name, url, wsURL); err != nil {
				return err
			}
			s, err := ctx.KM.Show(name)
			if err != nil {
				return err
			}
			// Smoke-test the endpoints (best-effort, never fatal): the URLs are
			// already stored, so an unreachable endpoint is reported as guidance
			// and the command still succeeds.
			var verification *probe.EndpointVerification
			if ctx.Verify != nil {
				v := ctx.Verify(s.BaseURL, s.WSBaseURL)
				verification = &v
			}
			return ctx.emit(setBaseURLResult{
				Name: name, BaseURL: s.BaseURL, WSBaseURL: s.WSBaseURL, Verification: verification,
			})
		}

	case "key set-default-account-seq":
		if len(positionals) == 0 {
			return output.Usagef("key name is required, e.g. `%s key set-default-account-seq trading-bot 2`", progname.Name())
		}
		if len(positionals) > 2 {
			return output.Usagef("unexpected argument %q", positionals[2])
		}
		name := positionals[0]
		clear := flags["clear"] == "true"
		var acctSeqStr string
		if len(positionals) == 2 {
			acctSeqStr = positionals[1]
		}
		switch {
		case clear && acctSeqStr != "":
			return output.Usagef("pass either an accountSeq or --clear, not both")
		case !clear && acctSeqStr == "":
			return output.Usagef("provide an accountSeq (e.g. 2) or --clear to revert to the main account")
		case clear:
			if err := ctx.KM.SetDefaultAccountSeq(name, 0); err != nil {
				return err
			}
			return ctx.emit(setDefaultAccountSeqResult{Name: name})
		default:
			accountSeq, err := strconv.Atoi(acctSeqStr)
			if err != nil || accountSeq < 1 {
				return output.Usagef("accountSeq must be an integer >= 1 (got %q)", acctSeqStr)
			}
			if err := ctx.KM.SetDefaultAccountSeq(name, accountSeq); err != nil {
				return err
			}
			return ctx.emit(setDefaultAccountSeqResult{Name: name, DefaultAccountSeq: &accountSeq})
		}
	}
	return fmt.Errorf("internal: unhandled key command %q", c.Key())
}
