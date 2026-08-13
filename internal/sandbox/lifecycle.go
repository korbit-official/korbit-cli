// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/sandbox/deno"
)

// StartResult reports a completed `sandbox start`.
type StartResult struct {
	PID         int    `json:"pid"`
	Port        int    `json:"port"`
	RestBaseURL string `json:"restBaseUrl"`
	WSBaseURL   string `json:"wsBaseUrl"`
	KeyName     string `json:"keyName"`
	APIKeyID    string `json:"apiKeyId"`
	Runtime     string `json:"runtime"`
	Bundle      string `json:"bundle"`
	DB          string `json:"db"`
	LogPath     string `json:"logPath"`
	Imported    bool   `json:"imported"`
	// Paper reports the started database's ACTUAL mode, read from the bundle's
	// status — not merely whether --paper was passed. True when the database
	// was initialized for paper trading (`init-db --source live`), or when
	// every configured pair mirrors live production market data.
	Paper bool `json:"paper"`
	// WalkPairs lists the pairs still on the simulated walk while Paper is true
	// — pairs production reported non-launched when the database was
	// initialized, so their market data is not mirrored. Omitted from the JSON
	// (not an empty array) for a fully-live paper database and when Paper is
	// false.
	WalkPairs []string `json:"walkPairs,omitempty"`
	// PairCount is the total number of configured pairs.
	PairCount int `json:"pairCount"`
	// Recreated reports that --fresh deleted an existing database before the
	// start (previous balances/orders/trades were discarded). False when
	// --fresh found no database to delete.
	Recreated bool `json:"recreated"`
}

// readinessTimeout bounds the /v2/time poll after spawning the child.
const readinessTimeout = 20 * time.Second

// readinessInterval is the /v2/time poll cadence.
const readinessInterval = 150 * time.Millisecond

// Start ensures the bundle+runtime, initializes the db if needed, spawns the
// bundle's `run` detached, polls /v2/time for readiness, then imports and
// (re)pins the seeded Ed25519 key. The bound port comes from the pidfile (the
// single source of truth), so a --port 0 collision fallback transparently
// updates the summary and the key pin.
func (m *Manager) Start(ctx context.Context) (StartResult, error) {
	// The imported seeded key is always a sandbox key, so its name must advertise
	// the sandbox (keys.AssertSandboxKeyName). Validate the resolved --key-name up
	// front, so a bad name fails before anything is spawned rather than after a
	// successful start leaves a running server the key import then refuses.
	if err := keys.AssertSandboxKeyName(m.keyName()); err != nil {
		return StartResult{}, err
	}
	if err := os.MkdirAll(m.stateDir(), 0o755); err != nil {
		return StartResult{}, err
	}
	bundle, rt, err := m.ensureBundleAndRuntime(ctx)
	if err != nil {
		return StartResult{}, err
	}
	m.log().Info("sandbox runtime resolved", "runtime", string(rt.Kind), "denoPath", m.resolvedDenoPath(rt.Kind), "bundle", bundle)

	// Refresh the cache README + acknowledge any pending network fetch, then show
	// the short license notice — on every start (reuse or fresh), before bring-up.
	// The banner is the bundle's own `license --show-banner` output, so the CLI
	// reproduces no license text (pointer, never body).
	m.announceBundleSource(rt)
	m.showBanner(ctx)

	// Fresh: stop any running server (a wedged one included — the user asked
	// for a clean slate) and delete the database, BEFORE the paper check and
	// the reuse guard, so the bring-up below initializes a brand-new db in the
	// requested mode.
	recreated := false
	if m.cfg.Fresh {
		var err error
		if recreated, err = m.freshen(ctx); err != nil {
			return StartResult{}, err
		}
	}

	// Paper mode never flips an existing database in place — an initialized db
	// keeps whatever mode it was created with, so verify it BEFORE any bring-up
	// (including the reuse path below) and fail with the recreate/flip options.
	if err := m.ensurePaperDB(ctx, rt, bundle); err != nil {
		return StartResult{}, err
	}

	// Never spawn a second server for a database that already has a live one.
	// spawnAndWait overwrites the shared pidfile, so a duplicate would orphan the
	// running instance — its pidfile lost, `stop` could no longer find it. (The
	// bundle's own collision guard can't help: the CLI clears the pidfile before
	// the bundle runs.) A re-start of a HEALTHY server is idempotent: reuse it and
	// just re-pin the key. A wedged one (alive but not answering) is left for the
	// user to stop deliberately rather than clobbered.
	if pf, perr := m.readPidfile(); perr == nil && pf.Port != 0 && processAlive(pf.PID) {
		if !m.probeReady(ctx, pf.Port) {
			m.log().Warn("existing sandbox alive but unresponsive — refusing to start", "pid", pf.PID, "port", pf.Port)
			return StartResult{}, fmt.Errorf("a sandbox (pid %d) is already running for this database but is not responding on port %d — stop it (`sandbox stop`, or kill %d) before starting again", pf.PID, pf.Port, pf.PID)
		}
		m.log().Info("reusing running sandbox", "pid", pf.PID, "port", pf.Port)
		if m.deps.Log != nil {
			m.deps.Log(fmt.Sprintf("sandbox already running (pid %d, port %d) — reusing it", pf.PID, pf.Port))
		}
		return m.finishStart(ctx, rt, bundle, pf)
	}

	pf, err := m.bringUpWithRecovery(ctx, rt, bundle)
	if err != nil {
		return StartResult{}, err
	}
	res, err := m.finishStart(ctx, rt, bundle, pf)
	res.Recreated = recreated
	return res, err
}

