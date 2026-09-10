// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package sandbox is the lifecycle manager for the local Digital X API Sandbox: it
// runs the single-file mock under Deno — the CLI-managed pinned Deno by default,
// or a system `deno` — handing Deno the Official-Source bundle URL to fetch and
// cache itself, runs/stops/inspects it as a managed background server, and imports
// the seeded ED25519 key into the normal keystore so a user goes from nothing to a
// signed, working local exchange in one command. It is a deliberately limited
// convenience feature: there is no Node runtime and the CLI does not manage a
// bundle cache. For anything more involved, download the bundle from the Official
// Source and run it yourself.
//
// Design boundaries this package enforces:
//   - It NEVER writes config.json (the global default is untouched) and only ever
//     writes LOOPBACK base URLs onto the sandbox key. So a sandbox run can't
//     redirect real commands, and a real command can't silently hit the mock.
//   - The seeded key is imported via keys.AddBound so its SANDBOX_ id is present
//     at creation and it is never even transiently the default.
//   - The artifact cache (the managed Deno + Deno's own module cache) is shared
//     across agents; mutable state (db, pidfile, log) lives under
//     DIGITALX_CLI_HOME/sandbox/.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/config"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox/deno"
)

// DefaultPort is the predictable default the sandbox binds, falling back to an
// OS-assigned ephemeral port (via the bundle's --port 0) on collision.
const DefaultPort = 9999

// DefaultKeyName is the imported seeded key's name.
const DefaultKeyName = "sandbox"

// MinSandboxVersion is the lowest sandbox bundle version this CLI supports. It
// is passed to the bundle (via MinVersionEnv) on start; an older bundle refuses
// to run, so the CLI can update + retry rather than fail confusingly against a
// build missing a capability it relies on. Bump this in lockstep when the CLI
// starts depending on a newer bundle feature. The pre-release version-skew guard
// asserts this never exceeds the bundle's own version.
//
// The floor is exactly 1.4.0: that is the first bundle that accepts `init-db
// --mode digitalx-api` and reads the DIGITALX_SANDBOX_* config names, both of
// which this CLI relies on.
const MinSandboxVersion = "1.4.0"

// Config carries one sandbox operation's settings. Zero values resolve to the
// documented defaults.
type Config struct {
	// Home is the CLI home (DIGITALX_CLI_HOME); mutable state lives under it.
	Home string
	// CacheDir is the shared artifact cache root (os.UserCacheDir()/digitalx-cli,
	// or an existing os.UserCacheDir()/korbit-cli, or DIGITALX_CLI_SANDBOX_CACHE).
	CacheDir string
	// URL overrides the bundle source (Official Source by default; a local path
	// or file:// is read from disk).
	URL string
	// RuntimePref is "", "deno", or "managed-deno".
	RuntimePref string
	// Port is the requested fixed port (0 ⇒ DefaultPort with ephemeral fallback;
	// a non-zero value forces that exact port with no fallback).
	Port int
	// DB overrides the database path (default <home>/sandbox/sandbox.db; see DBFileName).
	DB string
	// KeyName overrides the imported key's name (default DefaultKeyName).
	KeyName string
	// Reimport replaces the imported key's material even if it already exists.
	Reimport bool
	// Paper enables paper trading: a fresh database is initialized with its
	// seeded pairs' market data mirrored LIVE from production Digital X (the
	// bundle's `init-db --source live`, which verifies each pair's production
	// status and needs network); fills stay simulated locally. The seeded set is
	// the bundle's fixture pairs by default, or every LAUNCHED production pair
	// (the tradable universe) when AllPairs is set. On an existing database the
	// flag only VERIFIES the pairs
	// are already in live mode — it never flips a database in place (see
	// ensurePaperDB); combine with Fresh to switch modes.
	Paper bool
	// AllPairs seeds a fresh database from a live production snapshot (the
	// bundle's `init-db --mode digitalx-api`) so every LAUNCHED production pair
	// exists (the tradable universe), instead of the bundle's built-in fixture
	// pairs. Orthogonal to Paper: with Paper it mirrors those pairs LIVE, without
	// it they are on the simulated market. The snapshot is cached beside the db
	// (see marketCachePath), so a repeated (re)seed reuses it instead of
	// refetching. Affects initialization only — an existing database keeps its
	// pairs; combine with Fresh to reseed. Needs network.
	AllPairs bool
	// Fresh starts from a brand-new database: any running server for it is
	// stopped first, then the database and its sidecars are deleted before the
	// normal bring-up initializes a new one (in the mode Paper selects).
	// Destructive by design — balances, orders, and trades are discarded; the
	// seeded keys are deterministic, so the imported key stays valid.
	Fresh bool
	// PassThrough are extra args forwarded to the bundle's `run` (e.g.
	// --history-lag-ms), after the CLI-managed ones.
	PassThrough []string
	// SkipVersionCheck disables the min-version gate on start (the bundle is run
	// without MinVersionEnv), so a known-older bundle can still be started.
	SkipVersionCheck bool
}

