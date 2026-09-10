// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package cli wires the declarative spec into a cobra command tree, parses and
// validates input, and enforces the output contract: one JSON document on
// stdout, diagnostics and one structured error on stderr, and the documented
// exit codes. cobra handles parsing and dispatch; help, the error envelope, and
// exit codes are owned here so the agent contract holds.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/cli/agentskillcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
	"github.com/korbit-official/korbit-cli/internal/cli/doctorcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/keymgmtcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/monitorcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/cli/sandboxcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/selfcmd"
	"github.com/korbit-official/korbit-cli/internal/cli/setupui"
	"github.com/korbit-official/korbit-cli/internal/cli/tuicmd"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/netbind"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/spec"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui"
	"github.com/korbit-official/korbit-cli/internal/version"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// Deps are the injectable dependencies, so the whole CLI runs in-process under
// test with buffers, a stub HTTP client, a temp home, and a fixed clock.
type Deps struct {
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer
	Doer   apiclient.Doer
	Now    func() int64
	// Sleep delays between auto-retries; defaults to time.Sleep. Tests inject a
	// no-op so the retry layer runs instantly.
	Sleep func(time.Duration)
	// IPProbe fetches the public IP over a given network family for the `ip`
	// command and `setup`. Defaults to a netbind family-pinned probe; tests stub it.
	IPProbe apiclient.IPProber
	// FamilyDoer returns a Doer pinned to a TCP family ("tcp4"/"tcp6"); doctor
	// uses it to replay a signed request over each family to diagnose an
	// IP-allowlist rejection. Defaults to a netbind family-pinned doer; tests stub it.
	FamilyDoer func(network string, timeoutMs int) apiclient.Doer
	// WSDial opens the monitor/tui commands' WebSocket connections. Defaults
	// to stream.DefaultDialer; tests inject a fake.
	WSDial stream.Dialer
	// TUIRun runs the interactive terminal for the `tui` command. nil means
	// the real implementation (tui.Run), which requires stdout to be a TTY;
	// tests inject a stub.
	TUIRun func(tui.Config) error
	// SetupUIRun runs the interactive prompt for `setup` on a TTY. nil means the
	// real implementation (setupui.Run), which requires stdin/stderr to be a TTY;
	// tests inject a stub (and injecting one forces the interactive path on,
	// bypassing the TTY gate, so the wiring is exercisable in-process).
	SetupUIRun func(setupui.Config) error
	// SelfUninstallConfirm is the yes/no prompt `self uninstall` asks its removal
	// questions through. nil means the real stdin-backed confirmer (which needs a
	// terminal); tests inject a scripted one (which also forces the interactive
	// path on, bypassing the TTY gate, so the destructive flow is exercisable
	// in-process).
	SelfUninstallConfirm clienv.Confirm
	// SelfInstallConfirm is the yes/no prompt `self install` asks its PATH-wiring
	// questions through. nil means the real /dev/tty-backed confirmer (the install
	// script pipes into `sh`, so the prompt must talk to the controlling terminal,
	// not the piped stdin); tests inject a scripted one so the flow never reaches a
	// real terminal — without it a `go test` run in an interactive shell prompts
	// the developer's own /dev/tty.
	SelfInstallConfirm clienv.Confirm
	// SkillFS is the embedded Agent Skill content, rooted at the skill directory
	// (SKILL.md at the top), used by `agent skill install`/`doctor`. main injects
	// the //go:embed FS; tests inject a fstest.MapFS. nil only on a build that
	// didn't wire it — the agent-skill commands then fail with a clear error.
	SkillFS fs.FS
}

// runtime holds parsed global flags and shared dependencies for one invocation.
type runtime struct {
	deps depsResolved
	io   output.IO

	key          string
	baseURL      string
	wsBaseURL    string
	bind         string
	family       string
	timeout      string
	retryTimeout string
	timeSyncFlag string
	noReconcile  bool
	dryRun       bool
	jsonOut      bool
	compact      bool
	debug        bool
	logLevelFlag string
	logFile      string
	logFormat    string
	lang         string
	enableExp    bool
	noFsync      bool

	// The centralized operational logger and its sinks, resolved ONCE per
	// invocation by setupLogging (called from dispatch). Every command logs
	// through rt.log instead of building its own logger; the TUI is the one
	// exception (it can't write to stderr under the alt-screen, so it logs only
	// when --log-file routes logs to a file — see tuicmd / surfaceLogger).
	//
	// stderr is shared by everything that isn't the result: operational logs,
	// output.IO Note/Notef + the error envelope, a bot's console.log, and ops
	// honesty notes. So buildLogger funnels ALL of it through ONE locked stderr
	// sink (rt.io.Err becomes that *syncWriter) — concurrent writers (monitor/mcp)
	// can't interleave mid-line. stdout (rt.io.Out) is single-writer (the result),
	// so it stays lock-free.
	//   logToFile — true when --log-file diverts the operational logs to a file
	//           (else they share the stderr sink). The explicit source for the
	//           "file vs stderr" question (the log style's timestamp, the tui's
	//           log-only-to-file rule).
	//   logW  — the sink the operational logger writes to: the one stderr sink by
	//           default, or a separate locked --log-file writer when set (only
	//           operational logs divert to the file; program output stays on stderr).
	//   log   — the one operational logger (logging.NewStyled over logW).
	logToFile bool
	logW      io.Writer
	log       *slog.Logger
}

// debugMode reports whether verbose diagnostics and read journaling are
// on, via the --debug flag or a truthy KORBIT_CLI_DEBUG env var.
func (rt *runtime) debugMode() bool {
	if rt.debug {
		return true
	}
	switch rt.deps.Getenv("KORBIT_CLI_DEBUG") {
	case "1", "true", "yes":
		return true
	}
	return false
}

// noFsyncMode reports whether the action journal and the monitor bot database
// should be opened with PRAGMA synchronous=OFF (no fsync), via the --no-fsync
// flag or a truthy KORBIT_CLI_NO_FSYNC env var.
func (rt *runtime) noFsyncMode() bool {
	if rt.noFsync {
		return true
	}
	switch rt.deps.Getenv("KORBIT_CLI_NO_FSYNC") {
	case "1", "true", "yes":
		return true
	}
	return false
}

// experimentalEnabled reports whether opt-in, not-yet-stable features (currently
// the monitor command's JavaScript bot runtime) are allowed, via the
// --enable-experimental flag or a truthy KORBIT_CLI_ENABLE_EXPERIMENTAL env var.
func (rt *runtime) experimentalEnabled() bool {
	if rt.enableExp {
		return true
	}
	switch rt.deps.Getenv("KORBIT_CLI_ENABLE_EXPERIMENTAL") {
	case "1", "true", "yes":
		return true
	}
	return false
}

// timeSyncSetting returns the raw --time-sync request (the flag, else
// KORBIT_CLI_TIME_SYNC), or "" when neither is set (the auto default).
func (rt *runtime) timeSyncSetting() string {
	if rt.timeSyncFlag != "" {
		return rt.timeSyncFlag
	}
	return rt.deps.Getenv("KORBIT_CLI_TIME_SYNC")
}

// timeSyncMode resolves the server-clock sync mode (--time-sync /
// KORBIT_CLI_TIME_SYNC). dispatch validates the value up front, so an
// unrecognized word falls back to the auto default here.
func (rt *runtime) timeSyncMode() clienv.TimeSyncMode {
	m, _ := clienv.ParseTimeSyncMode(rt.timeSyncSetting())
	return m
}

