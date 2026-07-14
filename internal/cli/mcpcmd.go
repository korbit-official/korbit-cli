// Copyright (c) 2026 Korbit Inc.
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
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/korbit-official/korbit-cli/internal/agentskill"
	"github.com/korbit-official/korbit-cli/internal/callrec"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/doctorcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/keymgmtcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
	"github.com/korbit-official/korbit-cli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// The mcp command runs a Model Context Protocol stdio server exposing the
// Korbit API as tools. Like monitor, it is a long-running command and breaks
// the "one JSON document on stdout" rule — here stdout carries ONLY the
// JSON-RPC protocol, and all diagnostics go to stderr (a stray stdout write
// would corrupt the protocol stream). Stopping deliberately (client disconnect,
// Ctrl-C, SIGTERM) exits 0.
//
// The tool surface is GENERATED from the ops catalog — every endpoint operation
// becomes a tool, validated by the same shared validation engine (NormalizeValue /
// CrossValidate) the CLI and bot runtime use and executed through the same
// operation (which owns all retry/idempotency policy). The MCP layer carries no
// call policy of its own; it is one of the generated frontends over the catalog.
//
// Key model: by default ONE server signs with ONE key (resolved at launch from
// --key/KORBIT_CLI_KEY); multiple accounts = multiple named servers. With
// --multi-key a single server accepts an optional `key` per authenticated tool.

// mcpBoolFlagOrEnv returns the bool flag's value, or — when the flag was not set
// — a truthy env var, matching the flag-or-KORBIT_CLI_* idiom of the global
// toggles (see runtime.experimentalEnabled). The env form lets the .mcpb Desktop
// Extension expose --read-only / --multi-key as install-time checkboxes, which
// MCPB hosts pass through as env vars rather than conditional args.
func (rt *runtime) mcpBoolFlagOrEnv(cmd *cobra.Command, flag, env string) bool {
	if v, _ := cmd.Flags().GetBool(flag); v {
		return true
	}
	switch rt.deps.Getenv(env) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// runMCP builds the MCP server from the spec and serves it over stdio until the
// client disconnects or the process is signalled.
func (rt *runtime) runMCP(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return output.Usagef("unexpected argument %q — mcp serve takes no positional arguments; see `%s mcp serve --help`", args[0], progname.Name())
	}
	readOnly := rt.mcpBoolFlagOrEnv(cmd, "read-only", "KORBIT_CLI_MCP_READ_ONLY")
	multiKey := rt.mcpBoolFlagOrEnv(cmd, "multi-key", "KORBIT_CLI_MCP_MULTI_KEY")

	home, cfg, err := rt.LoadConfig()
	if err != nil {
		return err
	}
	km := rt.KeyManager(home, cfg)

	// Single key-selection front door: a stored key (--key / KORBIT_CLI_KEY, else
	// the default) OR inline KORBIT_CLI_API_KEY_* material — mutually exclusive.
	// sel.Name is the launch/default key (empty => the registry default, or — when
	// inline — no stored name at all); a --multi-key tool call still names its own
	// stored key, resolved separately in buildKeyAPI.
	sel, err := keys.Select(rt.key, rt.deps.Getenv)
	if err != nil {
		return err
	}
	launchKey := sel.Name

	timeoutMs := 15000
	if cmd.Flags().Changed("timeout") {
		if timeoutMs, err = parseRange(rt.timeout, "--timeout", 1, 600000); err != nil {
			return err
		}
	}
	retryBudgetMs := 5000
	if cmd.Flags().Changed("retry-timeout") {
		if retryBudgetMs, err = parseRange(rt.retryTimeout, "--retry-timeout", 0, 600000); err != nil {
			return err
		}
	}

	// The launch key (explicit --key/env, else the default) sets the base URL for
	// public tools, the plan, and the shared clock measurement — so a key pinned
	// to a non-prod host keeps its public traffic on that host instead of prod.
	// Each authenticated key still re-resolves through its own per-key override
	// (buildKeyAPI), so a --multi-key server can talk to keys on different hosts.
	launchKeyBaseURL := ""
	if !sel.Inline {
		launchKeyBaseURL = km.MetaBaseURL(launchKey)
	}
	publicBaseURL, _ := rt.resolveBaseURL(cmd, cfg, launchKeyBaseURL)
	if err := probe.ValidateBaseURL(publicBaseURL, false); err != nil {
		return err
	}

	keyNames, _ := km.Names() // best-effort; empty when no keys exist yet

	if rt.dryRun {
		return rt.Emit("mcp serve", mcpPlanDoc(readOnly, multiKey, launchKey, publicBaseURL, keyNames))
	}

	// Ctrl-C / SIGTERM stop the server cleanly (exit 0) — a deliberate stop
	// is success, not failure, the same as monitor.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// ONE shared server-clock estimate covers every signed tool call: the clock
	// offset is server-wide (key-independent), so one EXCEED_TIME_WINDOW resync
	// fixes signing for all keys at once.
	localNow := rt.deps.Now
	if localNow == nil {
		localNow = func() int64 { return time.Now().UnixMilli() }
	}
	clk := clock.New(localNow)

	// Tool handlers run concurrently and several surfaces write to stderr (ops
	// truncation notes, the signing-as-key disclosure). Program output on stderr
	// is serialized through the one locked stderr sink (rt.io.Err); stdout is the
	// JSON-RPC channel and must stay untouched. mlog is the centralized
	// operational logger (rt.log), used for both the startup diagnostics
	// (single-threaded here) and the handler-goroutine journal-warn callback; it
	// writes to rt.logW — the same sink as rt.io.Err when logs go to stderr, or the
	// --log-file file when diverted there.
	mlog := rt.logger()

	// One process clock Syncer, measured against the public base URL. The offset
	// is server-wide (key-independent), so a --multi-key key on a different host
	// shares this estimate and self-corrects via its own reactive resync.
	syncer := rt.NewClockSyncer(clk, publicBaseURL, timeoutMs, korbit.SurfaceMCP, "clock")

	// --time-sync on: measure once up front so the first signed call already signs
	// in the server's window instead of paying an EXCEED_TIME_WINDOW round-trip.
	// Under auto the estimate is corrected only reactively on a rejection.
	// Best-effort (a public-only session needs no clock); logged at Debug.
	if rt.timeSyncMode().Proactive() {
		if err := syncer.Sync(); err != nil {
			mlog.Debug(fmt.Sprintf("mcp: --time-sync clock measure failed (%v); signing with the local clock until a resync", err))
		}
	}

	// The shared journal-backed recorder — the same single-home policy the
	// endpoint commands use (callrec.DefaultPolicy: writes always journaled, reads
	// only in --debug, lazy-open). Each tool call gets its own
	// per-call recorder (the client's NewRecorder closure), so concurrent calls
	// never share recorder state. mcp is a Warn surface: a post-call journal
	// failure warns rather than failing the long-running server.
	rec := rt.LogRecorder(home, mlog)
	defer rec.Close()

	srv := &mcpServer{
		rt: rt, cmd: cmd, cfg: cfg, km: km, rec: rec, clk: clk, syncer: syncer, errW: rt.io.Err, log: mlog,
		home:          home,
		publicBaseURL: publicBaseURL, launchKey: launchKey, sel: sel, multiKey: multiKey,
		timeoutMs: timeoutMs, retryBudgetMs: retryBudgetMs,
		keyNames: keyNames, skillFS: rt.deps.SkillFS, apis: map[string]*keyAPI{},
	}

	// These startup diagnostics run single-threaded before server.Run; they log
	// through mlog (the same locked writer the handler-goroutine journal-warn
	// callback uses), so stdout stays the clean JSON-RPC channel.
	//
	// Resolve the launch key eagerly so a missing/bad key is reported on stderr
	// at startup; authenticated tools still degrade to a clear per-call error,
	// and public tools work regardless (mirrors monitor's eager-creds path).
	if ka := srv.apiForKey(launchKey); ka.err != "" {
		mlog.Warn(fmt.Sprintf("mcp: no usable signing key (%s) — authenticated tools will return an error until fixed; run `%s doctor`", ka.err, progname.Name()))
	} else {
		// Always-shown safety disclosure (which key/account this server signs
		// with), not a level-gated log and not part of stdout JSON-RPC.
		fmt.Fprintf(rt.io.Err, "korbit-cli: mcp: signing as key %q\n", ka.keyName)
	}

	server := srv.build(readOnly, multiKey)
	mlog.Info(fmt.Sprintf("mcp: serving %d tools over stdio (Ctrl-C to stop)", srv.toolCount))

	if err := server.Run(sigCtx, &mcp.StdioTransport{}); err != nil {
		if sigCtx.Err() != nil {
			return nil // deliberate stop
		}
		return err // transport/server failure -> exit 1 (untyped)
	}
	return nil
}