// Deps are the injectable dependencies, so the manager runs under test with a
// fake runtime, stub HTTP client, and fixed clock.
type Deps struct {
	// Doer downloads the bundle and the managed Deno, and probes /v2/time.
	Doer Doer
	// Now is the clock (UnixMilli).
	Now func() int64
	// Log receives one-line USER-FACING progress notes (downloads, the resolved
	// command line, reuse) — program output the CLI routes to a Note, NOT a
	// level-gated log. Kept as a func(string) seam.
	Log func(string)
	// Logger receives structured operational DIAGNOSTICS about the lifecycle —
	// runtime resolution, the resolved command line, detached spawn pid, readiness
	// polling, pidfile reads, the reuse/spawn decision, the port-collision
	// fallback, stop/kill, and the version gate. nil = silent (logging.Or). It is
	// distinct from Log: Log is program output, Logger is --debug telemetry.
	Logger *slog.Logger
	// LookPath resolves an executable (exec.LookPath); tests stub it to control
	// whether a system `deno` appears present.
	LookPath func(string) (string, error)
	// DenoURL / DenoVersion override the managed-Deno pin (DIGITALX_CLI_DENO_*).
	DenoURL     string
	DenoVersion string
	// KeyManager imports the seeded key; the caller wires it over DIGITALX_CLI_HOME.
	KeyManager *keys.Manager
	// BannerOut, if set, receives the short license notice at the top of `start`
	// (rendered by the bundle's own `license --show-banner`, so the text is never
	// reproduced in the CLI). Wired to the CLI's stderr; nil disables the banner.
	BannerOut io.Writer
	// DefaultBackend is the keystore backend for the imported key; the sandbox
	// always forces "file" (headless/parallel-safe), so this is informational.
}

// Manager performs sandbox operations for one Config.
type Manager struct {
	cfg  Config
	deps Deps
}