// freshen implements --fresh: stop any running server for this database (the
// same graceful-then-SIGKILL path as `sandbox stop`, so even a wedged one goes
// down), then delete the database and its sidecars — strictly the resolved db
// path plus the -wal/-shm/-pid files sqlite and the bundle put beside it.
// Returns whether a database actually existed and was deleted.
func (m *Manager) freshen(ctx context.Context) (recreated bool, err error) {
	if _, err := m.Stop(ctx); err != nil {
		return false, fmt.Errorf("stopping the running sandbox before recreating its database: %w", err)
	}
	dbPath := m.dbPath()
	if rerr := os.Remove(dbPath); rerr == nil {
		recreated = true
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return false, fmt.Errorf("deleting the sandbox database %s: %w", dbPath, rerr)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm", dbPath + "-pid"} {
		if rerr := os.Remove(sidecar); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return recreated, fmt.Errorf("deleting %s: %w", sidecar, rerr)
		}
	}
	if recreated {
		m.log().Info("recreated the sandbox database", "db", dbPath)
		if m.deps.Log != nil {
			m.deps.Log("recreating the sandbox database — previous balances, orders, and trades are discarded")
		}
	}
	return recreated, nil
}

// withBundleRecovery runs one bundle invocation and recovers ONCE from a corrupt
// cached bundle: Deno's module cache holds HTML (or otherwise non-JS) for a
// remote bundle URL — e.g. the docs SPA index served during a deploy gap. It
// force-refreshes the cache (deno cache --reload) and re-runs; a still-bad body
// surfaces an actionable error.
//
// EVERY bundle invocation `start` makes goes through here — the license banner,
// the `status --json` reads (readStatusDoc), and the bring-up — because the
// poisoned cache entry fails all of them alike, and which one runs first depends
// on the flags and on whether a server is already up. Any other failure, and a
// corrupt-classified LOCAL source (which can't be a stale cache), passes straight
// through. The retry is per-invocation: a heal by the first one leaves nothing
// for the rest to recover from, and when the source is genuinely serving a web
// page each subsequent invocation re-attempts the refresh — cheap, because what
// it re-fetches in that case is that page, not the bundle.
func (m *Manager) withBundleRecovery(ctx context.Context, run func() error) error {
	err := run()
	if !isBundleCorrupt(err) {
		return err
	}
	// Only a remote source caches, so a local/file:// bundle can't be a stale
	// cache — reloading it is a no-op and retrying would fail identically.
	if localPath(m.sourceURL()) != "" {
		return err
	}
	m.log().Warn("cached sandbox bundle is not valid JavaScript — refreshing and retrying once", "detail", err.Error())
	if m.deps.Log != nil {
		m.deps.Log(err.Error() + " — refreshing the bundle and retrying once")
	}
	if uerr := m.reloadDenoBundle(ctx); uerr != nil {
		return fmt.Errorf("%w; refreshing it failed too: %v", err, uerr)
	}
	rerr := run()
	if isBundleCorrupt(rerr) {
		return fmt.Errorf("%w even after refreshing it from %s — the source may be serving a web page instead of the bundle; check the URL and your network", rerr, m.sourceURL())
	}
	return rerr
}