// mcpServer holds the shared state for one mcp invocation: the clock, journal
// recorder, key manager, and a per-key cache of built ops.APIs. Tool handlers
// run concurrently (the SDK serves requests on their own goroutines), so the
// api cache is mutex-guarded and every per-call recorder is isolated via
// ForCall.
type mcpServer struct {
	rt     *runtime
	cmd    *cobra.Command
	cfg    config.Config
	km     *keys.Manager
	rec    *callrec.Recorder
	clk    *clock.State
	syncer *clock.Syncer
	errW   io.Writer    // the one locked stderr sink (rt.io.Err) for concurrent handlers' program output
	log    *slog.Logger // the centralized operational logger (rt.log)

	home          string // CLI home, for the local key-lifecycle tools (setup)
	publicBaseURL string
	launchKey     string
	// sel is how the launch/default credential was chosen (stored name vs inline
	// KORBIT_CLI_API_KEY_* material); buildKeyAPI uses it for the default key.
	sel           keys.Selection
	multiKey      bool
	timeoutMs     int
	retryBudgetMs int
	keyNames      []string
	skillFS       fs.FS // embedded Agent Skill, source for the korbit_guide tool (may be nil)

	mu        sync.Mutex
	apis      map[string]*keyAPI // by key name ("" = the launch default)
	pub       *ops.API           // creds-less api for public tools
	toolCount int
}

// keyAPI is a built (or failed) per-key ops.API. A non-empty err means the key
// could not be resolved/used; authenticated tools using it return that as a
// tool error pointing at doctor.
type keyAPI struct {
	api      *ops.API
	keyName  string
	apiKeyID string
	err      string
}

// apiForKey returns the ops.API that signs with the named key, building and
// caching it on first use. Concurrency-safe.
func (s *mcpServer) apiForKey(name string) *keyAPI {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ka, ok := s.apis[name]; ok {
		return ka
	}
	ka := s.buildKeyAPI(name)
	s.apis[name] = ka
	return ka
}