// New builds a Manager, applying defaults.
func New(cfg Config, deps Deps) *Manager {
	if deps.Now == nil {
		deps.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if deps.LookPath == nil {
		deps.LookPath = realLookPath
	}
	if deps.Doer == nil {
		deps.Doer = http.DefaultClient
	}
	return &Manager{cfg: cfg, deps: deps}
}

// log returns the manager's operational logger, never nil (logging.Or).
func (m *Manager) log() *slog.Logger { return logging.Or(m.deps.Logger) }

// StateDir is the per-home mutable state root (DIGITALX_CLI_HOME/sandbox): the db
// (+ its -wal/-shm/-pid sidecars) and run.log. Exported so a caller cleaning up
// after the sandbox (e.g. `self uninstall`) removes the same directory this
// manager writes, without duplicating the "sandbox" subdir name.
func StateDir(home string) string { return filepath.Join(home, "sandbox") }

// stateDir is DIGITALX_CLI_HOME/sandbox — the per-home mutable state root.
func (m *Manager) stateDir() string { return StateDir(m.cfg.Home) }

// DBDefaultName is the sandbox database's name under the state dir. It carries
// no product name: the CLI home above it already says whose data it is.
// LegacyDBFileName is the name a state dir under a home created with the earlier
// product's directory name carries, used for exactly one home (see DBFileName).
const (
	DBDefaultName    = "sandbox.db"
	LegacyDBFileName = "korbit-sandbox.db"
)

// DBFileName is the sandbox database's filename inside home's state dir:
// DBDefaultName, except under a home whose own directory name is the earlier
// product's (config.LegacyLayout), where it is LegacyDBFileName so an older
// `korbit` binary still sharing that directory serves the same database.
//
// It takes the HOME, not the state dir, because the home's name is what decides
// the layout — the state dir is always named "sandbox".
func DBFileName(home string) string {
	if config.LegacyLayout(home) {
		return LegacyDBFileName
	}
	return DBDefaultName
}

// dbCompanions are the files SQLite and the bundle keep beside the database:
// the write-ahead log, its shared-memory index, the {pid,port} pidfile, and the
// market snapshot cache. They belong to the database — none of them may be left
// pointing at a database that has moved or gone.
var dbCompanions = []string{"-wal", "-shm", "-pid", ".market-snapshot.json"}

// DBCompanionSuffixes returns the suffixes of every file that belongs beside a
// sandbox database (see dbCompanions), so a caller moving or removing the
// database takes them with it. It is exported because the set is documented for
// users too — MIGRATION.md's hand-rename recipe lists exactly these — and a
// second copy of the list would be one that could go stale.
func DBCompanionSuffixes() []string { return append([]string{}, dbCompanions...) }

// dbPath is the sandbox database path — the explicit override, else the one the
// state dir's layout implies (DBFileName). The directory decides the filename
// and nothing is renamed on open, so this is a pure path computation.
func (m *Manager) dbPath() string {
	if m.cfg.DB != "" {
		return m.cfg.DB
	}
	return filepath.Join(m.stateDir(), DBFileName(m.cfg.Home))
}

// pidfileSuffix is what the bundle appends to the database path to get its
// {pid,port} file.
const pidfileSuffix = "-pid"

// pidfileCandidates are the pidfiles that could belong to this state dir, the
// one this home's own layout implies FIRST. An explicit --db has exactly one; a
// default state dir may carry the other spelling too, left by an older binary or
// by a home renamed without its files, and a stale one there must be visible
// rather than silently outranked.
func (m *Manager) pidfileCandidates() []string {
	if m.cfg.DB != "" {
		return []string{m.cfg.DB + pidfileSuffix}
	}
	inUse := DBFileName(m.cfg.Home)
	names := []string{inUse}
	for _, other := range []string{DBDefaultName, LegacyDBFileName} {
		if other != inUse {
			names = append(names, other)
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(m.stateDir(), n+pidfileSuffix))
	}
	return out
}

// pidfilePath is where the bundle writes {pid,port} — <db>-pid.
func (m *Manager) pidfilePath() string { return m.dbPath() + pidfileSuffix }

// logPath is the detached child's captured stdout/stderr.
func (m *Manager) logPath() string { return filepath.Join(m.stateDir(), "run.log") }

func (m *Manager) keyName() string {
	if m.cfg.KeyName != "" {
		return m.cfg.KeyName
	}
	return DefaultKeyName
}

// denoManager builds the managed-Deno manager over the shared cache. The
// user-facing download note rides deps.Log (program output → Note); the
// structured operational diagnostics (asset URL, sha256 verify, install) ride
// deps.Logger (the level-gated slog logger).
func (m *Manager) denoManager() *deno.Manager {
	return deno.NewManager(m.cfg.CacheDir, m.deps.Doer, deno.Config{
		Version: m.deps.DenoVersion,
		BaseURL: m.deps.DenoURL,
	}, m.deps.Now, m.deps.Log, m.deps.Logger)
}

// readPidfile reads and parses the bundle's pidfile, or an error if absent/bad.
func (m *Manager) readPidfile() (Pidfile, error) {
	pf, err := parsePidfile(m.pidfilePath())
	if err != nil {
		return Pidfile{}, err
	}
	m.log().Debug("read sandbox pidfile", "path", m.pidfilePath(), "pid", pf.PID, "port", pf.Port)
	return pf, nil
}

// parsePidfile reads the bundle's {pid,port} pidfile at an explicit path, for
// the callers that walk pidfileCandidates rather than the one path in use.
func parsePidfile(path string) (Pidfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Pidfile{}, err
	}
	var pf Pidfile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return Pidfile{}, fmt.Errorf("sandbox pidfile %s is malformed: %w", path, err)
	}
	return pf, nil
}