// bringUpWithRecovery runs the bundle's init-db (if needed) + detached `run`,
// recovering from the two startup failures a cache refresh fixes: the corrupt
// cached bundle (withBundleRecovery, shared with every other bundle invocation)
// and the min-version gate (MinVersionEnv) — the bundle reports itself too old,
// so it updates the bundle from the source and retries once; a still-too-old
// bundle, or a failed update, surfaces an actionable error pointing at
// --skip-version-check.
//
// The corrupt-cache recovery runs first (it wraps the bring-up), so a refreshed
// bundle that then reports itself too old still gets the version recovery and its
// actionable error — at the cost of a second refresh of the same bundle. Rare
// enough to leave simple: it needs the source to have served a web page AND the
// bundle behind it to be outdated.
func (m *Manager) bringUpWithRecovery(ctx context.Context, rt Runtime, bundle string) (Pidfile, error) {
	var pf Pidfile
	err := m.withBundleRecovery(ctx, func() error {
		var berr error
		pf, berr = m.bringUp(ctx, rt, bundle)
		return berr
	})
	var vt *versionTooOldError
	if err == nil || !errors.As(err, &vt) {
		return pf, err
	}
	// SkipVersionCheck leaves MinVersionEnv unset, so the bundle can't raise
	// this; guard anyway so a stray signal can't loop.
	if m.cfg.SkipVersionCheck {
		return Pidfile{}, err
	}
	m.log().Warn("sandbox bundle too old — auto-updating and retrying once", "detail", err.Error())
	if m.deps.Log != nil {
		m.deps.Log(err.Error() + " — updating the bundle and retrying once")
	}
	if uerr := m.reloadDenoBundle(ctx); uerr != nil {
		return Pidfile{}, fmt.Errorf("%w; updating the bundle failed too: %v — re-run with --skip-version-check to start anyway", err, uerr)
	}
	pf, err = m.bringUp(ctx, rt, bundle)
	if err != nil && errors.As(err, &vt) {
		return Pidfile{}, fmt.Errorf("%w even after updating the bundle — re-run with --skip-version-check to start anyway, or update %s", err, progname.Name())
	}
	return pf, err
}

// bringUp initializes the database if absent, then spawns the detached `run` on
// the requested/default port (with the OS-assigned ephemeral fallback on the
// default). A version-too-old or corrupt-bundle failure is NOT a port collision,
// so it skips the fallback and propagates for the recovery caller to handle.
func (m *Manager) bringUp(ctx context.Context, rt Runtime, bundle string) (Pidfile, error) {
	if !m.dbInitialized() {
		if err := m.runInitDB(ctx, rt, bundle); err != nil {
			return Pidfile{}, err
		}
	}

	// First attempt the requested/default port; on a fixed port there is no
	// fallback, on the default we retry with --port 0 (atomic ephemeral bind).
	wantPort := m.cfg.Port
	fixed := wantPort != 0
	if wantPort == 0 {
		wantPort = DefaultPort
	}
	pf, err := m.spawnAndWait(ctx, rt, bundle, wantPort)
	if err != nil && !fixed && !isVersionTooOld(err) && !isBundleCorrupt(err) {
		// Collision (or an early bind failure): re-spawn letting the OS assign a
		// free port. The bundle writes the real port back to the pidfile. A
		// version-too-old or corrupt-bundle failure is NOT a port collision, so it
		// skips the fallback (the recovery caller refreshes and retries instead).
		m.log().Warn("sandbox port unavailable — retrying on an OS-assigned port", "port", wantPort)
		if m.deps.Log != nil {
			m.deps.Log(fmt.Sprintf("port %d unavailable — retrying on an OS-assigned port", wantPort))
		}
		pf, err = m.spawnAndWait(ctx, rt, bundle, 0)
	}
	return pf, err
}