// clientBase is the mcp server's shared client context — the fields common to
// every tool client: the MCP surface, the one shared clock, recorder, logger,
// and per-attempt timeout. Each site fills in the per-client bits (the base URL,
// and for an authenticated client the per-key creds/keyName/resync/recvWindow),
// so those shared fields are written once rather than restated at all three
// construction sites (per-key, public, dry-run preflight).
func (s *mcpServer) clientBase() clienv.ClientSpec {
	return clienv.ClientSpec{
		Surface:   korbit.SurfaceMCP,
		Clock:     s.clk,
		TimeoutMs: s.timeoutMs,
		Rec:       s.rec,
		Log:       s.log,
	}
}

// buildKeyAPI resolves the named key and assembles its L1 client + L2 ops.API,
// re-running the base-URL precedence with THAT key's per-key override (so a
// --multi-key server can legitimately talk to keys on different hosts) and
// refusing to sign over plaintext http to a non-local host.
func (s *mcpServer) buildKeyAPI(name string) *keyAPI {
	rt := s.rt
	// The default slot (name "") is the launch credential, which may be inline
	// KORBIT_CLI_API_KEY_* material; resolve it through the selection. A named key
	// (a --multi-key tool call) is always a stored key — there is no inline name —
	// so it resolves through the registry, and an inline credential has no stored
	// per-key host (keyMeta stays "").
	var resolved keys.Resolved
	var err error
	keyMeta := ""
	if name == "" {
		resolved, err = s.km.ResolveSelection(s.sel, rt.deps.Getenv)
		if !s.sel.Inline {
			keyMeta = s.km.MetaBaseURL(name)
		}
	} else {
		resolved, err = s.km.Resolve(name)
		keyMeta = s.km.MetaBaseURL(name)
	}
	if err != nil {
		return &keyAPI{err: err.Error()}
	}
	signer, err := resolved.Signer()
	if err != nil {
		return &keyAPI{err: err.Error()}
	}
	baseURL, _ := rt.resolveBaseURL(s.cmd, s.cfg, keyMeta)
	if err := probe.ValidateBaseURL(baseURL, true); err != nil {
		return &keyAPI{err: err.Error()}
	}
	spec := s.clientBase()
	spec.BaseURL = baseURL
	spec.Creds = &korbit.Credentials{APIKeyID: resolved.APIKeyID, Signer: signer}
	spec.KeyName = resolved.Name
	// --time-sync off opts out of the reactive EXCEED_TIME_WINDOW resync too.
	if rt.timeSyncMode().Reactive() {
		spec.Resync = s.syncer.Sync
	}
	client := rt.BuildClient(spec)
	api := ops.NewAPI(client)
	api.RetryBudgetMs = s.retryBudgetMs
	api.Stderr = s.errW
	api.Journal = s.rec
	return &keyAPI{api: api, keyName: resolved.Name, apiKeyID: resolved.APIKeyID}
}

// publicAPI is the creds-less ops.API for public (unauthenticated) tools, built
// once at the launch base URL.
func (s *mcpServer) publicAPI() *ops.API {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pub != nil {
		return s.pub
	}
	rt := s.rt
	spec := s.clientBase()
	spec.BaseURL = s.publicBaseURL
	client := rt.BuildClient(spec)
	api := ops.NewAPI(client)
	api.RetryBudgetMs = s.retryBudgetMs
	api.Stderr = s.errW
	api.Journal = s.rec
	s.pub = api
	return s.pub
}

// placeDryRunDoc is the order_place preview returned for a dryRun call: the
// estimated outcome and advisory warnings, or a note when the market data
// couldn't be fetched. It places nothing.
type placeDryRunDoc struct {
	DryRun        bool                 `json:"dryRun"`
	Simulation    *ops.PlaceSimulation `json:"simulation,omitempty"`
	Warnings      []ops.PlaceWarning   `json:"warnings,omitempty"`
	ChecksSkipped string               `json:"checksSkipped,omitempty"`
}

// placeDryRunResult runs the order_place customer-protection preflight for the
// dryRun tool path: it fetches PUBLIC market data (orderbook + tick size) at the
// chosen key's resolved baseURL — so a per-key/sandbox host previews against its
// own book — and returns the simulated fill + warnings. It signs nothing and
// reads no key secret (baseURL is resolved metadata-only via MetaBaseURL), so a
// preview works even if the key isn't fully set up. The reads go through the
// shared journal recorder under the "mcp" policy (journaled only in --debug; a
// post-write failure warns rather than failing the preview).
func (s *mcpServer) placeDryRunResult(ctx context.Context, chosenKey string, params map[string]string) (*mcp.CallToolResult, error) {
	rt := s.rt
	keyMeta := ""
	if chosenKey != "" || !s.sel.Inline {
		keyMeta = s.km.MetaBaseURL(chosenKey)
	}
	baseURL, _ := rt.resolveBaseURL(s.cmd, s.cfg, keyMeta)
	// Creds-less public client; PrePlaceCheck runs no ops Operation, so the
	// client's per-call closure records each read standalone (journaled only in
	// --debug). A post-write failure is non-fatal for a preview — the recorder's
	// Warn sink (set on s.rec) logs it.
	spec := s.clientBase()
	spec.BaseURL = baseURL
	client := rt.BuildClient(spec)
	sim, ws, err := ops.PrePlaceCheck(ctx, rawapi.New(client, s.log), params)
	doc := placeDryRunDoc{DryRun: true}
	if err != nil {
		doc.ChecksSkipped = fmt.Sprintf("market simulation/safety checks skipped: %v", err)
	} else {
		doc.Simulation = &sim
		doc.Warnings = ws
	}
	return toolDataResultValue(doc)
}