// denoRunPerms is the least-privilege Deno permission set the sandbox bundle is
// run with (under either Deno runtime) — replacing a blanket --allow-all with
// exactly what the bundle's subcommands (run/init-db/status/exec) need, verified
// empirically against the pinned Deno. One uniform set covers every subcommand
// (init-db/exec don't open a socket, but granting loopback is harmless and avoids
// per-subcommand special-casing). The scopes:
//
//   - --no-prompt: fail closed instead of hanging the detached `run` on an
//     interactive permission prompt.
//   - --allow-read: only the state dir (the db + its -wal/-shm/-pid sidecars). The
//     bundle itself needs no read grant — Deno's entry module (a local file or a
//     fetched URL alike) is loaded by the runtime, exempt from --allow-read — and
//     its own module cache (DENO_DIR) is likewise runtime-managed. No home/CWD read.
//   - --allow-write: only the state dir (db sidecars + pidfile). run.log is
//     written by this process, not the bundle.
//   - --allow-net: loopback — IPv6 "[::1]" and IPv4 "127.0.0.1" (the server binds
//     loopback and `status` probes the loopback port) — plus
//     "*.digitalx.miraeasset.com" (the API hosts the bundle reaches for a live
//     seed or feed) and their "*.korbit.co.kr" alias hosts. No other external
//     host and no non-loopback interface is reachable, so the bundle can't phone
//     home elsewhere or be reached from the LAN. Not port-scoped because the
//     collision fallback binds an OS-assigned ephemeral port (--port 0). The
//     alias hosts are what a pre-1.4.0 bundle reaches for (a bundle pinned via
//     the URL override and started with --skip-version-check); drop that grant
//     once no pre-1.4.0 bundle is in use.
//   - --allow-env: only the bundle's own config knobs — both the
//     DIGITALX_SANDBOX_* namespace and the KORBIT_SANDBOX_* one it also reads
//     (prefix wildcards, so they never drift from the bundle's config) — plus
//     NODE_OPTIONS (read by the node-version precheck) and the POSIX locale vars
//     (LANG/LANGUAGE/LC_ALL/LC_MESSAGES) the bundle reads to pick the
//     banner/`license` language. No blanket env access — secrets in the
//     environment stay unreadable.
//   - --allow-sys: only osRelease, cpus, and systemMemoryInfo (host facts the
//     bundle reads, e.g. on the production-seeding path). No blanket --allow-sys.
//
// Deliberately NOT granted: --allow-run (no subprocesses), --allow-ffi, and
// --allow-import (the bundle is a single self-contained file with no remote imports
// — even when itself fetched from a URL, the main module load is not an "import").
func (m *Manager) denoRunPerms() []string {
	return []string{
		"--no-prompt",
		"--allow-read=" + m.stateDir(),
		"--allow-write=" + m.stateDir(),
		"--allow-net=[::1],127.0.0.1,*.digitalx.miraeasset.com,*.korbit.co.kr",
		"--allow-env=" + sandboxEnvPrefix + "*," + legacySandboxEnvPrefix + "*,NODE_OPTIONS,LANG,LANGUAGE,LC_ALL,LC_MESSAGES",
		"--allow-sys=osRelease,cpus,systemMemoryInfo",
	}
}

// runArgs assembles the bundle `run` arguments: the managed db, the port, and
// any pass-through tuning. port 0 ⇒ ephemeral bind (collision fallback).
func (m *Manager) runArgs(port int) []string {
	args := []string{"run", "--db", m.dbPath(), "--port", fmt.Sprintf("%d", port)}
	return append(args, m.cfg.PassThrough...)
}

// loopbackURLs returns the REST and WS loopback base URLs for a bound port. The
// sandbox command is the SOLE enforcer that only loopback URLs are ever written
// onto a key — keys.SetBaseURL itself accepts any host, so this must stay the
// only path that constructs them.
func loopbackURLs(port int) (rest, ws string) {
	return fmt.Sprintf("http://127.0.0.1:%d", port), fmt.Sprintf("ws://127.0.0.1:%d", port)
}

// initDBArgs builds the `init-db` invocation for an uninitialized db. AllPairs
// seeds from a live production snapshot (`--mode digitalx-api`) so every launched
// pair exists rather than just the bundle fixtures, and points the bundle at a
// snapshot cache (`--market-cache`) beside the db so a repeated (re)seed reuses
// the fetched pair set + tick policies instead of refetching them. Paper mirrors
// the seeded pairs' market data LIVE (`--source live`). Both need network, and
// the bundle fails with an actionable message when offline or when no pair is
// launched.
func (m *Manager) initDBArgs() []string {
	args := []string{"init-db", "--db", m.dbPath()}
	if m.cfg.AllPairs {
		args = append(args, "--mode", "digitalx-api", "--market-cache", m.marketCachePath())
	}
	if m.cfg.Paper {
		args = append(args, "--source", "live")
	}
	return args
}