// runInitDB runs the bundle's `init-db` in the foreground under the min-version
// gate, capturing its output so a version refusal is detected (and appending it
// to run.log for support). A version-too-old build returns a *versionTooOldError.
func (m *Manager) runInitDB(ctx context.Context, rt Runtime, bundle string) error {
	bin, argv, env, err := rt.command(ctx, m.denoRunPerms(), bundle, m.initDBArgs()...)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, argv...)
	withRuntimeEnv(cmd, append(env, m.versionGateEnv()...))
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	if logF, e := os.OpenFile(m.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); e == nil {
		_, _ = logF.Write(buf.Bytes())
		logF.Close()
	}
	if runErr == nil {
		return nil
	}
	if vt := versionTooOldFrom(buf.String()); vt != nil {
		return vt
	}
	if bc := bundleCorruptFrom(buf.String()); bc != nil {
		return bc
	}
	return fmt.Errorf("initializing the sandbox database: %w (see %s)", runErr, m.logPath())
}

// versionGateEnv is the env that arms the bundle's min-version refusal on the
// start invocations (init-db / run); empty when the check is skipped.
func (m *Manager) versionGateEnv() []string {
	if m.cfg.SkipVersionCheck {
		return nil
	}
	return []string{MinVersionEnv + "=" + MinSandboxVersion}
}

// ensurePaperDB gates a --paper start on an ALREADY-initialized database: the
// paper flag configures initialization only (initDBArgs), so an existing db
// must already be in paper mode or the start refuses with the two ways out
// (recreate, or flip pairs in place). A missing db passes — init-db will create
// it in paper mode. No-op without --paper: a db keeps whatever mode it has.
func (m *Manager) ensurePaperDB(ctx context.Context, rt Runtime, bundle string) error {
	if !m.cfg.Paper || !m.dbInitialized() {
		return nil
	}
	doc, err := m.readStatusDoc(ctx, rt, bundle)
	if err != nil {
		return err
	}
	// A database initialized with `init-db --source live` IS a paper database,
	// even when some pairs stayed on the simulated walk (production reported
	// them non-launched at initialization; the start result names them in
	// WalkPairs). Only a walk-initialized database — or one from a bundle that
	// does not report the field — falls through to the per-pair check.
	if doc.Markets.InitializedSource == "live" {
		return nil
	}
	walk := nonPaperPairs(doc)
	if len(walk) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the existing sandbox database was not initialized for paper trading (%s still use(s) the simulated market) — "+
			"either re-run with --fresh to recreate it in paper mode (sandbox data is disposable), "+
			"or flip pairs in place: `sandbox exec set-market --symbol %s --source live`",
		strings.Join(walk, ", "), walk[0])
}

// finishStart builds the result for a running server (freshly spawned or reused)
// and imports + re-pins the seeded key to its actual bound port — always, even
// when the key already exists, so a reused server still corrects the pin. One
// `status --json` read feeds both the key import and the reported mode: Paper
// is the db's ACTUAL state (initialized for paper trading, or every pair
// mirroring live production data), so a paper db started without --paper still
// reports truthfully — with any non-mirrored pairs named in WalkPairs.
func (m *Manager) finishStart(ctx context.Context, rt Runtime, bundle string, pf Pidfile) (StartResult, error) {
	rest, ws := loopbackURLs(pf.Port)
	res := StartResult{
		PID: pf.PID, Port: pf.Port, RestBaseURL: rest, WSBaseURL: ws,
		KeyName: m.keyName(), Runtime: string(rt.Kind), Bundle: bundle,
		DB: m.dbPath(), LogPath: m.logPath(),
	}
	doc, err := m.readStatusDoc(ctx, rt, bundle)
	if err != nil {
		return res, err
	}
	walk := nonPaperPairs(doc)
	res.PairCount = len(doc.Markets.Pairs)
	res.Paper = doc.Markets.InitializedSource == "live" ||
		(len(doc.Markets.Pairs) > 0 && len(walk) == 0)
	if res.Paper {
		res.WalkPairs = walk
	}
	apiKeyID, imported, err := m.importKey(doc, pf.Port)
	if err != nil {
		return res, err
	}
	res.APIKeyID, res.Imported = apiKeyID, imported
	return res, nil
}