// build registers one tool per endpoint command (plus list_keys) on a new MCP
// server. read-only drops every non-GET command; multi-key adds the `key`
// argument to authenticated tools.
func (s *mcpServer) build(readOnly, multiKey bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "korbit-cli",
		Version: version.Version,
	}, &mcp.ServerOptions{
		Instructions: mcpInstructions(s.launchKeyLabel(), multiKey, readOnly),
	})

	count := 0
	for _, c := range commandSurface() {
		if !c.IsEndpoint() {
			continue // local builtins are not tools
		}
		if readOnly && c.Method != "GET" {
			continue
		}
		server.AddTool(&mcp.Tool{
			Name:        toolName(c),
			Description: toolDescription(c),
			InputSchema: inputSchema(c, multiKey, s.keyNames),
			Annotations: toolAnnotations(c),
		}, s.makeToolHandler(c, multiKey))
		count++
	}

	server.AddTool(&mcp.Tool{
		Name:        "list_keys",
		Description: "List the locally configured Korbit API keys (name, type, keystore backend, bound api-key id, per-key base URL, default flag, and whether this server signs with each). Read-only; returns no secrets and does not call the Korbit API. Use the whoami tool for the live account behind the signing key.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "List configured keys", ReadOnlyHint: true},
	}, s.makeListKeysHandler())
	count++

	server.AddTool(&mcp.Tool{
		Name:        botRuntimeReferenceName,
		Description: withProg(botRuntimeReferenceDesc),
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Bot runtime reference", ReadOnlyHint: true},
	}, makeBotRuntimeReferenceHandler())
	count++

	// korbit_guide surfaces the bundled Agent Skill's workflow guidance so an
	// MCP-only client (Claude Desktop via the .mcpb bundle), which never loads
	// the skill the way Claude Code does, can still reach the same safety rules
	// and per-task playbooks. Topics are discovered from the embedded skill, so
	// the enum/description can never drift from what `agent skill install` ships.
	guideTopics, _ := agentskill.GuideTopics(s.skillFS)
	server.AddTool(&mcp.Tool{
		Name:        korbitGuideName,
		Description: korbitGuideDesc(guideTopics),
		InputSchema: korbitGuideSchema(guideTopics),
		Annotations: &mcp.ToolAnnotations{Title: "Korbit workflow guide", ReadOnlyHint: true},
	}, s.makeGuideHandler())
	count++

	// Onboarding & diagnostics: setup lets a keyless first-timer get going entirely
	// in chat, and doctor verifies the result. setup changes only the local keystore
	// and additionally runs the complementary doctor once a key ends up configured
	// (so it can sign read-only diagnostics); doctor signs read-only diagnostic calls
	// with the selected key (launch key by default). Neither can move money, so they
	// are exposed in every mode — including --read-only, since a read-only user still
	// needs setup/diagnostics. setup changes local key state (not read-only); doctor
	// is a pure read.
	server.AddTool(&mcp.Tool{
		Name:        "setup",
		Description: withProg("Set up a Korbit API key for this machine without leaving chat: generates a local ED25519 keypair (private key stays in the keystore) and returns a `registrationLink` to open in a browser — the human reviews the prefilled permissions + IP allowlist and confirms with MFA, then pastes back the issued key id. Call this tool again with that id as `apiKey` to bind it and run a complementary read-only `doctor` health check in one step. Re-running is always safe (resumes an unregistered key, binds an unbound one, or reports it's already configured); if a credential is already supplied inline via the environment, it reports that instead of creating a key. Optional `name` (default \"default\") and `withTransfers` (also request deposit/withdrawal write permissions); pass `apiKey` only once the public key is registered."),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"key name (default \"default\")"},"apiKey":{"type":"string","description":"the API key id issued by the developers portal — pass it (after the public key is registered) to bind the key in one step"},"withTransfers":{"type":"boolean","description":"also request deposit/withdrawal write permissions in the registration link"}},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Set up a Korbit API key", IdempotentHint: true, DestructiveHint: new(bool)},
	}, s.makeSetupHandler())
	count++

	server.AddTool(&mcp.Tool{
		Name:        "doctor",
		Description: withProg("Health-check the key setup and report any fix: inspects the local keystore, runs a signed whoami for the selected key, warns if the effective default accountSeq is not in the key's allowedAccountSeqs, reports the public IP for the allowlist, checks clock skew, and dials the WebSocket endpoint — read-only, changes nothing. Use it to verify a freshly bound key. Optional `key` selects a specific key (default: this server's signing key). Note: a key set up and bound in this session (setup, then setup with `apiKey`) becomes usable by the tools immediately — no restart — because binding refreshes the server's key cache. The exception is a server launched with inline env credentials or an explicit --key whose name you did not set up: its signing slot is fixed at launch and still needs a restart."),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"key name to check (default: the server's signing key)"}},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Diagnose key setup", ReadOnlyHint: true},
	}, s.makeDoctorHandler())
	count++

	s.toolCount = count
	return server
}

