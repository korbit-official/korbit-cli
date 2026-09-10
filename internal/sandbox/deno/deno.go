// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package deno manages a pinned, checksum-verified Deno runtime that digitalx-cli
// downloads on demand to run the local API sandbox. The managed Deno is the
// default runtime (it runs the bundle under a least-privilege permission sandbox
// and fetches the bundle straight from its source URL); it ships as a single
// static executable, so a host with no `deno` of its own still gets a working
// sandbox. A system `deno` on PATH can be used instead (--runtime deno).
//
// The pinned Version and per-target checksums live in the generated pinned.go
// (maintained by scripts/update-deno.sh). The managed runtime is matched to that
// compile-time pin, NOT to "latest upstream": whenever it is used, the cached
// copy's recorded version+sha256 are compared to the pin and any drift (a
// missing binary, a missing/corrupt/mismatched meta, a wrong size) triggers an
// automatic re-download of the pinned version. There is no network "is there a
// newer Deno?" check — the pin moves only when digitalx-cli itself is upgraded.
//
// Each version+target is cached in its own directory
// (<cache>/deno/<version>_<target>/deno), so several digitalx-cli versions sharing
// one cache coexist without clobbering each other's binary. After a successful
// install the manager prunes only STRICTLY-OLDER versions for the same target,
// keeping the current and any newer ones — so alternating CLI versions settle
// into a stable steady state instead of re-downloading on every switch. All
// writes are atomic (temp + rename) and every read self-heals by re-downloading
// on drift, so the cache can never get stuck in a broken state.
package deno

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/logging"
)

// Doer performs an HTTP request; *http.Client satisfies it. Injected so tests
// serve a fake zip without touching the network.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// targets maps a Go "<GOOS>/<GOARCH>" to the Deno release target triple used in
// the asset name (deno-<target>.zip). Linux is glibc-only — Deno publishes no
// musl build — so Alpine has no managed-Deno path and needs a system `deno`.
var targets = map[string]string{
	"darwin/arm64":  "aarch64-apple-darwin",
	"darwin/amd64":  "x86_64-apple-darwin",
	"linux/arm64":   "aarch64-unknown-linux-gnu",
	"linux/amd64":   "x86_64-unknown-linux-gnu",
	"windows/arm64": "aarch64-pc-windows-msvc",
	"windows/amd64": "x86_64-pc-windows-msvc",
}

// platformKey is the current host's "<GOOS>/<GOARCH>".
func platformKey() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Target returns the Deno release target triple for the current platform, or an
// error if digitalx-cli has no managed-Deno build for it (e.g. Linux musl).
func Target() (string, error) {
	t, ok := targets[platformKey()]
	if !ok {
		return "", fmt.Errorf("no managed Deno build for %s (on Alpine/musl Linux, install Deno yourself and use --runtime deno)", platformKey())
	}
	return t, nil
}