// logLevelSetting returns the explicit log-level request (the --log-level flag,
// else KORBIT_CLI_LOG_LEVEL), or "" when neither is set.
func (rt *runtime) logLevelSetting() string {
	if rt.logLevelFlag != "" {
		return rt.logLevelFlag
	}
	return rt.deps.Getenv("KORBIT_CLI_LOG_LEVEL")
}

// logLevel resolves the operational logger threshold. --log-level (or
// KORBIT_CLI_LOG_LEVEL) controls ONLY the level and wins over --debug when both
// are set; otherwise the level follows debug mode (Debug under
// --debug/KORBIT_CLI_DEBUG, Error by default — warn-and-below operational logs
// are opt-in, since failures surface through the program-output error envelope).
// --debug still
// independently governs read journaling regardless of --log-level. An
// unrecognized value falls back to the debug-derived default here; setupLogging
// validates it up front so a bad value is a clean usage error before any logging.
func (rt *runtime) logLevel() slog.Level {
	if v := rt.logLevelSetting(); v != "" {
		if lvl, ok := logging.ParseLevel(v); ok {
			return lvl
		}
	}
	return logging.LevelFor(rt.debugMode())
}

// logFileSetting returns the explicit --log-file path (else KORBIT_CLI_LOG_FILE),
// or "" when neither is set.
func (rt *runtime) logFileSetting() string {
	if rt.logFile != "" {
		return rt.logFile
	}
	return rt.deps.Getenv("KORBIT_CLI_LOG_FILE")
}

// logFormatSetting returns the explicit --log-format request (the --log-format
// flag, else KORBIT_CLI_LOG_FORMAT), or "" when neither is set (text default).
func (rt *runtime) logFormatSetting() string {
	if rt.logFormat != "" {
		return rt.logFormat
	}
	return rt.deps.Getenv("KORBIT_CLI_LOG_FORMAT")
}

// logStyle resolves the operational logger's presentation (see logging.Style):
// --log-format json selects JSON for every sink; otherwise text, timestamped
// (and untagged) only when logs are diverted to a file, where a wall-clock
// anchor is wanted and the terminal-scoped "korbit-cli:" tag is not. setupLogging
// validates the format word up front, so an unrecognized value here falls back
// to text.
func (rt *runtime) logStyle() logging.Style {
	if strings.ToLower(strings.TrimSpace(rt.logFormatSetting())) == "json" {
		return logging.Style{Format: logging.FormatJSON}
	}
	return logging.Style{Format: logging.FormatText, Timestamp: rt.logToFile}
}

// setupLogging resolves the operational-logging configuration once per
// invocation and builds the one logger every command shares. It validates
// --log-level and --log-format, opens --log-file (returning a closer the caller
// defers), and wires rt.log/rt.logW + the locked stderr sink. Operational diagnostics (signing,
// retries, timing, clock-sync, journal-write health) flow through rt.log; the
// result and error envelope stay program output owned by internal/output and
// never touch this logger.
func (rt *runtime) setupLogging() (func() error, error) {
	if v := rt.logLevelSetting(); v != "" {
		if _, ok := logging.ParseLevel(v); !ok {
			return func() error { return nil }, output.Usagef(`--log-level: %q is not a level — use trace, debug, info, warn, error, or off`, v)
		}
	}
	if v := strings.ToLower(strings.TrimSpace(rt.logFormatSetting())); v != "" && v != "text" && v != "json" {
		return func() error { return nil }, output.Usagef(`--log-format: %q is not a format — use text or json`, rt.logFormatSetting())
	}
	closer := func() error { return nil }
	var logFile io.Writer
	if path := rt.logFileSetting(); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return closer, output.Usagef("--log-file: cannot open %q for logging: %v", path, err)
		}
		logFile = f
		closer = f.Close
	}
	rt.buildLogger(logFile)
	return closer, nil
}

// buildLogger funnels all stderr through one locked sink and wires rt.logW/rt.log.
// logFile is the opened --log-file (nil = logs go to stderr). Split out so tests
// (and any path that skips setupLogging) can establish the default stderr logger
// directly with buildLogger(nil). It is idempotent: it reassigns rt.io.Err to the
// locked sink, so a second call must not re-wrap it.
func (rt *runtime) buildLogger(logFile io.Writer) {
	if rt.logW != nil {
		return
	}
	rt.logToFile = logFile != nil
	// One locked stderr sink for ALL program output (notes, the error envelope,
	// a bot's console.log, ops honesty notes) AND for operational logs when they
	// go to stderr — so nothing on the shared fd interleaves mid-line. stdout
	// stays lock-free (single writer: the result).
	stderrSink := &syncWriter{w: rt.io.Err}
	rt.io.Err = stderrSink
	if rt.logToFile {
		// --log-file diverts ONLY operational logs to the file (its own lock —
		// several loggers, the operational one plus the stream/state/notice ones,
		// write concurrently); program output stays on the stderr sink.
		rt.logW = &syncWriter{w: logFile}
	} else {
		rt.logW = stderrSink // logs and program output share the one stderr lock
	}
	rt.log = logging.NewStyled(rt.logW, rt.logLevel(), rt.logStyle())
}

// logger returns the centralized operational logger, building a default
// stderr logger on first use if setupLogging has not run (defensive — every
// real command path runs through dispatch, which calls setupLogging first).
func (rt *runtime) logger() *slog.Logger {
	if rt.log == nil {
		rt.buildLogger(nil)
	}
	return rt.log
}

// surfaceLogger returns the operational logger appropriate for a command
// surface. Every surface logs through rt.logger() EXCEPT the tui: it owns the
// full-screen alt-screen, so stderr is unusable and it stays silent (nil logger,
// treated as no-op by logging.Or in the lower layers) unless --log-file diverts
// logs to a file. This is the one place that distinction is made, so every
// clock/stream/client built for a surface gets the right logger from one call.
// (The tui frontend in tuicmd makes the same decision via its own
// tuiLogging over clienv.Env.LogToFile.)
func (rt *runtime) surfaceLogger(surface string) *slog.Logger {
	if surface == apiclient.SurfaceTUI && !rt.logToFile {
		return nil // silent on stderr unless --log-file is set
	}
	return rt.logger()
}

// jsonMode reports whether stdout should carry the machine-readable JSON
// contract. --compact implies --json (compact JSON is still JSON), so the
// effective rule is `--json || --compact`. When false, the success path emits
// human-readable text instead (errors are unaffected — always structured JSON
// on stderr).
func (rt *runtime) jsonMode() bool { return rt.jsonOut || rt.compact }

func (rt *runtime) LoadConfig() (string, config.Config, error) {
	home := config.Home(rt.deps.Getenv)
	cfg, err := config.Load(home, rt.logger())
	return home, cfg, err
}

func (rt *runtime) KeyManager(home string, cfg config.Config) *keys.Manager {
	return keys.NewManager(home, cfg.Keystore, rt.deps.Now, rt.logger())
}

// cmd builds the clienv.Cmd a command subpackage runs against: the resolved
// environment data plus the cli-implemented capability interfaces (Backend,
// Console), which the runtime itself satisfies. A subpackage thus reaches every
// capability off the one value without touching cli-internal state.
func (rt *runtime) cmd() *clienv.Cmd {
	return &clienv.Cmd{Env: rt.env(), Backend: rt, Console: rt}
}