// spawnAndWait spawns the detached `run`, polls readiness, and returns the
// pidfile (the real bound port). On failure it scans the run.log tail for the
// bundle's stable error prefixes to surface a precise message.
func (m *Manager) spawnAndWait(ctx context.Context, rt Runtime, bundle string, port int) (Pidfile, error) {
	// Remove a stale pidfile so we don't read a previous run's port.
	_ = os.Remove(m.pidfilePath())

	bin, argv, env, err := rt.command(ctx, m.denoRunPerms(), bundle, m.runArgs(port)...)
	if err != nil {
		return Pidfile{}, err
	}
	// Show exactly what is being run, including the bundle URL Deno is handed — the
	// sandbox runs remote code, so the command line is not hidden from the user.
	cmdline := strings.Join(append([]string{bin}, argv...), " ")
	m.log().Debug("spawning sandbox", "port", port, "cmd", cmdline, "logPath", m.logPath())
	if m.deps.Log != nil {
		m.deps.Log("running: " + cmdline)
	}
	logF, err := os.OpenFile(m.logPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Pidfile{}, err
	}
	defer logF.Close()

	cmd := exec.Command(bin, argv...)
	withRuntimeEnv(cmd, append(env, m.versionGateEnv()...))
	cmd.Stdout = logF
	cmd.Stderr = logF
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return Pidfile{}, fmt.Errorf("spawning the sandbox: %w", err)
	}
	m.log().Debug("sandbox spawned detached", "pid", cmd.Process.Pid)
	// Reap the child handle in the background so it doesn't linger as a zombie if
	// it exits during readiness (it is detached, so this Wait is just bookkeeping).
	childDone := make(chan error, 1)
	go func() { childDone <- cmd.Wait() }()

	start := time.Now()
	attempt := 0
	deadline := start.Add(readinessTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Pidfile{}, ctx.Err()
		case werr := <-childDone:
			// Child exited before becoming ready — a startup failure. Classify it.
			tail := m.logTail()
			// A min-version refusal is handled by the caller (update + retry), so
			// surface it as the typed error rather than a generic startup failure.
			if vt := versionTooOldFrom(tail); vt != nil {
				return Pidfile{}, vt
			}
			// A cached bundle that is HTML rather than JS is a stale-cache problem
			// (recovered by a refresh + retry), not a bundle-emitted startup line,
			// so classify it before the bundle's own prefixes.
			if bc := bundleCorruptFrom(tail); bc != nil {
				return Pidfile{}, bc
			}
			if msg := classifyStartupFailure(tail); msg != "" {
				m.log().Warn("sandbox failed to start", "reason", msg, "logPath", m.logPath())
				return Pidfile{}, fmt.Errorf("sandbox failed to start: %s (see %s)", msg, m.logPath())
			}
			m.log().Warn("sandbox exited during startup", "err", fmt.Sprint(werr), "logPath", m.logPath())
			return Pidfile{}, fmt.Errorf("sandbox exited during startup: %v (see %s)", werr, m.logPath())
		default:
		}
		attempt++
		if pf, err := m.readPidfile(); err == nil && pf.Port != 0 {
			m.log().Debug("sandbox readiness poll", "attempt", attempt, "port", pf.Port, "elapsedMs", time.Since(start).Milliseconds())
			if m.probeReady(ctx, pf.Port) {
				m.log().Info("sandbox started", "pid", pf.PID, "port", pf.Port, "elapsedMs", time.Since(start).Milliseconds())
				return pf, nil
			}
		}
		time.Sleep(readinessInterval)
	}
	m.log().Warn("sandbox readiness timeout", "timeout", readinessTimeout.String(), "attempts", attempt, "logPath", m.logPath())
	return Pidfile{}, fmt.Errorf("sandbox did not become ready within %s (see %s)", readinessTimeout, m.logPath())
}

// probeReady issues GET /v2/time against the bound port; ok on a 2xx.
func (m *Manager) probeReady(ctx context.Context, port int) bool {
	if m.deps.Doer == nil {
		return false
	}
	rest, _ := loopbackURLs(port)
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rest+"/v2/time", nil)
	if err != nil {
		return false
	}
	resp, err := m.deps.Doer.Do(req)
	if err != nil {
		return false
	}
	if resp.Body != nil {
		resp.Body.Close()
	}
	return resp.StatusCode/100 == 2
}