// makeToolHandler builds the MCP handler for one endpoint command: decode the
// JSON arguments, select the signing key, validate (same rules as the CLI),
// then run through ops.Invoke. Domain failures come back as IsError tool
// results (never JSON-RPC protocol errors) so the model sees and self-corrects.
func (s *mcpServer) makeToolHandler(c surfaceCmd, multiKey bool) mcp.ToolHandler {
	needsAuth := c.Auth != nil
	isPlace := c.Key() == cmdPlace
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req)
		if err != nil {
			return toolErrorResult(err), nil
		}

		// order_place exposes a dry-run preview (the only tool that does). Pull the
		// synthetic flag out before parsing so it isn't rejected as an unknown wire
		// argument; the preview branch runs after validation, below.
		dryRun := false
		if isPlace {
			if raw, ok := args[mcpDryRunArg]; ok {
				if json.Unmarshal(raw, &dryRun) != nil {
					return toolErrorResult(output.Usagef("%q must be a boolean (true or false)", mcpDryRunArg)), nil
				}
				delete(args, mcpDryRunArg)
			}
		}

		// Key selection applies ONLY to authenticated tools. For a public tool the
		// `key` arg is never part of the schema, so it is left in args and rejected
		// by parseToolArgs as an unknown argument (the SDK does not validate; the
		// handler is the guard) — never silently accepted.
		chosenKey := s.launchKey
		if needsAuth {
			if raw, ok := args[mcpKeyArg]; ok {
				// A `key` argument is only valid under --multi-key; otherwise it is
				// rejected (never silently ignored — signing with the launch key
				// could move funds on the wrong account).
				if !multiKey {
					return toolErrorResult(output.Usagef(
						"this server signs with a single key (%s); the %q argument requires launching the server with --multi-key, or register a separate MCP server per key (see list_keys)",
						s.launchKeyLabel(), mcpKeyArg)), nil
				}
				var name string
				if json.Unmarshal(raw, &name) != nil || name == "" {
					return toolErrorResult(output.Usagef("%q must be a configured key name", mcpKeyArg)), nil
				}
				if !s.knownKey(name) {
					return toolErrorResult(output.Usagef("unknown key %q — configured keys: %s", name, strings.Join(s.knownKeyNames(), ", "))), nil
				}
				chosenKey = name
				delete(args, mcpKeyArg)
			}
		}

		params, perr := parseToolArgs(c, args, s.accountSeqDefault(chosenKey))
		if perr != nil {
			return toolErrorResult(perr), nil
		}
		if cv := c.op.Meta().CrossValidate; cv != nil {
			if verr := cv(params); verr != nil {
				return toolErrorResult(verr), nil
			}
		}

		if dryRun {
			// Preview: simulate against live public market data and return the
			// estimate + warnings WITHOUT placing. Signs nothing and needs no key
			// secret — only the chosen key's resolved baseURL (metadata) — so it
			// works even before the key is fully set up.
			return s.placeDryRunResult(ctx, chosenKey, params)
		}

		var api *ops.API
		var keyName, apiKeyID string
		if needsAuth {
			ka := s.apiForKey(chosenKey)
			if ka.err != "" {
				return toolErrorResult(output.Configf(
					"%s — run `%s doctor` in your terminal to diagnose (it reports the exact fix)", ka.err, progname.Name())), nil
			}
			api = ka.api
			keyName, apiKeyID = ka.keyName, ka.apiKeyID
		} else {
			api = s.publicAPI()
		}

		// The mcp server always reconciles a placement (Controls.SkipReconcile is
		// false): --no-reconcile is a CLI concept, not an MCP one.
		res, callErr := c.op.Run(ctx, api, ops.RunInput{
			Values:   params,
			Controls: ops.Controls{Surface: korbit.SurfaceMCP},
			KeyName:  keyName,
			APIKeyID: apiKeyID,
		})
		if callErr != nil {
			return toolErrorResult(callErr), nil
		}
		return toolDataResult(res), nil
	}
}

// accountSeqDefault returns the stored per-key accountSeq default for a stored
// key. The launch slot (chosenKey == "") falls back to the registry default only
// when this server launched with a stored-key selection; inline credentials have
// no local metadata and must not inherit an unrelated on-disk default key.
func (s *mcpServer) accountSeqDefault(chosenKey string) string {
	if chosenKey == "" && s.sel.Inline {
		return ""
	}
	return s.km.MetaDefaultAccountSeq(chosenKey)
}

// makeListKeysHandler returns the read-only list_keys tool handler. Local
// metadata only — no signing, no secrets, no API call.
func (s *mcpServer) makeListKeysHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sums, err := s.km.List()
		if err != nil {
			return toolErrorResult(err), nil
		}
		type keyInfo struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Keystore    string `json:"keystore"`
			APIKeyID    string `json:"apiKeyId,omitempty"`
			Bound       bool   `json:"bound"`
			IsDefault   bool   `json:"isDefault"`
			BaseURL     string `json:"baseUrl,omitempty"`
			IsLaunchKey bool   `json:"isLaunchKey"`
		}
		launchName := s.launchKeyLabel()
		out := make([]keyInfo, 0, len(sums))
		for _, su := range sums {
			id := ""
			if su.APIKeyID != nil {
				id = *su.APIKeyID
			}
			out = append(out, keyInfo{
				Name: su.Name, Type: su.Type, Keystore: su.Keystore, APIKeyID: id,
				Bound: su.Bound, IsDefault: su.IsDefault, BaseURL: su.BaseURL,
				IsLaunchKey: su.Name == launchName,
			})
		}
		return toolDataResultValue(map[string]any{"keys": out, "multiKey": s.multiKey})
	}
}

// makeBotRuntimeReferenceHandler returns the read-only bot_runtime_reference
// handler: it renders the monitor command's spec entry into documentation text.
// No signing, no API call — pure local documentation generated from the spec.
func makeBotRuntimeReferenceHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: botRuntimeReferenceText()}},
		}, nil
	}
}