// marketCachePath is where the digitalx-api snapshot cache lives: a file beside the
// db, distinct from the db and its -wal/-shm/-pid sidecars so `freshen` (which
// deletes only those) leaves it in place — a repeated `start --all-pairs --fresh`
// reseeds from the cache without refetching. It sits in the db's directory, which
// the bundle can already read/write (denoRunPerms grants the state dir), so no
// widening of the least-privilege permission set is needed.
func (m *Manager) marketCachePath() string { return m.dbPath() + ".market-snapshot.json" }

// nonPaperPairs lists the pairs a paper-trading start would leave on the
// simulated walk — the pairs whose market source is not "live". An empty
// result means the database is fully in paper mode (or has no pairs at all).
func nonPaperPairs(doc statusDoc) []string {
	var out []string
	for _, p := range doc.Markets.Pairs {
		if p.MarketSource != "live" {
			out = append(out, p.Symbol)
		}
	}
	return out
}

// execArgs builds a pass-through `exec` invocation against the managed db,
// injecting --db unless the user already named one (as --db or --db=...). --db is
// placed right after the subcommand (the first non-flag token), so a leading flag
// like --help is preserved as the bundle's first argument instead of being
// displaced; a tail that is all flags (e.g. just --help) gets --db appended.
func (m *Manager) execArgs(args []string) []string {
	for _, a := range args {
		if a == "--db" || strings.HasPrefix(a, "--db=") {
			return args
		}
	}
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			out := append([]string{}, args[:i+1]...)
			out = append(out, "--db", m.dbPath())
			return append(out, args[i+1:]...)
		}
	}
	return append(append([]string{}, args...), "--db", m.dbPath())
}

// sourceURL is the configured bundle source, defaulting to the Official Source.
func (m *Manager) sourceURL() string {
	if m.cfg.URL != "" {
		return m.cfg.URL
	}
	return DefaultSandboxURL
}

// ensureBundleAndRuntime resolves the runtime and the bundle reference to hand
// Deno. Both runtimes run the bundle straight from its source (the Official-Source
// URL by default, or a local path / file:// override) — Deno fetches+caches a
// remote URL itself, so the CLI never downloads or caches the bundle. Returned for
// start/exec reuse.
func (m *Manager) ensureBundleAndRuntime(ctx context.Context) (bundleRef string, rt Runtime, err error) {
	rt, err = resolveRuntime(m.cfg.RuntimePref, m.deps.LookPath, m.denoManager())
	if err != nil {
		return "", Runtime{}, err
	}
	return m.sourceURL(), rt, nil
}

// withRuntimeEnv applies a runtime's extra environment to a child command (e.g.
// Deno's DENO_DIR), rebuilding from os.Environ so the child still inherits the
// ambient environment it needs (PATH, locale, proxy settings, …). The CLI owns
// the bundle's config namespaces end to end: every inherited var in either
// sandboxEnvPrefixes namespace is dropped, and only the ones the manager passes
// in extra (e.g. versionGateEnv, LicenseCmdEnv) reach the bundle. This keeps the
// bundle's config knobs fully CLI-controlled — in particular a stray
// user-exported MinVersionEnv (under either spelling) can't arm the bundle's
// version refusal on a non-gated command (status/license/exec).
func withRuntimeEnv(cmd *exec.Cmd, extra []string) {
	environ := os.Environ()
	base := make([]string, 0, len(environ)+len(extra))
	for _, e := range environ {
		if hasSandboxEnvPrefix(e) {
			continue
		}
		base = append(base, e)
	}
	cmd.Env = append(base, extra...)
}

// hasSandboxEnvPrefix reports whether an environment entry belongs to one of the
// bundle config namespaces the CLI owns.
func hasSandboxEnvPrefix(entry string) bool {
	for _, p := range sandboxEnvPrefixes {
		if strings.HasPrefix(entry, p) {
			return true
		}
	}
	return false
}

// dbInitialized reports whether the sandbox db file exists (a proxy for
// initialized; the bundle's init-db is idempotent so a re-run is harmless).
func (m *Manager) dbInitialized() bool {
	_, err := os.Stat(m.dbPath())
	return err == nil
}