// env snapshots the resolved per-invocation environment for a command. Called
// from dispatch after setupLogging, so rt.logger() is the resolved logger.
func (rt *runtime) env() clienv.Env {
	log := rt.logger() // builds the logger (and the locked stderr sink) if not yet built
	return clienv.Env{
		Getenv:    rt.deps.Getenv,
		Doer:      rt.deps.Doer,
		Now:       rt.deps.Now,
		Sleep:     rt.deps.Sleep,
		IO:        rt.io,
		Log:       log,
		LogSink:   rt.logW,
		LogStyle:  rt.logStyle(),
		LogLevel:  rt.logLevel(),
		LogToFile: rt.logToFile,
		Modes: clienv.Modes{
			DryRun:       rt.dryRun,
			JSONMode:     rt.jsonMode(),
			TimeSync:     rt.timeSyncMode(),
			NoFsync:      rt.noFsyncMode(),
			Experimental: rt.experimentalEnabled(),
		},
		Key:            rt.key,
		IPProbe:        rt.deps.IPProbe,
		FamilyDoer:     rt.deps.FamilyDoer,
		Family:         rt.deps.netFamily,
		WSDial:         rt.deps.WSDial,
		Confirm:        rt.confirmer(),
		InstallConfirm: rt.deps.SelfInstallConfirm,
	}
}

// confirmer resolves the interactive yes/no prompt for a by-nature-interactive
// command (self uninstall): the injected test confirmer when set, else the real
// stdin-backed one — which is nil when stdin is not a terminal, the signal the
// command uses to refuse to run non-interactively.
func (rt *runtime) confirmer() clienv.Confirm {
	if rt.deps.SelfUninstallConfirm != nil {
		return rt.deps.SelfUninstallConfirm
	}
	return selfcmd.StdinConfirmer(rt.io.Err)
}

// CommandHelp renders focused per-command help for a command id, folding in the
// experimental opt-in; it reports false when no such command exists. It is the
// clienv.Console help capability — implemented here because it reaches the
// command surface.
func (rt *runtime) CommandHelp(id ...string) (string, bool) {
	if c := findSurface(id); c != nil {
		return RenderCommandHelp(*c, rt.experimentalEnabled()), true
	}
	return "", false
}

// Generic CLI helpers shared with command subpackages; the canonical
// implementations live in clienv (which command subpackages import directly).
// These aliases keep the cli's own call sites unchanged.
var (
	requireNoArgs     = clienv.RequireNoArgs
	parseRange        = clienv.ParseRange
	parseDuration     = clienv.ParseDuration
	formatJournalWarn = clienv.FormatJournalWarn
)

// globalValueFlags is the set of global (persistent) flag names that consume a
// following value in the `--flag value` (space) form, derived from the one
// canonical list (spec.GlobalFlags). A group command disables flag parsing, so
// its RunE scans args by hand and must skip such a value or it would mistake it
// for an attempted subcommand (e.g. read "sandbox" in `--key sandbox <group>`).
var globalValueFlags = func() map[string]bool {
	m := make(map[string]bool)
	for _, g := range spec.GlobalFlags {
		if g.TakesValue {
			m[g.Flag] = true
		}
	}
	return m
}()

// Execute builds and runs the CLI for args, returning the process exit code.
func Execute(args []string, d Deps) int {
	rt := &runtime{
		deps: resolveDeps(d),
		io:   output.IO{Out: d.Stdout, Err: d.Stderr},
	}
	root := buildTree(rt)
	root.SetArgs(args)
	root.SetOut(rt.io.Out)
	root.SetErr(rt.io.Err)

	err := root.Execute()
	if err == nil {
		return output.ExitSuccess
	}
	// A help/version path already wrote its own output and just carries an exit code.
	var ee clienv.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	// Everything else is classified by its type: UsageError->ExitUsage,
	// ConfigError->ExitConfig, ApiError->ExitAPI, and anything untyped (notably
	// transport/network failures)->ExitInternal, so an agent can tell "fix the
	// invocation" from "retry". cobra's own flag-parse errors are turned into
	// UsageErrors by the FlagErrorFunc.
	return rt.io.EmitError(err, rt.jsonMode(), rt.compact)
}

