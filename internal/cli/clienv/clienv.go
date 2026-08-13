// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package clienv is the seam between the cli command tree and the command
// subpackages it dispatches into. A command subpackage cannot import cli (cli
// imports it), so clienv holds the CONTRACTS a command runs against — the
// resolved environment (Env, plain data) and the interfaces a command needs
// (Backend, Console) — plus the generic runtime-free helpers both cli and those
// subpackages share. The IMPLEMENTATIONS stay in cli (its runtime satisfies the
// interfaces), so no command logic lives here.
package clienv

import (
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/korbit-official/korbit-cli/internal/callrec"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/netbind"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/spf13/cobra"
)

// Env is the resolved per-invocation environment a command runs against: plain
// data, no behavior beyond pure predicates over it. cli builds one from its
// runtime and hands it down inside a Cmd.
type Env struct {
	// Getenv reads the process environment (injectable so tests can stub it).
	Getenv func(string) string
	// Doer is the HTTP client public/signed calls run through.
	Doer korbit.Doer
	// Now is the local clock in unix-ms (injectable for tests).
	Now func() int64
	// Sleep delays for a duration (injectable for tests); the streaming commands
	// thread it into the stream session and bot retry budgets.
	Sleep func(time.Duration)
	// IO is the stdout/stderr output contract sink. IO.Err is the one locked
	// stderr sink shared by program output and (when not diverted) the logs.
	IO output.IO
	// Log is the one operational logger for this invocation (already resolved
	// for level/destination/format). A command needing a logger at a different
	// level builds one over the same sink via the logging package.
	Log *slog.Logger
	// LogSink is the writer Log writes to — the locked stderr sink, or the
	// --log-file writer. A streaming command builds its own leveled stream/state/
	// notice loggers over it with logging.NewStyled(LogSink, level, LogStyle).
	LogSink io.Writer
	// LogStyle is the resolved log line shape (text/json, timestamped under
	// --log-file), shared so a command's extra loggers match the operational one.
	LogStyle logging.Style
	// LogLevel is the resolved operational threshold, the level a streaming
	// command gives its stream/state loggers.
	LogLevel slog.Level
	// LogToFile is true when --log-file diverts the operational logs to a file
	// (else they share the stderr sink). The tui logs only in that case — under
	// its alt-screen stderr is unusable.
	LogToFile bool

	// Modes carries the resolved global mode flags (CLI flag / env), so a command
	// subpackage reads them without reaching into cli's runtime.
	Modes Modes

	// Key is the --key selection (empty = the default / env / inline rules).
	Key string
	// IPProbe fetches the public IP over a TCP family (doctor / setup).
	IPProbe korbit.IPProber
	// FamilyDoer returns a Doer pinned to a TCP family ("tcp4"/"tcp6"); doctor
	// uses it to replay a signed request per family to diagnose an IP-allowlist
	// rejection.
	FamilyDoer func(network string, timeoutMs int) korbit.Doer
	// Family is the effective outbound IP family (after any --bind narrowing). The
	// ip/doctor probes cap to Family.Networks() so they never reach out a family
	// --family excluded, and doctor warns when it is not dualstack (a single
	// family means a narrower diagnosis). The zero value is dualstack (both).
	Family netbind.Family
	// WSDial opens the WebSocket connections the streaming commands and doctor's
	// reachability check use.
	WSDial stream.Dialer
	// Confirm asks the user a yes/no question, for a command that is interactive by
	// nature (self uninstall). nil means no interactive confirmer is available
	// (stdin is not a terminal, or a machine-output mode is active) — such a
	// command must refuse to run rather than proceed non-interactively.
	Confirm Confirm
	// InstallConfirm is the yes/no prompt `self install` asks its PATH-wiring
	// questions through. Distinct from Confirm because install's real backing is
	// /dev/tty (the install script pipes into `sh`, so stdin is the pipe), not
	// stdin. nil means no confirmer was injected, so install falls back to its own
	// /dev/tty confirmer; tests inject one so the flow never reaches a real
	// terminal.
	InstallConfirm Confirm
}

// Confirm asks the user a yes/no question with a default (used on empty input or
// EOF), returning the choice, or an error only on a genuine input failure.
type Confirm func(question string, defaultYes bool) (bool, error)

// TimeSyncMode is the resolved --time-sync setting: how the server clock is
// corrected for signed requests. The two behaviors it gates are exposed by
// Proactive (measure up front) and Reactive (resync on rejection).
type TimeSyncMode string