// makeGuideHandler returns the read-only korbit_guide handler: it returns the
// requested skill guidance text (overview when `topic` is omitted, the named
// playbook otherwise). The SDK does not validate arguments against the schema,
// so the topic is re-checked here — agentskill.GuideContent rejects an unknown
// topic with a recoverable error naming the valid ones. No signing, no API call.
func (s *mcpServer) makeGuideHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req)
		if err != nil {
			return toolErrorResult(err), nil
		}
		topic := ""
		if raw, ok := args[mcpGuideTopicArg]; ok {
			if json.Unmarshal(raw, &topic) != nil {
				return toolErrorResult(output.Usagef("%q must be a string", mcpGuideTopicArg)), nil
			}
		}
		text, err := agentskill.GuideContent(s.skillFS, topic)
		if err != nil {
			return toolErrorResult(output.Usagef("%s", err.Error())), nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil
	}
}

// --- onboarding & diagnostics tools (setup, doctor) -------------------------
//
// These reuse the exact CLI logic with output redirected to a buffer, so the
// tool result is byte-for-byte the CLI's JSON document (registrationLink, next
// steps, ipAllowlist, the doctor report) with zero drift. setup acts on the
// local keystore and runs the complementary doctor once a key ends up
// configured (which signs read-only diagnostics); doctor signs read-only
// diagnostics.

// makeSetupHandler runs `setup`: with no `apiKey` it returns the registration
// link + next steps; with `apiKey` it binds the issued id (and runs the
// complementary doctor), the one-step CLI `setup --api-key` flow.
func (s *mcpServer) makeSetupHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req)
		if err != nil {
			return toolErrorResult(err), nil
		}
		// The SDK does not validate against InputSchema, so the handler is the guard:
		// reject unknown keys and type-check each field. Silently coercing a
		// malformed apiKey to absent would turn a bind into a plain resume.
		if err := rejectUnknownArgs(args, "name", "apiKey", "withTransfers"); err != nil {
			return toolErrorResult(err), nil
		}
		name, err := stringArg(args, "name")
		if err != nil {
			return toolErrorResult(err), nil
		}
		apiKey, err := stringArg(args, "apiKey")
		if err != nil {
			return toolErrorResult(err), nil
		}
		withTransfers, err := boolArg(args, "withTransfers")
		if err != nil {
			return toolErrorResult(err), nil
		}
		flags := map[string]string{}
		if name != "" {
			flags["name"] = name
		}
		if apiKey != "" {
			flags["api-key"] = apiKey
		}
		return s.runSetupTool(flags, withTransfers)
	}
}

// makeDoctorHandler runs the read-only `doctor` health check and returns its report.
func (s *mcpServer) makeDoctorHandler() mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req)
		if err != nil {
			return toolErrorResult(err), nil
		}
		if err := rejectUnknownArgs(args, "key"); err != nil {
			return toolErrorResult(err), nil
		}
		key, err := stringArg(args, "key")
		if err != nil {
			return toolErrorResult(err), nil
		}
		return s.runDoctorTool(key)
	}
}

// runSetupTool runs `setup` through the same keymgmtcmd entry the CLI uses
// (RunSetup), capturing its JSON document for the tool result. It probes the
// public IP for the registration allowlist; withTransfers prefills the
// deposit/withdrawal write scopes into the link.
func (s *mcpServer) runSetupTool(flags map[string]string, withTransfers bool) (*mcp.CallToolResult, error) {
	var buf bytes.Buffer
	kc := keymgmtcmd.KeyContext{
		IO:       output.IO{Out: &buf, Err: io.Discard},
		JSONMode: true,
		Compact:  true,
		Home:     s.home,
		KM:       s.km,
		Getenv:   s.rt.deps.Getenv,
		// Run the complementary doctor when setup ends configured, the same as the
		// cli — the report rides the setup result's `doctor` field (the human ✓/⚠/✗
		// note goes to the discarded stderr).
		Doctor: func(keyName string) *doctorcmd.Report {
			return doctorcmd.AdvisoryReport(s.rt.cmd(), s.cmd, keyName)
		},
	}
	if withTransfers {
		kc.Perms = keymgmtcmd.PermAll
	}
	// setup probes the public IP best-effort so the registration link prefills the
	// allowlist — exactly as the CLI dispatch does (root.go).
	if rep := probe.IPs(s.rt.deps.IPProbe, probe.ProdBaseURL, probe.DefaultTimeoutMs, s.rt.deps.netFamily.Networks()); rep.Any() {
		kc.IP = &rep
	}
	if runErr := keymgmtcmd.RunSetup(flags, nil, kc); runErr != nil {
		return toolErrorResult(runErr), nil
	}
	// setup mutated the local keystore through s.km; drop cached key resolutions
	// and re-read the registry so the next tool call sees the new or bound key
	// without restarting the server.
	s.refreshAfterKeyChange()
	return toolDataResultRaw(buf.Bytes())
}

// runDoctorTool runs the read-only doctor check on a shallow copy of the runtime
// whose output is captured to a buffer, then returns the report. keyOverride, when
// set, checks that named key instead of the server's signing key. The exit-code
// error doctor returns to carry its 0/4 verdict is swallowed — the report IS the result.
func (s *mcpServer) runDoctorTool(keyOverride string) (*mcp.CallToolResult, error) {
	var buf bytes.Buffer
	drt := *s.rt // shallow copy: override only output + key + mode for this call
	drt.io = output.IO{Out: &buf, Err: io.Discard}
	drt.jsonOut = true
	drt.compact = true
	if keyOverride != "" {
		drt.key = keyOverride
	}
	err := doctorcmd.Run(drt.cmd(), s.cmd, nil)
	var ee clienv.ExitError
	if err != nil && !errors.As(err, &ee) {
		return toolErrorResult(err), nil
	}
	return toolDataResultRaw(buf.Bytes())
}