func buildTree(rt *runtime) *cobra.Command {
	root := &cobra.Command{
		Use:           progname.Name(),
		Short:         "Korbit Open API v2 CLI",
		Version:       version.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		// Resolve the outbound networking flags once, before any command runs, and
		// bind the real-network dialers. Runs for every leaf (no child overrides
		// it); injected (test) dialers are left untouched so the flags stay inert
		// under a stub. A bad --bind/--family fails fast here as a usage error.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return rt.applyNetBinding()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				// Bare invocation: print help to stdout, exit ExitUsage (a usage situation).
				_, _ = rt.io.Out.Write([]byte(RenderRootHelp() + "\n"))
				return clienv.ExitError{Code: output.ExitUsage}
			}
			// Accept the bare `version` verb as a sibling of `--version`: it is a
			// near-universal reflex, so print the version (matching the --version
			// template) and exit 0 instead of treating it as an unknown command.
			if args[0] == "version" {
				_, _ = rt.io.Out.Write([]byte(version.Version + "\n"))
				return nil
			}
			// Root accepts arbitrary args, so an unknown command lands here:
			// emit a structured usage error (stdout stays a clean JSON channel)
			// with a did-you-mean and a pointer to the machine catalog.
			msg := fmt.Sprintf("unknown command %q", args[0])
			if s := suggestCommand(args[0]); s != "" {
				msg += fmt.Sprintf(` — did you mean "%s"?`, s)
			}
			return output.Usagef("%s — run `%s --help` or `%s commands`", msg, progname.Name(), progname.Name())
		},
	}
	root.SetVersionTemplate(version.Version + "\n")
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		_, _ = rt.io.Out.Write([]byte(helpFor(rt, cmd) + "\n"))
	})
	// cobra flag-parse failures (unknown flag, missing value) become usage errors
	// so they map to exit 2 with the structured error envelope.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &output.UsageError{Message: err.Error()}
	})

	pf := root.PersistentFlags()
	pf.StringVar(&rt.key, "key", "", "")
	pf.StringVar(&rt.baseURL, "base-url", "", "")
	pf.StringVar(&rt.wsBaseURL, "ws-base-url", "", "")
	pf.StringVar(&rt.bind, "bind", "", "")
	pf.StringVar(&rt.family, "family", "", "")
	pf.StringVar(&rt.timeout, "timeout", "", "")
	pf.StringVar(&rt.retryTimeout, "retry-timeout", "", "")
	pf.StringVar(&rt.timeSyncFlag, "time-sync", "", "")
	pf.BoolVar(&rt.noReconcile, "no-reconcile", false, "")
	pf.BoolVar(&rt.dryRun, "dry-run", false, "")
	pf.BoolVar(&rt.jsonOut, "json", false, "")
	pf.BoolVar(&rt.compact, "compact", false, "")
	pf.BoolVar(&rt.debug, "debug", false, "")
	pf.StringVar(&rt.logLevelFlag, "log-level", "", "")
	pf.StringVar(&rt.logFile, "log-file", "", "")
	pf.StringVar(&rt.logFormat, "log-format", "", "")
	pf.StringVar(&rt.lang, "lang", "", "")
	pf.BoolVar(&rt.enableExp, "enable-experimental", false, "")
	pf.BoolVar(&rt.noFsync, "no-fsync", false, "")

	// Group parents for multi-segment commands (e.g. order, key, sandbox runtime).
	// A group has no Command entry of its own; it is synthesized here from the
	// segments preceding its subcommands' last id segment, and nested under its
	// own parent group (or the root for a top-level group). Keyed by the
	// space-joined path so each group is built once.
	groups := map[string]*cobra.Command{}
	var groupOf func(path []string) *cobra.Command
	groupOf = func(path []string) *cobra.Command {
		key := strings.Join(path, " ")
		if g, ok := groups[key]; ok {
			return g
		}
		g := &cobra.Command{
			Use:           path[len(path)-1],
			Annotations:   map[string]string{"groupPath": key},
			Args:          cobra.ArbitraryArgs,
			SilenceErrors: true,
			SilenceUsage:  true,
			// Disable flag parsing on the group itself so a mistyped subcommand
			// followed by flags (e.g. `order plac --symbol btc_krw`) reaches this
			// RunE as plain args instead of cobra rejecting `--symbol` as an
			// "unknown flag" against the bare group. cobra still routes a
			// correctly-spelled subcommand (`order place ...`) to its leaf child
			// before this RunE runs, so the leaf keeps parsing its own flags and
			// real unknown-flag errors there are unaffected.
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				subs := surfaceChildren(path...)
				names := make([]string, 0, len(subs))
				for _, s := range subs {
					names = append(names, s.Name)
				}
				// With flag parsing disabled, args may interleave the attempted
				// subcommand token with flags; the intended subcommand is the
				// first non-dash arg. A bare `--help`/`-h` shows focused group help.
				// Flag parsing is off on the group, so the global output flags
				// (--json/--compact) aren't bound by cobra when they follow the group
				// name (e.g. `key --json`) — honor them here so EVERY command, group
				// or leaf, accepts them and a group's error envelope respects the mode.
				attempted := ""
				for i := 0; i < len(args); i++ {
					a := args[i]
					switch a {
					case "--help", "-h":
						_, _ = rt.io.Out.Write([]byte(RenderGroupHelp(path...) + "\n"))
						return clienv.ExitError{Code: output.ExitSuccess}
					case "--json":
						rt.jsonOut = true
						continue
					case "--compact":
						rt.compact = true
						continue
					}
					// A global value-taking flag in `--flag value` (space) form puts
					// its value in the next token; skip it so the value isn't read as
					// the attempted subcommand. The `--flag=value` form is one token
					// and is already skipped as a dash-prefixed arg below.
					if strings.HasPrefix(a, "--") && !strings.Contains(a, "=") && globalValueFlags[strings.TrimPrefix(a, "--")] {
						i++ // consume the value token
						continue
					}
					if attempted == "" && !strings.HasPrefix(a, "-") {
						attempted = a
					}
				}
				if attempted != "" {
					msg := fmt.Sprintf("unknown subcommand %q", key+" "+attempted)
					if s := suggestFrom(attempted, names); s != "" {
						msg += fmt.Sprintf(` — did you mean "%s %s"?`, key, s)
					}
					return output.Usagef("%s — valid: %s", msg, strings.Join(names, ", "))
				}
				// A bare group with no subcommand is an incomplete command: print the
				// focused group help (the same text `<group> --help` shows) so the user
				// sees the choices, and exit ExitUsage to keep the "fix the invocation"
				// signal (mirrors the bare-root behavior).
				_, _ = rt.io.Out.Write([]byte(RenderGroupHelp(path...) + "\n"))
				return clienv.ExitError{Code: output.ExitUsage}
			},
		}
		groups[key] = g
		if len(path) == 1 {
			root.AddCommand(g)
		} else {
			groupOf(path[:len(path)-1]).AddCommand(g)
		}
		return g
	}

	for _, sc := range commandSurface() {
		sc := sc
		leaf := newLeaf(rt, sc)
		if len(sc.ID) >= 2 {
			groupOf(sc.ID[:len(sc.ID)-1]).AddCommand(leaf)
		} else {
			root.AddCommand(leaf)
		}
	}
	return root
}

func newLeaf(rt *runtime, sc surfaceCmd) *cobra.Command {
	use := sc.ID[len(sc.ID)-1]
	leaf := &cobra.Command{
		Use:           use,
		Short:         sc.Summary,
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return rt.dispatch(sc, cmd, args)
		},
	}
	leaf.Annotations = map[string]string{"specKey": sc.Key()}
	// A hidden command (e.g. `self install`, run only by the install script) stays
	// dispatchable and keeps its own `--help`, but is dropped from cobra's own
	// listings — matching its absence from this CLI's help/catalog.
	leaf.Hidden = sc.Hidden
	// `sandbox exec` forwards everything after it verbatim to the sandbox bundle
	// (its own subcommands carry flags this CLI doesn't model, e.g. --user), so
	// disable cobra flag parsing on that one leaf and treat the tail as raw args.
	// A bare --help/-h is still surfaced as group/command help in the RunE.
	if sc.Key() == "sandbox exec" {
		leaf.DisableFlagParsing = true
		return leaf
	}
	for _, p := range sc.Params {
		if p.Kind == cmdmeta.KindFlag {
			leaf.Flags().Bool(p.Flag, false, p.Desc)
		} else {
			leaf.Flags().String(p.Flag, "", p.Desc)
		}
	}
	// Every command carries two hidden diagnostic flags: --cpuprofile /
	// --memprofile write pprof profiles spanning the command's run (dispatch
	// honors them). They are operational, not part of the documented surface (not
	// spec Params), so they stay out of the command catalog and help.
	leaf.Flags().String("cpuprofile", "", "write a CPU profile to this file")
	leaf.Flags().String("memprofile", "", "write a heap profile to this file")
	_ = leaf.Flags().MarkHidden("cpuprofile")
	_ = leaf.Flags().MarkHidden("memprofile")
	return leaf
}

func (rt *runtime) dispatch(sc surfaceCmd, cmd *cobra.Command, args []string) error {
	stop, err := startProfiling(cmd)
	if err != nil {
		return err
	}
	defer stop()
	// Resolve operational logging once, here at the universal command chokepoint,
	// so every command shares the one logger (rt.log) and the same level/
	// destination resolution rather than each building its own.
	closeLog, err := rt.setupLogging()
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	// Validate --time-sync once here; timeSyncMode() is tolerant (auto fallback)
	// so the bad value surfaces as a usage error rather than silently defaulting.
	if v := rt.timeSyncSetting(); v != "" {
		if _, ok := clienv.ParseTimeSyncMode(v); !ok {
			return output.Usagef(`--time-sync: %q is not a mode — use on, off, or auto`, v)
		}
	}
	// Resolve the display language once (i18n owns parsing + validation), so a
	// bad --lang is a usage error regardless of command. This only records the
	// detected language; a surface that localizes (the tui) activates it.
	if _, err := i18n.Resolve(rt.lang, rt.deps.Getenv); err != nil {
		return output.Usagef("--lang: %v", err)
	}
	if !sc.Builtin {
		return rt.runEndpoint(sc, cmd, args)
	}
	c := sc.cmd
	{
		if c.ID[0] == "commands" {
			if len(args) > 0 {
				return output.Usagef("unexpected argument %q", args[0])
			}
			return rt.Emit("commands", buildCatalog())
		}
		if c.ID[0] == "ip" {
			return rt.runIP(cmd, args)
		}
		if c.ID[0] == "license" {
			return rt.runLicense(cmd, args)
		}
		if c.ID[0] == "doctor" {
			return doctorcmd.Run(rt.cmd(), cmd, args)
		}
		if c.ID[0] == "logs" {
			return rt.runLogs(cmd, args)
		}
		if c.ID[0] == "keystore" {
			return keymgmtcmd.RunKeystore(rt.cmd(), c, cmd, args)
		}
		if c.ID[0] == "sandbox" {
			return sandboxcmd.Run(rt.cmd(), c, cmd, args)
		}
		if c.ID[0] == "agent" {
			return agentskillcmd.Run(rt.cmd(), rt.deps.SkillFS, c, cmd, args)
		}
		if c.ID[0] == "self" {
			return selfcmd.Run(rt.cmd(), c, cmd, args)
		}
		if c.ID[0] == "monitor" {
			return monitorcmd.Run(rt.cmd(), cmd, args)
		}
		if c.Key() == "mcp serve" {
			return rt.runMCP(cmd, args)
		}
		if c.ID[0] == "tui" {
			return tuicmd.Run(rt.cmd(), cmd, args, rt.deps.TUIRun)
		}
		if c.Key() == "debug bundle" {
			return rt.runDebugBundle(cmd, args)
		}
		if c.Key() == "setup" {
			return rt.runSetup(c, cmd, args)
		}
		if c.ID[0] == "key" {
			return rt.runKeyMgmt(c, cmd, args)
		}
		return fmt.Errorf("internal: unhandled command %q", c.Key())
	}
}