const (
	// TimeSyncAuto signs with the local clock and resyncs reactively — on a
	// correctable EXCEED_TIME_WINDOW rejection, and (for a streaming session) when
	// data appears delayed against a still-unmeasured clock. The default.
	TimeSyncAuto TimeSyncMode = "auto"
	// TimeSyncOn measures the server clock via /v2/time once before the first
	// signed send and signs every call with a corrected timestamp, in addition
	// to the reactive resync.
	TimeSyncOn TimeSyncMode = "on"
	// TimeSyncOff disables all clock correction — no proactive measure and no
	// reactive resync; every call signs with the local clock.
	TimeSyncOff TimeSyncMode = "off"
)

// Proactive reports whether the server clock is measured once before the first
// signed send (TimeSyncOn).
func (m TimeSyncMode) Proactive() bool { return m == TimeSyncOn }

// Reactive reports whether reactive clock resync is enabled (every mode except
// TimeSyncOff): a correctable EXCEED_TIME_WINDOW rejection resyncs, as does the
// stream's delivery-delay kick while the clock is still unmeasured.
func (m TimeSyncMode) Reactive() bool { return m != TimeSyncOff }

// ParseTimeSyncMode resolves a --time-sync value (case-insensitive, trimmed). An
// empty string is the auto default; ok is false for any other unrecognized word.
func ParseTimeSyncMode(s string) (TimeSyncMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return TimeSyncAuto, true
	case "on":
		return TimeSyncOn, true
	case "off":
		return TimeSyncOff, true
	}
	return TimeSyncAuto, false
}

// Modes is the resolved set of global mode flags a command reacts to, each
// resolved once from its CLI flag (and, where noted, env var) so a command
// subpackage never reaches into cli's runtime to read one.
type Modes struct {
	// DryRun is --dry-run: build and show the plan, sign/send nothing.
	DryRun bool
	// JSONMode is the effective machine-output mode (--json or --compact).
	JSONMode bool
	// TimeSync is --time-sync / KORBIT_CLI_TIME_SYNC: the server-clock sync mode
	// (auto default, on, or off).
	TimeSync TimeSyncMode
	// NoFsync is --no-fsync / KORBIT_CLI_NO_FSYNC: open the journal and bot DB
	// with synchronous=OFF.
	NoFsync bool
	// Experimental is --enable-experimental / KORBIT_CLI_ENABLE_EXPERIMENTAL: the
	// opt-in gate for not-yet-stable features (the monitor JS bot runtime).
	Experimental bool
}

// StreamLogger builds a stream-layer logger over the resolved log sink/style at
// the given level, tagged component=<component> — the seam by which a streaming
// command subpackage (monitor/tui) rebuilds the leveled loggers the stream layer
// takes, without reaching into cli. Pass stream.LogComponentStream /
// stream.LogComponentState. It is a pure builder over Env's own data.
func (e Env) StreamLogger(level slog.Level, component string) *slog.Logger {
	return logging.NewStyled(e.LogSink, level, e.LogStyle).With(stream.LogComponentKey, component)
}

// NoticeLogger builds the mirrored-stream-notice logger: component=stream plus
// kind=stream_notice, gated at level. stream.LogNotice adds the per-notice code.
func (e Env) NoticeLogger(level slog.Level) *slog.Logger {
	return e.StreamLogger(level, stream.LogComponentStream).With(stream.LogKindKey, stream.LogKindNotice)
}

// ClientSpec is the per-surface input to Backend.BuildClient: the few things
// that vary across the ways a command calls Korbit (endpoint, tui, monitor, mcp,
// doctor, dry-run, stream). The Backend turns it into the one korbit.Client a
// surface uses, so "forgot the logger / recorder / Origin / User-Agent" is
// structurally impossible — there is one construction site. When one signing
// context mints several clients, write the shared fields once and derive each
// with As, so only surface/detail/logger vary per client.
type ClientSpec struct {
	Surface, Detail string              // korbit.Origin + the useragent component/detail
	BaseURL         string              //
	Creds           *korbit.Credentials // nil = public-only
	KeyName         string              // the named signing key (recorded into CallInfo; "" for public)
	// Clock is the shared server-clock estimate to sign against: a syncer's
	// State for normal surfaces, a raw never-resynced clock for doctor (so skew
	// surfaces), or nil for a pure-public read.
	Clock korbit.Clock
	// Resync is the ONE clock-resync primitive (a syncer's Sync) for
	// auto-correcting EXCEED_TIME_WINDOW; nil = no auto-correct.
	Resync    func() error
	TimeoutMs int
	// Rec is the surface's journal recorder; nil = never journaled.
	Rec *callrec.Recorder
	// Log is the surface's operational logger; nil = silent.
	Log *slog.Logger
}

// As returns a copy of the spec carrying a different call identity — the surface
// and detail (which flow into korbit.Origin and the User-Agent) and the logger.
// It is the per-client variation when one signing context mints several clients,
// so the shared signing fields are written once in a base spec and each client
// is base.As(surface, detail, log). The value receiver makes each call a fresh
// copy; the base is never mutated.
func (s ClientSpec) As(surface, detail string, log *slog.Logger) ClientSpec {
	s.Surface, s.Detail, s.Log = surface, detail, log
	return s
}