// parseVersion parses a Deno release tag ("v2.8.3" or "2.8.3") into its numeric
// components. ok is false for anything that isn't a plain X.Y.Z release — a
// prerelease/canary override (e.g. "v2.9.0-rc.1") or a non-version dir name — so
// such versions are treated as incomparable: never pruned, and never prune.
func parseVersion(s string) (v [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// older reports whether version a is strictly older than b.
func older(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// parseSlotName splits a cache sub-directory name "<version>_<target>" back into
// its parts at the FIRST underscore: the version has none, so the first "_" is
// the boundary and the target (which may contain underscores) is the remainder.
// ok is false if the name has no underscore, an unparseable version, or an empty
// target.
func parseSlotName(name string) (v [3]int, target string, ok bool) {
	i := strings.IndexByte(name, '_')
	if i < 0 {
		return [3]int{}, "", false
	}
	v, ok = parseVersion(name[:i])
	target = name[i+1:]
	if !ok || target == "" {
		return [3]int{}, "", false
	}
	return v, target, true
}

// meta records what is installed in a version+target's cache dir, so a re-run
// can skip the download when the cached copy already matches the pin. Size is
// the installed binary's byte length — a cheap integrity check (a truncated or
// partially-deleted binary no longer matches and is re-downloaded) without
// re-hashing ~38 MB on every run.
type meta struct {
	Version     string `json:"version"`
	Target      string `json:"target"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	InstalledAt int64  `json:"installedAt"`
}

// Config selects the pinned version, asset URL base, and expected checksum.
// Defaults come from the generated pin; overrides exist for tests (and the
// DIGITALX_CLI_DENO_VERSION / DIGITALX_CLI_DENO_URL env, wired by the caller).
type Config struct {
	// Version is the Deno release tag (e.g. "v2.8.3"). Defaults to the pinned Version.
	Version string
	// BaseURL is the release download base; defaults to the GitHub releases host.
	// The asset deno-<target>.zip is appended per the resolved version+target.
	BaseURL string
	// SHA256 is the expected zip checksum (hex). When empty it is taken from the
	// pinned checksums map for the current target; a non-empty value (a version
	// override with no pin) is used as-is.
	SHA256 string
}

const defaultBaseURL = "https://github.com/denoland/deno/releases/download"

// Manager installs and resolves the managed Deno under a cache directory.
type Manager struct {
	cacheDir string
	doer     Doer
	cfg      Config
	now      func() int64
	// note, when set, receives a one-line USER-FACING progress note before a
	// download (so the user sees the ~38 MB fetch). This stays a func(string) seam
	// because it is program output (the CLI routes it to a Note), distinct from the
	// level-gated operational diagnostics on Log. Nil is silent.
	note func(string)

	// Log receives operational diagnostics: the asset URL, the sha256 verify
	// pass/MISMATCH, the install path, and the pin-mismatch auto-update. nil =
	// silent (logging.Or). It is set by NewManager (so a fresh manager built per
	// call inherits it).
	Log *slog.Logger
}

// NewManager builds a Manager. cacheDir is the shared artifact cache root; the
// Deno lives under cacheDir/deno/. doer is the HTTP client seam. cfg's zero
// value resolves to the pinned version+checksum+default host. note is the
// user-facing download-progress seam (program output, func(string)); log is the
// optional operational logger for structured diagnostics (asset URL, sha256
// verify, install — nil is silent).
func NewManager(cacheDir string, doer Doer, cfg Config, now func() int64, note func(string), log *slog.Logger) *Manager {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return &Manager{cacheDir: cacheDir, doer: doer, cfg: cfg, now: now, note: note, Log: log}
}

// log returns the manager's operational logger, never nil (logging.Or).
func (m *Manager) log() *slog.Logger { return logging.Or(m.Log) }

// version returns the configured version, defaulting to the pin.
func (m *Manager) version() string {
	if m.cfg.Version != "" {
		return m.cfg.Version
	}
	return Version
}

// expectedSHA returns the checksum to verify the download against: the explicit
// config value, else the pinned checksum for the current target. An empty result
// with no error means a version override was given without a checksum AND the pin
// has no entry — the caller refuses rather than trusting an unverifiable download.
func (m *Manager) expectedSHA(target string) (string, error) {
	if m.cfg.SHA256 != "" {
		return strings.ToLower(m.cfg.SHA256), nil
	}
	// Only trust the embedded pin when the version matches the pin (a version
	// override pairs with its own checksum; the pinned hashes are for Version).
	if m.version() == Version {
		if sha, ok := checksums[platformKey()]; ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("no pinned checksum for Deno %s on %s — set the checksum (DIGITALX_CLI_DENO_URL pins must carry one)", m.version(), platformKey())
}

// versionsRoot holds one sub-directory per cached version+target. It is the dir
// pruneOldVersions scans.
func (m *Manager) versionsRoot() string { return filepath.Join(m.cacheDir, "deno") }

// slotName is the cache sub-directory for a version+target, e.g.
// "v2.8.3_aarch64-apple-darwin". The version (vX.Y.Z) has no underscore, so it is
// always the prefix up to the FIRST "_"; the target is everything after and may
// itself contain underscores (e.g. x86_64-unknown-linux-gnu). parseSlotName
// splits on that first underscore.
func slotName(version, target string) string { return version + "_" + target }

// slotDir is this manager's version's cache dir for the given target.
func (m *Manager) slotDir(target string) string {
	return filepath.Join(m.versionsRoot(), slotName(m.version(), target))
}

func (m *Manager) binPath(target string) string { return filepath.Join(m.slotDir(target), binName()) }
func (m *Manager) metaPath(target string) string {
	return filepath.Join(m.slotDir(target), "meta.json")
}

// ModuleCacheDir is where Deno caches the modules it fetches at run time (the
// sandbox bundle and any transitive deps), kept under the shared cache via
// DENO_DIR so it is co-located with the managed binary, removed with the cache,
// and never pollutes the user's own global Deno cache (~/.cache/deno).
func (m *Manager) ModuleCacheDir() string { return filepath.Join(m.cacheDir, "deno-modules") }

// RunEnv is the extra environment to set when invoking the managed Deno: it pins
// DENO_DIR to ModuleCacheDir so module fetching/caching stays inside our cache.
func (m *Manager) RunEnv() []string { return []string{"DENO_DIR=" + m.ModuleCacheDir()} }

// HasModuleCache reports whether Deno has fetched src into the run-time module
// cache yet (the "is this bundle cached" check for `sandbox status` and the
// first-download notice — the deno runtime delegates bundle caching to Deno, so
// there is no bundle file of ours to stat). It is per-source: a different URL,
// even one serving the same bundle, is a separate cache entry and reads as
// not-yet-fetched. A non-http(s) src is never in this cache.
func (m *Manager) HasModuleCache(src string) bool {
	dir := m.remoteCacheDir(src)
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// remoteCacheDir is the directory Deno caches a remote module's host under:
// <DENO_DIR>/remote/<scheme>/<host>, with an explicit non-default port appended
// as "_PORT<port>". Empty for anything but an http(s) URL with a host.
func (m *Manager) remoteCacheDir(src string) string {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := u.Hostname()
	if port := u.Port(); port != "" {
		host += "_PORT" + port
	}
	return filepath.Join(m.ModuleCacheDir(), "remote", u.Scheme, host)
}

// binName is the executable name within the cache (deno or deno.exe).
func binName() string {
	if runtime.GOOS == "windows" {
		return "deno.exe"
	}
	return "deno"
}

// Path is the managed Deno executable path for the current platform (whether or
// not it exists yet). On a platform with no managed build (musl Linux) it falls
// back to a clearly-unsupported slot name; that path is never installed or
// executed because Ensure fails at Target() first.
func (m *Manager) Path() string {
	target, err := Target()
	if err != nil {
		target = "unsupported-" + runtime.GOOS + "-" + runtime.GOARCH
	}
	return m.binPath(target)
}

// assetURL is the download URL for the current version+target.
func (m *Manager) assetURL(target string) string {
	base := m.cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return fmt.Sprintf("%s/%s/deno-%s.zip", strings.TrimRight(base, "/"), m.version(), target)
}

// cachedMeta reads the install meta for a target, or zero meta if absent/corrupt
// (a corrupt meta reads as zero, so upToDate fails and the binary is re-downloaded).
func (m *Manager) cachedMeta(target string) meta {
	var mt meta
	raw, err := os.ReadFile(m.metaPath(target))
	if err != nil {
		return meta{}
	}
	if json.Unmarshal(raw, &mt) != nil {
		return meta{}
	}
	return mt
}

// upToDate reports whether the cached Deno already matches the pin: the binary
// exists, its size matches, and meta records the expected version+target+sha256.
// This is the auto-update + self-heal gate — any drift means re-download.
func (m *Manager) upToDate(target, wantSHA string) bool {
	fi, err := os.Stat(m.binPath(target))
	if err != nil {
		return false
	}
	mt := m.cachedMeta(target)
	return mt.Version == m.version() &&
		mt.Target == target &&
		strings.EqualFold(mt.SHA256, wantSHA) &&
		mt.Size == fi.Size()
}

// Ensure resolves the managed Deno path, downloading (or re-downloading on a
// pin mismatch) the pinned version when needed, and returns the executable path.
// It is idempotent: an already-current cache is returned without a network call.
func (m *Manager) Ensure(ctx context.Context) (string, error) {
	target, err := Target()
	if err != nil {
		return "", err
	}
	wantSHA, err := m.expectedSHA(target)
	if err != nil {
		return "", err
	}
	if m.upToDate(target, wantSHA) {
		m.log().Debug("managed Deno up to date", "version", m.version(), "target", target, "path", m.binPath(target))
		return m.binPath(target), nil
	}
	m.log().Info("managed Deno pin mismatch — installing", "version", m.version(), "target", target)
	if err := m.install(ctx, target, wantSHA); err != nil {
		return "", err
	}
	return m.binPath(target), nil
}

// Update forces a re-download of the pinned version regardless of the cache
// state (pre-warming CI, or recovering a corrupt cache).
func (m *Manager) Update(ctx context.Context) (string, error) {
	target, err := Target()
	if err != nil {
		return "", err
	}
	wantSHA, err := m.expectedSHA(target)
	if err != nil {
		return "", err
	}
	if err := m.install(ctx, target, wantSHA); err != nil {
		return "", err
	}
	return m.binPath(target), nil
}

// Status reports the cached version/sha and whether it matches the pin, without
// any network call. installed is false when nothing is cached.
type Status struct {
	Installed     bool   `json:"installed"`
	Path          string `json:"path"`
	CachedVersion string `json:"cachedVersion,omitempty"`
	PinnedVersion string `json:"pinnedVersion"`
	UpToDate      bool   `json:"upToDate"`
	Target        string `json:"target,omitempty"`
}

// Status returns the managed-Deno cache status against the pin.
func (m *Manager) Status() Status {
	st := Status{Path: m.Path(), PinnedVersion: m.version()}
	target, terr := Target()
	if terr != nil {
		return st // no managed build for this platform
	}
	st.Target = target
	st.Path = m.binPath(target)
	if _, err := os.Stat(m.binPath(target)); err != nil {
		return st
	}
	mt := m.cachedMeta(target)
	st.Installed = true
	st.CachedVersion = mt.Version
	if wantSHA, err := m.expectedSHA(target); err == nil {
		st.UpToDate = m.upToDate(target, wantSHA)
	}
	return st
}

// install downloads the zip via the injected Doer, verifies its sha256 against
// wantSHA (refusing on mismatch — the download is never trusted), unzips the
// single deno executable, installs it atomically (0755), best-effort strips the
// macOS quarantine xattr, records meta.json (atomically), then prunes older
// versions for this target.
func (m *Manager) install(ctx context.Context, target, wantSHA string) error {
	if m.doer == nil {
		return fmt.Errorf("no HTTP client configured to download Deno")
	}
	slot := m.slotDir(target)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		return err
	}
	url := m.assetURL(target)
	if m.note != nil {
		m.note(fmt.Sprintf("downloading Deno %s (~38 MB) to %s …", m.version(), slot))
	}
	m.log().Info("downloading managed Deno", "version", m.version(), "target", target, "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.doer.Do(req)
	if err != nil {
		return fmt.Errorf("downloading Deno %s: %w", m.version(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("downloading Deno %s: %s returned HTTP %d", m.version(), url, resp.StatusCode)
	}

	// Read the whole zip and verify the checksum BEFORE extracting anything.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading Deno download: %w", err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, wantSHA) {
		// High-value diagnostic: the download is refused rather than installed.
		m.log().Warn("managed Deno sha256 verify MISMATCH — refusing to install", "bytes", len(body), "got", got, "want", wantSHA)
		return fmt.Errorf("Deno %s checksum mismatch: got %s, want %s — refusing to install", m.version(), got, wantSHA)
	}
	m.log().Debug("managed Deno sha256 verify ok", "bytes", len(body), "sha256", got)

	exe, err := extractDenoExe(body)
	if err != nil {
		return err
	}
	m.log().Debug("managed Deno unzipped", "exeBytes", len(exe))

	// Atomic install: write the binary, then meta, each via temp + rename so a
	// reader (or a concurrent install) never sees a half-written file.
	if err := writeFileAtomic(m.binPath(target), exe, 0o755); err != nil {
		return err
	}
	// Best-effort: strip the macOS quarantine xattr so the self-written binary
	// runs without a Gatekeeper prompt. Almost certainly absent on a file we just
	// wrote, but harmless and matches the manual `xattr -d` step.
	stripQuarantine(m.binPath(target))

	raw, err := json.MarshalIndent(meta{
		Version:     m.version(),
		Target:      target,
		SHA256:      strings.ToLower(wantSHA),
		Size:        int64(len(exe)),
		InstalledAt: m.now(),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(m.metaPath(target), append(raw, '\n'), 0o644); err != nil {
		return err
	}
	m.log().Info("managed Deno installed", "version", m.version(), "target", target, "path", m.binPath(target))
	m.pruneOldVersions(target)
	return nil
}

// writeFileAtomic writes data to a temp file in the destination's directory,
// chmods it, then renames it over path. A reader never sees a half-written file,
// and a crash mid-write leaves either the old file or none (re-downloaded next
// run) — never a torn one. Concurrent writers each use a distinct temp name, so
// the final rename is last-writer-wins of two complete files.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// pruneOldVersions removes cached versions for this target that are STRICTLY
// older than the one just installed, keeping the current and any newer ones.
// Scoped to this target so a cache shared across architectures never deletes
// another arch's binary. Best-effort: a failure to read the root or remove a dir
// is logged and ignored — pruning never blocks a successful install. Versions
// that don't parse (a prerelease/canary override, a foreign dir name) are left
// untouched, and if our OWN version doesn't parse we prune nothing.
//
// Prune only ever removes a STRICTLY-older version, so concurrency is bounded and
// self-healing: if another process is installing an older version into its slot
// while we prune it, that process's next run just re-downloads (a missing binary
// fails upToDate) — nothing wedges. Current/newer slots are never touched, so the
// steady state stays stable.
func (m *Manager) pruneOldVersions(target string) {
	cur, ok := parseVersion(m.version())
	if !ok {
		return
	}
	root := m.versionsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		m.log().Debug("could not scan managed Deno cache to prune", "root", root, "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, t, ok := parseSlotName(e.Name())
		if !ok || t != target || !older(v, cur) {
			continue
		}
		p := filepath.Join(root, e.Name())
		if err := os.RemoveAll(p); err != nil {
			m.log().Debug("could not prune old managed Deno", "path", p, "err", err)
			continue
		}
		m.log().Debug("pruned old managed Deno", "path", p, "keptVersion", m.version())
	}
}

// extractDenoExe finds and returns the deno/deno.exe entry from the release zip.
func extractDenoExe(zipBytes []byte) ([]byte, error) {
	zr, err := zip.NewReader(newByteReaderAt(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("opening Deno zip: %w", err)
	}
	want := binName()
	for _, f := range zr.File {
		// The release zip carries a single executable named deno / deno.exe.
		base := filepath.Base(f.Name)
		if base == want || base == "deno" || base == "deno.exe" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("Deno zip did not contain a %s executable", want)
}

// byteReaderAt adapts a byte slice to io.ReaderAt for archive/zip.
type byteReaderAt struct{ b []byte }

func newByteReaderAt(b []byte) *byteReaderAt { return &byteReaderAt{b: b} }

func (r *byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// RunArgs returns the argv to invoke the sandbox bundle under the managed Deno:
// deno run <perms...> <bundle> <args...>. perms are the explicit Deno permission
// flags (e.g. "--allow-net=...") the caller has scoped to exactly what the bundle
// needs — Deno denies everything not granted, so this is least-privilege by
// default. A nil/empty perms runs the bundle fully sandboxed (no permissions).
// This package stays policy-free: the caller (internal/sandbox) owns which paths
// and capabilities the bundle is allowed, since only it knows them.
func (m *Manager) RunArgs(bundlePath string, perms []string, args ...string) (string, []string) {
	argv := append([]string{"run"}, perms...)
	argv = append(argv, bundlePath)
	argv = append(argv, args...)
	return m.Path(), argv
}

// ReloadArgs returns the argv to force-refresh Deno's run-time cache of src
// (`deno cache --reload <src>`) — how `sandbox update` re-fetches the bundle for
// the managed-Deno runtime, which delegates bundle caching to Deno. `deno cache`
// only downloads (it runs no code), so it needs no permission flags. Pair with
// RunEnv so the refresh lands in the same DENO_DIR the run uses.
func (m *Manager) ReloadArgs(src string) (string, []string) {
	return m.Path(), []string{"cache", "--reload", src}
}