// keyContext builds the KeyContext shared by `setup` and the `key *` commands:
// config + key manager + the set flags, plus the --with-transfers permission
// opt-in that both `setup` and `key add` honor. The command-specific extras
// (setup's IP probe + doctor closure; `key set-base-url`'s endpoint verify) are
// added by the callers below.
func (rt *runtime) keyContext(c *spec.Command, cmd *cobra.Command) (keymgmtcmd.KeyContext, map[string]string, config.Config, error) {
	home, cfg, err := rt.LoadConfig()
	if err != nil {
		return keymgmtcmd.KeyContext{}, nil, config.Config{}, err
	}
	km := rt.KeyManager(home, cfg)
	flags := stringFlags(c, cmd)
	kc := keymgmtcmd.KeyContext{IO: rt.io, JSONMode: rt.jsonMode(), Compact: rt.compact, Home: home, KM: km, Getenv: rt.deps.Getenv}
	// --with-transfers (setup / key add) opts the registration deep link into the
	// deposit/withdrawal write scopes; off by default.
	if flags["with-transfers"] == "true" {
		kc.Perms = keymgmtcmd.PermAll
	}
	// --base-url / --ws-base-url (global flags): `key set-base-url` and the create
	// commands (`setup` first-create, `key add`) read these to pin a key to a
	// non-default API host; ws is derived from rest when --ws-base-url is omitted.
	if cmd.Flags().Changed("base-url") {
		kc.BaseURL, kc.BaseURLSet = rt.baseURL, true
	}
	if cmd.Flags().Changed("ws-base-url") {
		kc.WSBaseURL, kc.WSBaseURLSet = rt.wsBaseURL, true
	}
	return kc, flags, cfg, nil
}

// runSetup dispatches the `setup` command: it builds the key context, probes the
// public IP for the registration allowlist, and wires the complementary doctor
// health check (RunSetup runs it only once setup ends configured), then hands
// off to keymgmtcmd.RunSetup.
func (rt *runtime) runSetup(c *spec.Command, cmd *cobra.Command, args []string) error {
	kc, flags, cfg, err := rt.keyContext(c, cmd)
	if err != nil {
		return err
	}
	// setup augments its guidance (and the registration deep link) with the IP
	// allowlist entries, probed best-effort against production (probe.ProdBaseURL) —
	// a failure here (no connectivity) must not fail key generation: the IP section
	// is simply omitted and the guidance falls back to the generic "set an IP
	// allowlist" text.
	if timeoutMs, err := rt.ipTimeout(cmd); err == nil {
		if rep := probe.IPs(rt.deps.IPProbe, probe.ProdBaseURL, timeoutMs, rt.deps.netFamily.Networks()); rep.Any() {
			kc.IP = &rep
		}
	}
	// Complementary health check: when setup ends configured (a bound key, or an
	// inline credential — keyName "") RunSetup calls this to run doctor. Its report
	// rides the result and its problems surface as warnings; it never changes
	// setup's exit code (AdvisoryReport never returns an error).
	kc.Doctor = func(keyName string) *doctorcmd.Report {
		return doctorcmd.AdvisoryReport(rt.cmd(), cmd, keyName)
	}
	// Pre-bind validation for both interactive setup lanes: sign a whoami with the
	// candidate id before persisting it, so an id the server rejects is never
	// bound. wait=true (auto-claim) rides out the just-registered KEY_NOT_FOUND
	// window; wait=false (manual paste) is one-shot.
	kc.VerifyAPIKey = func(verifyCtx context.Context, keyName, apiKeyID string, wait bool) error {
		return rt.verifyKeyUsable(verifyCtx, cmd, kc.KM, cfg, kc.Home, keyName, apiKeyID, wait)
	}
	// Auto-claim: while the interactive prompt is open, poll for the registered
	// key in the background. Invoked only on the interactive path (RunInteractive
	// wired); harmless to set otherwise.
	kc.PollClaim = func(pollCtx context.Context, keyName string, onStatus func(string)) (string, string, error) {
		return rt.pollKeyClaim(pollCtx, cmd, kc.KM, cfg, kc.Home, keyName, onStatus)
	}
	// Flag consistency: --api-key (bind an id you already have) and --wait (wait for
	// the issued id to appear) supply the same thing two different ways — reject the
	// combination. --wait-timeout only tunes --wait, so it's meaningless alone.
	if flags["wait"] == "true" && strings.TrimSpace(flags["api-key"]) != "" {
		return output.Usagef("--wait can't be combined with --api-key: use --api-key to bind an id you already have, or --wait to wait for the issued id")
	}
	if strings.TrimSpace(flags["wait-timeout"]) != "" && flags["wait"] != "true" {
		return output.Usagef("--wait-timeout has no effect without --wait")
	}
	rt.wireSetupInteractive(&kc, flags)
	if err := rt.wireSetupWait(&kc, flags); err != nil {
		return err
	}
	// Interactive setup — a human at a TTY, not --json/--api-key/--no-interactive/
	// --wait — is the one setup mode that localizes. RunInteractive being wired is
	// exactly that condition (wireSetupInteractive bows out for every machine/headless
	// mode), so activate the detected display language here and the whole run renders
	// in it: the prompt, the result document, and the embedded health check. Every
	// other mode leaves it English, keeping the machine-facing contract stable.
	if kc.RunInteractive != nil {
		if err := i18n.Activate(i18n.Detected()); err != nil {
			return err
		}
	}
	return keymgmtcmd.RunSetup(flags, args, kc)
}