// logTail returns the last chunk of run.log for failure classification.
func (m *Manager) logTail() string {
	raw, err := os.ReadFile(m.logPath())
	if err != nil {
		return ""
	}
	const max = 8 << 10
	if len(raw) > max {
		raw = raw[len(raw)-max:]
	}
	return string(raw)
}

// StopResult reports a `sandbox stop`.
type StopResult struct {
	Stopped bool `json:"stopped"`
	PID     int  `json:"pid"`
	Port    int  `json:"port"`
}

// Stop reads the pidfile, SIGTERMs the process, and waits for it to exit (the
// bundle removes its own pidfile on a clean shutdown). It is runtime-free.
func (m *Manager) Stop(ctx context.Context) (StopResult, error) {
	pf, err := m.readPidfile()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StopResult{Stopped: false}, nil // nothing running
		}
		return StopResult{}, err
	}
	if !processAlive(pf.PID) {
		_ = os.Remove(m.pidfilePath()) // stale
		return StopResult{Stopped: false, PID: pf.PID, Port: pf.Port}, nil
	}
	proc, err := os.FindProcess(pf.PID)
	if err != nil {
		return StopResult{}, err
	}
	m.log().Debug("stopping sandbox (SIGTERM)", "pid", pf.PID, "port", pf.Port)
	if err := signalStop(proc); err != nil {
		return StopResult{}, err
	}
	// Wait for the graceful exit (pidfile removed by the bundle, or the process
	// to die).
	if waitForExit(pf.PID, 10*time.Second) {
		m.log().Info("sandbox stopped", "pid", pf.PID, "port", pf.Port)
		return StopResult{Stopped: true, PID: pf.PID, Port: pf.Port}, nil
	}
	// SIGTERM was ignored: escalate to SIGKILL (can't be caught) and give it a
	// short final wait before giving up — so a wedged sandbox needs no manual
	// kill -9.
	m.log().Warn("sandbox ignored SIGTERM — escalating to SIGKILL", "pid", pf.PID)
	if err := signalKill(proc); err != nil {
		return StopResult{Stopped: false, PID: pf.PID, Port: pf.Port}, err
	}
	if waitForExit(pf.PID, 2*time.Second) {
		return StopResult{Stopped: true, PID: pf.PID, Port: pf.Port}, nil
	}
	return StopResult{Stopped: false, PID: pf.PID, Port: pf.Port}, fmt.Errorf("sandbox PID %d did not exit even after SIGKILL", pf.PID)
}

// waitForExit polls until the process is gone or the timeout elapses, returning
// true once it has exited.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// ServerState describes whether a sandbox is running for this home's db.
type ServerState struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
	Port    int  `json:"port,omitempty"`
	// Reachable is the result of a /v2/time probe against the bound port.
	Reachable bool `json:"reachable"`
}

// StatusResult is the runtime-free `sandbox status` report: bundle source, the
// resolved runtime + the Deno binary that would run it, the server state, and the
// imported-key state.
type StatusResult struct {
	CachePath    string      `json:"cachePath"`
	BundleCached bool        `json:"bundleCached"`
	SourceURL    string      `json:"sourceUrl,omitempty"`
	Runtime      string      `json:"runtime"`
	DenoPath     string      `json:"denoPath,omitempty"`
	DenoStatus   deno.Status `json:"deno"`
	Server       ServerState `json:"server"`
	DB           string      `json:"db"`
	KeyName      string      `json:"keyName"`
	KeyImported  bool        `json:"keyImported"`
}

// resolvedRuntimeKind reports which runtime a sandbox command would use, without
// downloading or starting anything — the same auto-resolution `start` applies. An
// explicit but unsatisfiable preference (e.g. --runtime deno with no deno) is
// still reported as configured, so `status` shows the user's intent.
func (m *Manager) resolvedRuntimeKind() RuntimeKind {
	rt, err := resolveRuntime(m.cfg.RuntimePref, m.deps.LookPath, m.denoManager())
	if err != nil {
		if m.cfg.RuntimePref != "" {
			return RuntimeKind(m.cfg.RuntimePref)
		}
		return RuntimeManagedDeno
	}
	return rt.Kind
}