// stringArg reads a JSON string argument: "" when absent, an error when present
// but not a string. The SDK does not enforce InputSchema, so wrong-typed input
// must fail loudly rather than be silently coerced to absent.
func stringArg(args map[string]json.RawMessage, name string) (string, error) {
	raw, ok := args[name]
	if !ok {
		return "", nil
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return "", output.Usagef("%q must be a string", name)
	}
	return strings.TrimSpace(v), nil
}

// boolArg reads a JSON boolean argument: false when absent, an error when present
// but not a boolean.
func boolArg(args map[string]json.RawMessage, name string) (bool, error) {
	raw, ok := args[name]
	if !ok {
		return false, nil
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return false, output.Usagef("%q must be a boolean (true or false)", name)
	}
	return v, nil
}

// rejectUnknownArgs errors if args carries any key outside allowed. The SDK does
// not enforce additionalProperties:false, so a mistyped key (e.g. "api_key")
// would otherwise be silently ignored.
func rejectUnknownArgs(args map[string]json.RawMessage, allowed ...string) error {
	for k := range args {
		if !slices.Contains(allowed, k) {
			return output.Usagef("unknown argument %q — valid: %s", k, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// launchKeyLabel is the resolved launch-key name for messages/list_keys; falls
// back to "the default key" when no name resolved.
func (s *mcpServer) launchKeyLabel() string {
	if ka := s.apiForKey(s.launchKey); ka.err == "" && ka.keyName != "" {
		return ka.keyName
	}
	if s.launchKey != "" {
		return s.launchKey
	}
	return "the default key"
}

func (s *mcpServer) knownKey(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.keyNames, name)
}

// knownKeyNames returns a snapshot of the configured key names (for error text),
// taken under the lock since refreshAfterKeyChange may rewrite the slice.
func (s *mcpServer) knownKeyNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keyNames...)
}

// refreshAfterKeyChange drops every cached per-key API and re-reads the key
// registry after an in-process key mutation (the setup tool).
// Without it a key bound mid-session would be invisible until restart: a
// pre-bind authenticated call caches its failed launch-slot ("") resolution,
// and the --multi-key key set is captured at startup. Clearing s.apis lets the
// next call rebuild against the now-bound key, and refreshing keyNames lets
// --multi-key select a freshly created key (the handler is the enforcement
// point; the schema enum advertised to the client is advisory). The launch slot
// still resolves the SAME selection it was started with, so an inline-launch
// credential or an explicit --key whose name setup didn't create/bind is not
// rescued here — that still needs a restart.
func (s *mcpServer) refreshAfterKeyChange() {
	s.km.Invalidate()
	names, _ := s.km.Names() // re-read off the now-invalidated cache, outside s.mu
	s.mu.Lock()
	s.apis = map[string]*keyAPI{}
	s.keyNames = names
	s.mu.Unlock()
}

// decodeArgs reads the tool-call arguments object into raw per-key JSON.
func decodeArgs(req *mcp.CallToolRequest) (map[string]json.RawMessage, error) {
	raw := req.Params.Arguments
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, output.Usagef("arguments must be a JSON object")
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// toolErrorResult turns a domain error into an IsError tool result (NOT a
// JSON-RPC protocol error) so the model sees it. The symbolic API code and HTTP
// status are surfaced for self-correction; key/config (exit-4) and IP-allowlist
// failures point at doctor.
func toolErrorResult(err error) *mcp.CallToolResult {
	msg := err.Error()
	var ae *output.ApiError
	if errors.As(err, &ae) {
		var parts []string
		if ae.Code != "" {
			parts = append(parts, "code="+ae.Code)
		}
		if ae.HTTPStatus != 0 {
			parts = append(parts, fmt.Sprintf("httpStatus=%d", ae.HTTPStatus))
		}
		if ae.RetryAfterSec != nil {
			parts = append(parts, fmt.Sprintf("retryAfterSec=%d", *ae.RetryAfterSec))
		}
		if len(parts) > 0 {
			msg = fmt.Sprintf("%s (%s)", msg, strings.Join(parts, ", "))
		}
		if probe.IsIPAllowlistCode(ae.Code) {
			msg += " — run `" + progname.Name() + " doctor` in your terminal; it isolates which IP/TCP family the key's allowlist accepts"
		}
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// toolDataResult wraps a successful response: the verbatim JSON as text (decimal
// strings intact, what the model reads) plus, when the response is a JSON
// object, structuredContent (the MCP spec requires structuredContent to be an
// object, so arrays/scalars ride only the text channel). A truncation note is
// appended as a second text block.
func toolDataResult(res ops.Result) *mcp.CallToolResult {
	data := res.Data
	if len(bytes.TrimSpace(data)) == 0 {
		data = json.RawMessage("null")
	}
	r := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}
	var v any
	if json.Unmarshal(data, &v) == nil {
		if _, ok := v.(map[string]any); ok {
			r.StructuredContent = v
		}
	}
	if res.Truncated && res.Note != "" {
		r.Content = append(r.Content, &mcp.TextContent{Text: "note: " + res.Note})
	}
	return r
}

// toolDataResultRaw wraps a captured JSON document (the CLI's own output for
// setup/doctor) as a tool result: the verbatim bytes as text, plus
// structuredContent when it's a JSON object (the MCP spec requires that to be an
// object). Mirrors toolDataResult, minus the truncation note (these aren't paged).
func toolDataResultRaw(data []byte) (*mcp.CallToolResult, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("null")
	}
	r := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}
	var v any
	if json.Unmarshal(data, &v) == nil {
		if _, ok := v.(map[string]any); ok {
			r.StructuredContent = v
		}
	}
	return r, nil
}

// toolDataResultValue wraps an in-process value (e.g. list_keys output) as a
// tool result with both text and structured content.
func toolDataResultValue(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: v,
	}, nil
}