// wireSetupInteractive sets kc.RunInteractive when `setup` should drive the
// on-TTY prompt instead of printing the registration link and exiting: not in
// --json mode (the prompt isn't machine-readable), no --api-key (already the
// one-shot bind path), not --no-interactive, not --wait (the headless poll, wired
// by wireSetupWait), and the terminal supports it. An
// injected SetupUIRun (tests) forces it on and skips the TTY check, so the wiring
// is exercisable in-process; otherwise the real setupui.Run is used and both
// stdin and stderr must be TTYs (the UI reads stdin and renders on stderr, with
// stdout reserved for the result document). When the prompt is not wired, RunSetup
// falls back to the print-and-exit behavior unchanged.
func (rt *runtime) wireSetupInteractive(kc *keymgmtcmd.KeyContext, flags map[string]string) {
	// --wait selects the headless poll instead of the prompt (wired separately by
	// wireSetupWait), so it overrides the interactive UI even on a TTY.
	if rt.jsonMode() || strings.TrimSpace(flags["api-key"]) != "" || flags["no-interactive"] == "true" || flags["wait"] == "true" {
		return
	}
	run := rt.deps.SetupUIRun
	if run == nil {
		// The real prompt drives the process terminal directly (os.Stdin/os.Stderr,
		// not rt.io.Err which may be a line-buffered log sink): the UI reads stdin
		// and renders on stderr, leaving stdout for the result document.
		if !fileIsTerminal(os.Stdin) || !fileIsTerminal(os.Stderr) {
			return
		}
		run = setupui.Run
	}
	kc.RunInteractive = func(intro []string, registrationURL string, submit func(token string) (lines []string, done bool, err error), autoClaim func(context.Context) <-chan keymgmtcmd.ClaimUpdate) (bool, error) {
		// Bridge the command-layer auto-claim channel to the UI's own type, so
		// keymgmtcmd stays free of any setupui dependency. The forward selects on
		// ctx so it can't leak if the UI stops reading (paste won, then quit).
		var sac func(context.Context) <-chan setupui.ClaimUpdate
		if autoClaim != nil {
			sac = func(ctx context.Context) <-chan setupui.ClaimUpdate {
				in := autoClaim(ctx)
				if in == nil {
					return nil
				}
				out := make(chan setupui.ClaimUpdate)
				go func() {
					defer close(out)
					for u := range in {
						select {
						case out <- setupui.ClaimUpdate{Status: u.Status, Result: u.Result, Done: u.Done, Stop: u.Stop, Prefill: u.Prefill}:
						case <-ctx.Done():
							return
						}
					}
				}()
				return out
			}
		}
		err := run(setupui.Config{
			In:              os.Stdin,
			Out:             os.Stderr,
			Intro:           intro,
			RegistrationURL: registrationURL,
			Prompt:          i18n.T("Paste the issued API key id"),
			Submit:          submit,
			AutoClaim:       sac,
		})
		// Ctrl-C is a deliberate cancel, not a failure: report it as canceled so
		// the caller emits nothing (the non-interactive abort emits nothing too).
		if errors.Is(err, setupui.ErrInterrupted) {
			return true, nil
		}
		return false, err
	}
}

// setupWaitTimeoutDefault bounds `setup --wait` when --wait-timeout is omitted;
// setupWaitTimeoutMax is the largest value accepted (a human registering in the
// portal can take a while, but an unbounded blocking command should be opt-in via
// --wait-timeout 0).
const (
	setupWaitTimeoutDefault = 20 * time.Minute
	setupWaitTimeoutMax     = 24 * time.Hour
)

// wireSetupWait enables the non-interactive headless auto-claim (`setup --wait`):
// setup prints the registration link, then blocks polling for the issued key and
// binds + health-checks it automatically. It overrides the interactive prompt
// (wireSetupInteractive bows out when --wait is set) and needs no TTY, so an agent
// can drive setup end-to-end without the user copying the id back. --wait-timeout
// tunes the wait; 0 means wait indefinitely. The conflicting --api-key combination
// is already rejected in runSetup.
func (rt *runtime) wireSetupWait(kc *keymgmtcmd.KeyContext, flags map[string]string) error {
	if flags["wait"] != "true" {
		return nil
	}
	kc.WaitForClaim = true
	kc.WaitTimeout = setupWaitTimeoutDefault
	if raw := strings.TrimSpace(flags["wait-timeout"]); raw != "" {
		// 0 (or 0s) is the explicit "wait indefinitely" escape hatch; otherwise
		// range-check like any other duration flag.
		if raw == "0" || raw == "0s" {
			kc.WaitTimeout = 0
			return nil
		}
		d, err := parseDuration(raw, "--wait-timeout", time.Second, setupWaitTimeoutMax)
		if err != nil {
			return err
		}
		kc.WaitTimeout = d
	}
	return nil
}

// fileIsTerminal reports whether f is an interactive terminal.
func fileIsTerminal(f *os.File) bool {
	return term.IsTerminal(f.Fd())
}

// setupVerifyTimeoutMs bounds the pre-bind whoami probe interactive setup runs.
const setupVerifyTimeoutMs = 10000

// verifyKeyUsable signs a read-only /v2/currentKeyInfo for keyName with a
// CANDIDATE api key id (no binding persisted), so both interactive setup lanes
// confirm the id is registered and the key can sign with it BEFORE binding. It
// uses the doctor surface (never journaled) and the RetryPreExec policy, so a
// skewed clock self-corrects once (EXCEED_TIME_WINDOW) while network/5xx fail
// fast. The base URL honors the key's pin and the global --base-url override.
//
//   - wait=false (manual paste): one-shot. nil means the id works; any error (an
//     API rejection or a network failure) means do not bind.
//   - wait=true (auto-claim): the just-registered key may not be active yet, so a
//     KEY_NOT_FOUND is retried within setupKeyActiveBudget. Success returns nil;
//     the budget elapsing or any other error is returned for the caller to treat
//     as soft (it binds anyway). ctx (the session) aborts the wait on Ctrl-C/Esc.
func (rt *runtime) verifyKeyUsable(ctx context.Context, cmd *cobra.Command, km *keys.Manager, cfg config.Config, home, name, apiKeyID string, wait bool) error {
	signer, err := km.SignerFor(name)
	if err != nil {
		return err
	}
	baseURL := rt.ResolveBaseURL(cmd, cfg, km.MetaBaseURL(name))
	clk := clock.New(rt.localNow())
	syncer := rt.NewClockSyncer(clk, baseURL, setupVerifyTimeoutMs, apiclient.SurfaceDoctor, "setup-verify")
	client := rt.BuildClient(clienv.ClientSpec{
		Surface:   apiclient.SurfaceDoctor,
		Detail:    "setup-verify",
		BaseURL:   baseURL,
		Creds:     &apiclient.Credentials{APIKeyID: apiKeyID, Signer: signer},
		KeyName:   name,
		Clock:     clk,
		Resync:    syncer.Sync,
		TimeoutMs: setupVerifyTimeoutMs,
		Rec:       rt.LogRecorder(home, rt.log),
		Log:       rt.log,
	})
	call := apiclient.Call{Method: "GET", Path: "/v2/currentKeyInfo", Auth: true}
	if !wait {
		_, _, err = client.Do(ctx, call, apiclient.Policy{RetryPreExec: true})
		return err
	}
	// Tolerant: ride out the just-registered KEY_NOT_FOUND window within the budget.
	wctx, cancel := context.WithTimeout(ctx, setupKeyActiveBudget)
	defer cancel()
	for {
		_, _, err = client.Do(wctx, call, apiclient.Policy{RetryPreExec: true})
		if err == nil {
			return nil
		}
		var apiErr *output.ApiError
		if !errors.As(err, &apiErr) || apiErr.Code != "KEY_NOT_FOUND" {
			return err // not the not-yet-active case — surface to the (soft) caller
		}
		select {
		case <-wctx.Done():
			rt.log.Debug("auto-claim: key not active within budget; binding and health-checking anyway", "keyName", name)
			return wctx.Err()
		case <-time.After(time.Duration(setupKeyActivePollMs) * time.Millisecond):
		}
	}
}

// Auto-claim poll cadences are vars, not consts, so tests can shrink them to
// keep the poll loop fast (see export_test.go).
var (
	// setupClaimPollMs is the auto-claim poll cadence; setupClaimBackoffMs is the
	// longer wait after the server signals the poll is too frequent.
	setupClaimPollMs    = 3000
	setupClaimBackoffMs = 5000
	// setupKeyActiveBudget bounds the post-bind wait for a just-registered key to
	// become usable; setupKeyActivePollMs is its retry cadence.
	setupKeyActiveBudget = 5 * time.Second
	setupKeyActivePollMs = 700
)