// Backend is the call machinery a command uses to talk to Korbit: config/key
// resolution today, growing to base-URL resolution and client/clock/recorder
// construction for the streaming and diagnostic commands. cli's runtime is the
// sole implementation.
type Backend interface {
	LoadConfig() (home string, cfg config.Config, err error)
	KeyManager(home string, cfg config.Config) *keys.Manager
	// ResolveBaseURL resolves the REST base URL by the documented precedence
	// (flag > env > per-key > config > prod default).
	ResolveBaseURL(cmd *cobra.Command, cfg config.Config, keyBaseURL string) string
	// ResolveURLs resolves the REST base URL and its paired WebSocket base URL in
	// one step (the WS precedence guards against pairing a stale stored WS host
	// with a higher REST override).
	ResolveURLs(cmd *cobra.Command, cfg config.Config, keyBaseURL, keyWSBaseURL string) (rest, ws string, err error)
	// BuildClient is the one korbit.Client construction site (logger, retry
	// trace, journaling closure, Origin, User-Agent, shared clock, resync hook).
	BuildClient(ClientSpec) *korbit.Client
	// NewClockSyncer builds the process-wide clock Syncer for a surface over a
	// shared State.
	NewClockSyncer(state *clock.State, baseURL string, timeoutMs int, surface, detail string) *clock.Syncer
	// MeasureOffset probes /v2/time and returns the measured server-clock offset
	// (doctor's skew check signs against a raw clock, so it measures directly).
	MeasureOffset(baseURL string, timeoutMs int) (korbit.ClockOffset, error)
	// NewRecorder builds the journal-backed recorder with a caller-chosen
	// post-write-failure sink.
	NewRecorder(home string, log *slog.Logger, onFail func(callrec.FailMode, error)) *callrec.Recorder
	// LogRecorder is NewRecorder with the common sink: a post-call journal-write
	// failure is logged and the command continues.
	LogRecorder(home string, log *slog.Logger) *callrec.Recorder
}

// Console is the cli-owned, user-facing output a command produces. It can only
// be implemented in cli because it reaches the command catalog and the
// per-command human-formatter registry.
type Console interface {
	// Emit writes a command's success result through the mode-aware emitter —
	// JSON under --json/--compact, otherwise the command's human formatter.
	Emit(commandKey string, value any) error
	// CommandHelp renders focused per-command help for a command id (folding in
	// the experimental opt-in), reporting false when no such command exists.
	CommandHelp(id ...string) (string, bool)
}

// Cmd is what a command subpackage receives: the environment data plus the
// cli-implemented capability interfaces, composed so a command reaches them all
// off one value (cx.Doer, cx.LoadConfig(), cx.Emit(...)).
type Cmd struct {
	Env
	Backend
	Console
}

// ExitError carries a process exit code with no error envelope: the command has
// already written its own output (help text, a passthrough subprocess's stream,
// a report) and only the exit code remains to convey. Execute returns
// ExitError.Code and prints nothing further.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return "" }

// ---- generic helpers shared with command subpackages ----

// RequireNoArgs returns a usage error when a command that takes no positional
// arguments was given one.
func RequireNoArgs(positionals []string) error {
	if len(positionals) > 0 {
		return output.Usagef("unexpected argument %q", positionals[0])
	}
	return nil
}

var digits = regexp.MustCompile(`^\d+$`)

// ParseRange parses raw as an integer and checks it lies within [min, max],
// returning a usage error labeled by label otherwise.
func ParseRange(raw, label string, min, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if !digits.MatchString(raw) {
		return 0, output.Usagef("%s must be an integer", label)
	}
	n, _ := strconv.Atoi(raw)
	if n < min || n > max {
		return 0, output.Usagef("%s must be between %d and %d", label, min, max)
	}
	return n, nil
}

// FormatJournalWarn frames a post-call journal-write failure for the operator —
// the message a surface's onPostFailure sink logs (or toasts) under FailMode Warn.
func FormatJournalWarn(err error) string {
	return "failed to write to the action journal: " + err.Error() + " — your local action history is incomplete"
}

// ParseDuration parses raw as a Go duration string like "90s" (units ms, s, m,
// h, via cmdmeta.ParseDuration) and range-checks it against [min, max],
// returning a usage error labeled by label otherwise.
func ParseDuration(raw, label string, min, max time.Duration) (time.Duration, error) {
	d, ok := cmdmeta.ParseDuration(raw)
	if !ok {
		return 0, output.Usagef(`%s must be a duration like 90s (units: ms, s, m, h)`, label)
	}
	if d < min || d > max {
		return 0, output.Usagef("%s must be between %s and %s", label, min, max)
	}
	return d, nil
}