// resolvedDenoPath is the Deno binary that the given runtime kind would run — a
// system `deno` (best-effort PATH lookup) or the managed binary path. No download.
func (m *Manager) resolvedDenoPath(kind RuntimeKind) string {
	if kind == RuntimeSystemDeno {
		if p, err := m.deps.LookPath("deno"); err == nil {
			return p
		}
		return "deno" // configured but not (yet) on PATH
	}
	return m.denoManager().Path()
}

// announceBundleSource refreshes the cache README and, before Deno first reaches
// for the bundle, acknowledges the network fetch with its URL — but only for a
// remote source Deno hasn't already cached (a local file never hits the network).
func (m *Manager) announceBundleSource(rt Runtime) {
	writeCacheReadme(m.cfg.CacheDir)
	src := m.sourceURL()
	if localPath(src) != "" {
		return
	}
	if rt.deno != nil && rt.deno.HasModuleCache() {
		return
	}
	m.log().Info("downloading sandbox bundle", "url", src)
	if m.deps.Log != nil {
		m.deps.Log(fmt.Sprintf("downloading the Korbit API Sandbox from %s (Deno caches it for next time) …", src))
	}
}

// Status assembles the runtime-free status report. It does not download anything
// or start a runtime — pure inspection over the cache, pidfile, and a TCP probe.
func (m *Manager) Status(ctx context.Context) StatusResult {
	kind := m.resolvedRuntimeKind()
	res := StatusResult{
		CachePath:  m.cfg.CacheDir,
		Runtime:    string(kind),
		DenoPath:   m.resolvedDenoPath(kind),
		DenoStatus: m.denoManager().Status(),
		SourceURL:  m.sourceURL(),
		DB:         m.dbPath(), KeyName: m.keyName(),
	}
	// Deno fetches+caches the bundle itself (DENO_DIR), so there is no bundle file
	// of ours to stat: report whether Deno has fetched it. A local / file:// source
	// is read fresh on every run, so "cached" there means the file is present.
	if lp := localPath(res.SourceURL); lp != "" {
		_, err := os.Stat(lp)
		res.BundleCached = err == nil
	} else {
		res.BundleCached = m.denoManager().HasModuleCache()
	}
	if pf, err := m.readPidfile(); err == nil {
		res.Server.PID, res.Server.Port = pf.PID, pf.Port
		res.Server.Running = processAlive(pf.PID)
		if res.Server.Running && pf.Port != 0 {
			res.Server.Reachable = m.probeReady(ctx, pf.Port)
		}
	}
	if m.deps.KeyManager != nil {
		if list, err := m.deps.KeyManager.List(); err == nil {
			for _, s := range list {
				if s.Name == m.keyName() && s.IsSandbox {
					res.KeyImported = true
				}
			}
		}
	}
	return res
}

// Update re-fetches the bundle from the Official Source into Deno's module cache
// (`deno cache --reload`, so a stale cached bundle is replaced). It is the license
// seam (a consent gate drops in here later).
func (m *Manager) Update(ctx context.Context) (StatusResult, error) {
	if err := m.reloadDenoBundle(ctx); err != nil {
		return StatusResult{}, err
	}
	return m.Status(ctx), nil
}

// reloadDenoBundle force-refreshes Deno's module cache of the bundle source, using
// the resolved runtime's Deno binary (downloading the managed one only if that is
// the runtime, never just to refresh a system-deno cache). `deno cache` only
// downloads (runs no code), so it needs no permission flags — just DENO_DIR so the
// refresh lands in the same cache the run reads.
func (m *Manager) reloadDenoBundle(ctx context.Context) error {
	// A local / file:// bundle is read fresh from disk on every run, so there is
	// nothing in Deno's module cache to refresh — `deno cache --reload` of a local
	// path is a silent no-op. Only a remote source needs re-fetching.
	if localPath(m.sourceURL()) != "" {
		return nil
	}
	rt, err := resolveRuntime(m.cfg.RuntimePref, m.deps.LookPath, m.denoManager())
	if err != nil {
		return err
	}
	bin, err := rt.denoBin(ctx)
	if err != nil {
		return err
	}
	_, argv := rt.deno.ReloadArgs(m.sourceURL())
	cmd := exec.CommandContext(ctx, bin, argv...)
	withRuntimeEnv(cmd, rt.deno.RunEnv())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("refreshing the sandbox bundle via Deno: %w (%s)", err, bytes.TrimSpace(out))
	}
	return nil
}