// setupClaimWaitingNotice is the live status shown while the poll backs off; the
// terminal stop notices are built by keymgmtcmd.KeyContext.claimStopNotice from
// the stop code this poll returns (shared with the post-claim verify so both
// lanes phrase a fall-back identically).
const setupClaimWaitingNotice = "Still waiting — retrying shortly."

// pollKeyClaim runs the background auto-claim poll for a freshly generated,
// not-yet-bound key: it signs the keyless GET /v2/keys/claim with the key's
// private key (no X-KAPI-KEY header — proof of possession) and waits for the
// registered id to appear. It returns (id, "", nil) on success, ("", notice,
// nil) when polling stops without a claim (a documented claim conflict or a
// non-transient request error — the prompt then stays open for manual paste),
// or ("", "", ctx.Err()) when the session ends. It uses the doctor surface, so
// the poll is never journaled, and logs progress to rt.log. The base URL honors
// the key's pin and the global --base-url override.
func (rt *runtime) pollKeyClaim(ctx context.Context, cmd *cobra.Command, km *keys.Manager, cfg config.Config, home, name string, onStatus func(string)) (string, string, error) {
	signer, err := km.SignerFor(name)
	if err != nil {
		return "", "", err
	}
	s, err := km.Show(name)
	if err != nil {
		return "", "", err
	}
	pubB64, err := apiclient.PublicSPKIBase64URL(s.PublicKey)
	if err != nil {
		return "", "", err
	}
	baseURL := rt.ResolveBaseURL(cmd, cfg, km.MetaBaseURL(name))
	clk := clock.New(rt.localNow())
	syncer := rt.NewClockSyncer(clk, baseURL, setupVerifyTimeoutMs, apiclient.SurfaceDoctor, "setup-claim")
	client := rt.BuildClient(clienv.ClientSpec{
		Surface: apiclient.SurfaceDoctor,
		Detail:  "setup-claim",
		BaseURL: baseURL,
		// No APIKeyID: a keyless signed GET (the Build path omits X-KAPI-KEY when
		// the id is empty), authenticated purely by the request signature.
		Creds:     &apiclient.Credentials{Signer: signer},
		KeyName:   name,
		Clock:     clk,
		Resync:    syncer.Sync,
		TimeoutMs: setupVerifyTimeoutMs,
		Rec:       rt.LogRecorder(home, rt.log),
		Log:       rt.log,
	})
	call := apiclient.Call{
		Method: "GET",
		Path:   "/v2/keys/claim",
		Params: []apiclient.KV{{Key: "publicKey", Value: pubB64}, {Key: "type", Value: keys.TypeEd25519}},
		Auth:   true,
	}
	// Log the resolved endpoint up front: an auto-claim that hits the wrong host
	// (e.g. the prod default when the endpoint lives on a per-key/override URL) is
	// otherwise hard to spot — the only symptom is a 404 stop.
	rt.log.Debug("auto-claim polling", "keyName", name, "baseURL", baseURL)
	for {
		data, _, derr := client.Do(ctx, call, apiclient.Policy{RetryPreExec: true})
		if derr == nil {
			var resp struct {
				APIKey string `json:"apiKey"`
			}
			if uerr := json.Unmarshal(data, &resp); uerr != nil || resp.APIKey == "" {
				rt.log.Warn("auto-claim stopped: malformed claim response", "keyName", name)
				return "", "", nil // empty id + no error = a generic stop
			}
			rt.log.Info("auto-claim detected the registered key", "keyName", name)
			return resp.APIKey, "", nil
		}
		wait := setupClaimPollMs
		var apiErr *output.ApiError
		switch {
		case errors.Is(derr, context.Canceled) || errors.Is(derr, context.DeadlineExceeded):
			return "", "", derr
		case errors.As(derr, &apiErr):
			switch {
			case apiErr.Code == "KEY_CLAIM_PENDING":
				rt.log.Debug("auto-claim pending", "keyName", name)
			case apiErr.HTTPStatus == http.StatusTooManyRequests:
				rt.log.Debug("auto-claim backing off (rate limited)", "keyName", name)
				if onStatus != nil {
					onStatus(setupClaimWaitingNotice)
				}
				wait = setupClaimBackoffMs
			case apiErr.HTTPStatus >= 500:
				rt.log.Debug("auto-claim retrying (server error)", "keyName", name, "status", apiErr.HTTPStatus)
			default:
				// Non-transient: a claim conflict, a key-level rejection the server now
				// gates on (IP allowlist, deactivated/expired, key-not-found), or a bad
				// request. Retrying won't help — stop and return the code so the caller
				// renders the fall-back notice (the IP case uses the probed IPs).
				rt.log.Warn("auto-claim stopped", "keyName", name, "code", apiErr.Code, "status", apiErr.HTTPStatus, "baseURL", baseURL)
				return "", apiErr.Code, nil
			}
		default:
			// Transport/network error: transient, keep polling.
			rt.log.Debug("auto-claim retrying (transient error)", "keyName", name, "err", derr)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(time.Duration(wait) * time.Millisecond):
		}
	}
}

// runKeyMgmt dispatches the `key *` subcommands: it builds the key context and,
// for `key set-base-url`, wires the explicit --ws-base-url and the post-store
// endpoint smoke test, then hands off to keymgmtcmd.RunKeyCommand.
func (rt *runtime) runKeyMgmt(c *spec.Command, cmd *cobra.Command, args []string) error {
	kc, flags, _, err := rt.keyContext(c, cmd)
	if err != nil {
		return err
	}
	// `key set-base-url` smoke-tests the stored endpoints unless --no-verify;
	// the probe deps are captured here (root has the resolved deps).
	if c.Key() == "key set-base-url" && flags["no-verify"] != "true" {
		timeoutMs := 5000 // a smoke test must stay snappy; --timeout overrides
		if cmd.Flags().Changed("timeout") {
			if t, perr := parseRange(rt.timeout, "--timeout", 1, 600000); perr == nil {
				timeoutMs = t
			}
		}
		doer, dial := rt.deps.Doer, rt.deps.WSDial
		kc.Verify = func(restURL, wsBaseURL string) probe.EndpointVerification {
			return probe.Endpoints(doer, dial, restURL, wsBaseURL, timeoutMs)
		}
	}
	return keymgmtcmd.RunKeyCommand(c, flags, args, kc)
}

// stringFlags returns the set flags for a builtin command: string-valued flags
// by their value, and bool (KindFlag) flags as "true" when present.
func stringFlags(c *spec.Command, cmd *cobra.Command) map[string]string {
	out := map[string]string{}
	for _, p := range c.Params {
		if !cmd.Flags().Changed(p.Flag) {
			continue
		}
		if p.Kind == cmdmeta.KindFlag {
			out[p.Flag] = "true"
		} else {
			v, _ := cmd.Flags().GetString(p.Flag)
			out[p.Flag] = v
		}
	}
	return out
}

// helpFor renders help for a cobra command from the unified surface. The
// experimental opt-in (flag or env) reveals a command's experimental surface in
// its per-command help.
func helpFor(rt *runtime, cmd *cobra.Command) string {
	if cmd.Annotations["specKey"] != "" {
		if c := findSurface(strings.Fields(cmd.Annotations["specKey"])); c != nil {
			return RenderCommandHelp(*c, rt.experimentalEnabled())
		}
	}
	// A group parent (e.g. `mcp`, `order`, `sandbox runtime`) has no specKey but
	// carries its full path in groupPath — render focused group help, not the
	// root dump.
	if gp := cmd.Annotations["groupPath"]; gp != "" {
		return RenderGroupHelp(strings.Fields(gp)...)
	}
	return RenderRootHelp()
}