// versionTooOldError signals the bundle refused to start because it is older
// than MinSandboxVersion (the MinVersionEnv gate). It is recovered by updating
// the bundle and retrying once (bringUpWithRecovery); `have` is the bundle's
// reported version, "" if it couldn't be parsed.
type versionTooOldError struct{ have string }

func (e *versionTooOldError) Error() string {
	have := e.have
	if have == "" {
		have = "an older version"
	}
	return fmt.Sprintf("the sandbox bundle is %s, but this CLI requires %s or newer", have, MinSandboxVersion)
}

// isVersionTooOld reports whether err is (or wraps) a versionTooOldError.
func isVersionTooOld(err error) bool {
	var vt *versionTooOldError
	return errors.As(err, &vt)
}

// versionTooOldFrom returns a *versionTooOldError when captured bundle output
// carries the min-version refusal prefix, parsing the reported version from it;
// nil otherwise. (Output-classification, the same approach as
// classifyStartupFailure, so it works for the detached run captured to the log.)
func versionTooOldFrom(out string) *versionTooOldError {
	if !strings.Contains(out, VersionTooOldPrefix) {
		return nil
	}
	return &versionTooOldError{have: parseSandboxVersion(out)}
}

// bundleCorruptError signals Deno failed to load the remote bundle as a JS module
// because its cached body is not JavaScript — classically an HTML page served for
// the bundle URL (e.g. the docs site's SPA index during a deploy gap, before the
// bundle is published there). Deno keeps that body in its module cache, so every
// start re-parses it. Recovered by force-refreshing the cache (deno cache
// --reload) and retrying once (bringUpWithRecovery); a local/file:// source can't
// be a stale cache and is never classified this way.
type bundleCorruptError struct{}

func (e *bundleCorruptError) Error() string {
	return "the cached sandbox bundle is not valid JavaScript — it looks like an HTML page"
}

// bundleCorruptFrom returns a *bundleCorruptError when captured Deno output shows
// the remote bundle failing to parse because its body is HTML rather than JS — the
// tell is the markup Deno echoes on the offending source line (`1 | <!DOCTYPE
// html>`). Output-classification, the same approach as classifyStartupFailure, so
// it works for the detached run captured to the log.
func bundleCorruptFrom(out string) *bundleCorruptError {
	l := strings.ToLower(out)
	if strings.Contains(l, "<!doctype") || strings.Contains(l, "<html") {
		return &bundleCorruptError{}
	}
	return nil
}

// isBundleCorrupt reports whether err is (or wraps) a bundleCorruptError.
func isBundleCorrupt(err error) bool {
	var bc *bundleCorruptError
	return errors.As(err, &bc)
}

// parseSandboxVersion finds the bare MAJOR.MINOR.PATCH the bundle prints on the
// refusal path (it writes its version on its own line, exactly like `version`),
// returning the first such token or "".
func parseSandboxVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); semverLead.MatchString(t) {
			return t
		}
	}
	return ""
}

// semverLead matches a line that is exactly a MAJOR.MINOR.PATCH version (with an
// optional -prerelease/+build suffix) — the bundle's bare version line.
var semverLead = regexp.MustCompile(`^\d+\.\d+\.\d+(\S*)$`)

// classifyStartupFailure scans a captured run.log tail for the bundle's stable
// stderr prefixes and returns a precise message, or "" if none matched.
func classifyStartupFailure(logTail string) string {
	switch {
	case strings.Contains(logTail, PrecheckPrefix):
		// Surface the bundle's own precheck line verbatim (it carries the exact
		// upgrade command) rather than second-guessing the runtime floor.
		for _, line := range strings.Split(logTail, "\n") {
			if strings.Contains(line, PrecheckPrefix) {
				return strings.TrimSpace(line)
			}
		}
		return "the runtime failed the sandbox precheck"
	case strings.Contains(logTail, NotInitializedSubstr):
		return "the sandbox database is not initialized"
	case strings.Contains(logTail, AlreadyRunningSubstr):
		return "a sandbox may already be running for this database"
	case strings.Contains(logTail, SchemaMismatchSubstr):
		return "the sandbox database has a schema version this build can't run — re-run start with --fresh to recreate it"
	}
	return ""
}