// Exec passes args straight through to the bundle under the resolved Deno runtime
// against the managed db, streaming stdout/stderr to the given writers. It avoids
// re-implementing the data-poke subcommands (set-balance, add-pair, …).
func (m *Manager) Exec(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("sandbox exec needs a subcommand, e.g. `set-balance --user 1 --currency btc --available 5`")
	}
	bundle, rt, err := m.ensureBundleAndRuntime(ctx)
	if err != nil {
		return err
	}
	bin, argv, env, err := rt.command(ctx, m.denoRunPerms(), bundle, m.execArgs(args)...)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	withRuntimeEnv(cmd, env)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// License runs the bundle's `license` subcommand and streams its output through.
// Unlike Exec it injects NO --db (license opens no database) — it just needs the
// bundle resolved (and fetched, if remote). extra carries any pass-through flags
// (e.g. --lang ko, or --show-banner for just the short notice); with none the
// bundle follows the locale (LANG/LC_*). LicenseCmdEnv is set so the banner
// footer names this CLI's own `<prog> sandbox license`, not the standalone
// bundle invocation.
func (m *Manager) License(ctx context.Context, extra []string, stdout, stderr io.Writer) error {
	bundle, rt, err := m.ensureBundleAndRuntime(ctx)
	if err != nil {
		return err
	}
	args := append([]string{"license"}, extra...)
	bin, argv, env, err := rt.command(ctx, m.denoRunPerms(), bundle, args...)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	withRuntimeEnv(cmd, append(env, LicenseCmdEnv+"="+m.licenseCommand()))
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// licenseCommand is the command this CLI offers for the full sandbox terms,
// used to override the bundle banner's footer pointer (LicenseCmdEnv).
func (m *Manager) licenseCommand() string { return progname.Name() + " sandbox license" }

// showBanner renders the bundle's short license notice to deps.BannerOut via the
// bundle's own `license --show-banner` — so the notice text is never reproduced
// in the CLI (the "pointer, never body" rule). Best-effort: a failure to render
// the banner must not block `start` (the real fetch/runtime errors surface from
// the start sequence itself), so the caller ignores the error.
//
// It is `start`'s FIRST bundle invocation, so it is also the first to meet a
// corrupt cached bundle — hence the same refresh-and-retry as the rest
// (withBundleRecovery), which both keeps the notice printing and heals the cache
// before anything downstream reaches for it. That refresh is a network fetch, so
// "must not block" here means must not FAIL the start, not that the step is
// free.
//
// Classifying the failure needs the output, so the notice is buffered rather than
// streamed. Only a corrupt bundle withholds it — its output is a Deno stack trace,
// not a notice. Every other failure still relays whatever the bundle wrote, as
// streaming did: a bundle that printed the notice and then exited non-zero for an
// unrelated reason must not cost the user the notice.
func (m *Manager) showBanner(ctx context.Context) {
	if m.deps.BannerOut == nil {
		return
	}
	var buf bytes.Buffer
	err := m.withBundleRecovery(ctx, func() error {
		buf.Reset()
		lerr := m.License(ctx, []string{"--show-banner"}, &buf, &buf)
		if lerr == nil {
			return nil
		}
		if bc := bundleCorruptFrom(buf.String()); bc != nil {
			return bc
		}
		return lerr
	})
	if isBundleCorrupt(err) {
		return
	}
	_, _ = m.deps.BannerOut.Write(buf.Bytes())
}

// RuntimeStatus reports the managed-Deno cache status (no network call).
func (m *Manager) RuntimeStatus() deno.Status { return m.denoManager().Status() }

// RuntimeInstall ensures the managed Deno is present (download on first use).
func (m *Manager) RuntimeInstall(ctx context.Context) (deno.Status, error) {
	if _, err := m.denoManager().Ensure(ctx); err != nil {
		return deno.Status{}, err
	}
	return m.denoManager().Status(), nil
}

// RuntimeUpdate forces a re-download of the pinned managed Deno.
func (m *Manager) RuntimeUpdate(ctx context.Context) (deno.Status, error) {
	if _, err := m.denoManager().Update(ctx); err != nil {
		return deno.Status{}, err
	}
	return m.denoManager().Status(), nil
}