// suggestCommand returns the closest top-level command (first id segment) to
// name, or "" if nothing is within typo distance.
func suggestCommand(name string) string {
	seen := map[string]bool{}
	var tops []string
	for _, sc := range commandSurface() {
		if !seen[sc.ID[0]] {
			seen[sc.ID[0]] = true
			tops = append(tops, sc.ID[0])
		}
	}
	return suggestFrom(name, tops)
}

// suggestFrom returns the candidate closest to name within a small edit
// distance (a typo), or "" if none is close enough.
func suggestFrom(name string, candidates []string) string {
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		if d := levenshtein(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	// Tolerate up to ~1/3 of the word as typos, capped at 3.
	threshold := len(name)/3 + 1
	if threshold > 3 {
		threshold = 3
	}
	if bestDist <= threshold {
		return best
	}
	return ""
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// depsResolved is Deps with defaults applied.
type depsResolved struct {
	Getenv     func(string) string
	Now        func() int64
	Sleep      func(time.Duration)
	Doer       apiclient.Doer
	IPProbe    apiclient.IPProber
	FamilyDoer func(network string, timeoutMs int) apiclient.Doer
	WSDial     stream.Dialer
	TUIRun     func(tui.Config) error     // nil = the real tui.Run (needs a TTY)
	SetupUIRun func(setupui.Config) error // nil = the real setupui.Run (needs a TTY)
	// SelfUninstallConfirm is the `self uninstall` yes/no prompt; nil = the real
	// stdin-backed confirmer (selfcmd.StdinConfirmer, which needs a terminal).
	SelfUninstallConfirm clienv.Confirm
	// SelfInstallConfirm is the `self install` PATH-wiring yes/no prompt; nil = the
	// real /dev/tty-backed confirmer (selfcmd.openTTYConfirm).
	SelfInstallConfirm clienv.Confirm
	SkillFS            fs.FS

	// skipBinding marks dialer deps that a CALLER INJECTED (a test stub via
	// cli.Execute(.., Deps{Doer: ...}), or an embedder's own client) rather than
	// the built-in real-network defaults. applyNetBinding's --bind/--family
	// rebinding skips these — so injecting a dialer, which the whole test suite
	// does, is never silently overridden by a network flag. (It is the inverse of
	// "this is a built-in default": false here = safe to rebind.)
	skipBinding struct{ doer, ipProbe, familyDoer, wsDial bool }

	// netFamily is the effective outbound IP family resolved from --family (and any
	// dual→single narrowing a single-family --bind forces). It caps the per-family
	// ip/doctor probes so they never reach out a family the user excluded, and
	// doctor warns when it is not dualstack. The zero value is dualstack (both
	// families), i.e. unconstrained.
	netFamily netbind.Family
}

// applyNetBinding resolves --bind/--family (with their env fallbacks) and, when
// they request any customization, replaces the defaulted real-network dialers
// (REST Doer, WS dialer, family probe, IP probe) with bound equivalents so every
// outbound request — REST, WebSocket, the doctor/IP probes, and the sandbox/Deno
// downloads (which dial through the same Doer) — honors it. A validation failure
// is returned as a usage error (exit 2). Injected dialers are left as-is, so the
// flags are inert under a test stub.
//
// Invariant: these four dialer deps are the ONLY outbound paths. A new
// real-network dial that bypasses them would silently ignore --bind (a leak), so
// route any new egress through one of them.
func (rt *runtime) applyNetBinding() error {
	fam, err := netbind.ParseFamily(rt.firstNonEmpty(rt.family, "KORBIT_CLI_NET_IP_FAMILY"))
	if err != nil {
		return &output.UsageError{Message: err.Error()}
	}
	rt.deps.netFamily = fam // caps the ip/doctor probes even with no --bind
	cfg := netbind.Config{Bind: rt.firstNonEmpty(rt.bind, "KORBIT_CLI_NET_BIND"), Family: fam}
	binder, err := netbind.Resolve(cfg, func(msg string) {
		fmt.Fprintln(rt.io.Err, progname.Name()+": "+msg)
	})
	if err != nil {
		return &output.UsageError{Message: err.Error()}
	}
	if binder == nil {
		return nil
	}
	rt.deps.netFamily = binder.Family() // effective family (a single-family --bind can narrow dual)
	if !rt.deps.skipBinding.doer {
		rt.deps.Doer = binder.Doer()
	}
	if !rt.deps.skipBinding.wsDial {
		rt.deps.WSDial = stream.DialerWithClient(binder.WSClient())
	}
	if !rt.deps.skipBinding.familyDoer {
		rt.deps.FamilyDoer = familyDoerFor(binder)
	}
	if !rt.deps.skipBinding.ipProbe {
		rt.deps.IPProbe = ipProberFor(binder)
	}
	return nil
}

// familyDoerFor builds the family-pinned Doer factory the diagnostics use, bound
// to b's source (b == nil = unbound). The family pin + binding live in netbind.
func familyDoerFor(b *netbind.Binder) func(network string, timeoutMs int) apiclient.Doer {
	return func(network string, timeoutMs int) apiclient.Doer {
		return netbind.FamilyDoer(b, network, timeoutMs)
	}
}

// ipProberFor builds the /v2/ip prober: a netbind family-pinned (and b-bound)
// client handed to apiclient.ProbeIP, so the prober carries no family/binding logic.
func ipProberFor(b *netbind.Binder) apiclient.IPProber {
	return func(ctx context.Context, network, baseURL, userAgent string, timeoutMs int) (string, error) {
		return apiclient.ProbeIP(ctx, netbind.FamilyDoer(b, network, timeoutMs), baseURL, userAgent)
	}
}

// firstNonEmpty returns flagVal if set, else the named environment variable.
func (rt *runtime) firstNonEmpty(flagVal, envKey string) string {
	if flagVal != "" {
		return flagVal
	}
	return rt.deps.Getenv(envKey)
}

func resolveDeps(d Deps) depsResolved {
	getenv := d.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	now := d.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	sleep := d.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	probe := d.IPProbe
	if probe == nil {
		probe = ipProberFor(nil)
	}
	familyDoer := d.FamilyDoer
	if familyDoer == nil {
		familyDoer = familyDoerFor(nil)
	}
	wsDial := d.WSDial
	if wsDial == nil {
		wsDial = stream.DefaultDialer
	}
	doer := d.Doer
	if doer == nil {
		// Same default apiclient.Client applies to a nil Doer, hoisted here so direct
		// callers (the set-base-url endpoint smoke test) get a working client too.
		doer = http.DefaultClient
	}
	r := depsResolved{Getenv: getenv, Now: now, Sleep: sleep, Doer: doer, IPProbe: probe, FamilyDoer: familyDoer, WSDial: wsDial, TUIRun: d.TUIRun, SetupUIRun: d.SetupUIRun, SelfUninstallConfirm: d.SelfUninstallConfirm, SelfInstallConfirm: d.SelfInstallConfirm, SkillFS: d.SkillFS}
	r.skipBinding.doer = d.Doer != nil
	r.skipBinding.ipProbe = d.IPProbe != nil
	r.skipBinding.familyDoer = d.FamilyDoer != nil
	r.skipBinding.wsDial = d.WSDial != nil
	return r
}