// installOneLiner is the platform-appropriate CLI install command, matched to
// the OS this server process runs on (which is the user's machine): PowerShell's
// irm|iex on Windows, the curl|sh one-liner everywhere else.
func installOneLiner() string {
	if goruntime.GOOS == "windows" {
		return "`irm https://docs.korbit.co.kr/install.ps1 | iex`"
	}
	return "`curl -fsSL https://docs.korbit.co.kr/install.sh | sh`"
}

// mcpInstructions is the server-level guidance the host shows the model: what
// this server is, which key it signs with, and the load-bearing safety rules.
func mcpInstructions(launchKey string, multiKey, readOnly bool) string {
	var b strings.Builder
	b.WriteString("Korbit cryptocurrency exchange via the korbit-cli MCP server. ")
	b.WriteString("Each tool maps to a Korbit Open API v2 endpoint; arguments are keyed on the wire parameter names. ")
	b.WriteString(fmt.Sprintf("Authenticated tools sign with the key %q. ", launchKey))
	if multiKey {
		b.WriteString("This server is in --multi-key mode: pass an optional `key` (a configured key name; see list_keys) to sign a call with a different account. ")
	} else {
		b.WriteString("This server signs with a single key; to use another Korbit account, the operator must run a separate MCP server. ")
	}
	if readOnly {
		b.WriteString("This server is read-only: only market-data and account-read tools are exposed. ")
	}
	b.WriteString("Money and quantity values are decimal STRINGS (e.g. \"0.001\"), never numbers. ")
	b.WriteString("Placing an order returns the full order including its clientOrderId; to retry a placement after a failure, reuse that id. ")
	b.WriteString("For the recommended workflow and safety rules for a task — placing orders, monitoring, funding, debugging, key setup — call the korbit_guide tool (no topic for the overview and task router); consult it before placing an order or driving an unfamiliar flow. ")
	b.WriteString(fmt.Sprintf("These tools call REST endpoints directly; the CLI also offers a scriptable `%s monitor` bot runtime (streaming data plus a synchronous `ta` technical-indicator library) that this server cannot run — see the bot_runtime_reference tool. ", progname.Name()))
	b.WriteString("If no key is configured yet, onboard the user without a terminal: call the setup tool (it returns a registration link to open and confirm with MFA), then call setup again with the issued key id as `apiKey` to bind it, then the doctor tool to verify. ")
	b.WriteString(fmt.Sprintf("Some capabilities are CLI-only (live streaming via `%s monitor`, `sandbox`, log inspection, full key management) and some hosts refuse money-moving actions entirely; when a user needs one and this host can't provide it, tell them they can get the full experience by installing the CLI (%s) and using it from Claude Code. ", progname.Name(), installOneLiner()))
	b.WriteString(fmt.Sprintf("If a tool reports a key/config problem, the doctor tool diagnoses it; a fully broken launch may need `%s doctor` in a terminal and a restart.", progname.Name()))
	return b.String()
}

// mcpPlan is the `mcp serve --dry-run` document: what the server WOULD expose,
// without serving (it lists the tool names so a caller can preview the surface).
// It implements textout.TextFormatter (FormatText below) so the emitter renders
// the human plan and marshals this struct in --json mode.
type mcpPlan struct {
	DryRun     bool     `json:"dryRun"`
	Transport  string   `json:"transport"`
	ReadOnly   bool     `json:"readOnly"`
	MultiKey   bool     `json:"multiKey"`
	LaunchKey  string   `json:"launchKey,omitempty"`
	BaseURL    string   `json:"baseUrl"`
	ConfigKeys []string `json:"configuredKeys,omitempty"`
	ToolCount  int      `json:"toolCount"`
	Tools      []string `json:"tools"`
}

func mcpPlanDoc(readOnly, multiKey bool, launchKey, baseURL string, keyNames []string) mcpPlan {
	var tools []string
	for _, c := range commandSurface() {
		if !c.IsEndpoint() {
			continue
		}
		if readOnly && c.Method != "GET" {
			continue
		}
		tools = append(tools, toolName(c))
	}
	tools = append(tools, "list_keys", botRuntimeReferenceName, korbitGuideName, "setup", "doctor")
	return mcpPlan{
		DryRun: true, Transport: "stdio", ReadOnly: readOnly, MultiKey: multiKey,
		LaunchKey: launchKey, BaseURL: baseURL, ConfigKeys: keyNames,
		ToolCount: len(tools), Tools: tools,
	}
}

// FormatText renders the mcp-serve plan for human output.
func (p mcpPlan) FormatText(w io.Writer) {
	fmt.Fprint(w, "DRY RUN — mcp serve plan (server not started)\n")
	fmt.Fprint(w, textout.IndentLines(textout.KVBlock([][2]string{
		{"transport", p.Transport},
		{"base URL", p.BaseURL},
		{"read-only", textout.YesNo(p.ReadOnly)},
		{"multi-key", textout.YesNo(p.MultiKey)},
		{"launch key", p.LaunchKey},
		{"tools", fmt.Sprintf("%d", p.ToolCount)},
	}), "  "))
	if len(p.Tools) > 0 {
		fmt.Fprint(w, "\n  "+strings.Join(p.Tools, ", "))
	}
}
