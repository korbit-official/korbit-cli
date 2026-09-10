// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package deno

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeZip builds a Deno release zip containing one executable entry with the
// given body, plus its sha256 hex.
func fakeZip(t *testing.T, body string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	name := "deno"
	if osWindows() {
		name = "deno.exe"
	}
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func osWindows() bool { return binName() == "deno.exe" }

// doerFunc adapts a function to Doer.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// serveBytes returns a Doer that serves b with HTTP 200 and counts calls.
func serveBytes(b []byte, calls *int) Doer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader(b)),
			Header:     http.Header{},
		}, nil
	})
}

func TestEnsureDownloadsVerifiesUnzipsInstalls(t *testing.T) {
	cache := t.TempDir()
	zipBytes, sha := fakeZip(t, "#!/bin/sh\necho fake-deno\n")
	calls := 0
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: sha}, func() int64 { return 1700000000000 }, nil, nil)

	path, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed deno: %v", err)
	}
	if !strings.Contains(string(got), "fake-deno") {
		t.Fatalf("installed body wrong: %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed deno is not executable: %v", info.Mode())
	}
	if calls != 1 {
		t.Errorf("expected 1 download, got %d", calls)
	}

	// A second Ensure with a matching cache must NOT re-download.
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if calls != 1 {
		t.Errorf("up-to-date cache should skip download, got %d calls", calls)
	}
}

// fakeZipWithMalicious builds a Deno release zip that, alongside the legit
// executable entry, carries a traversal/absolute-path entry — to pin that
// extraction matches by base name and writes only the managed binary (zip-slip
// is impossible by construction).
func fakeZipWithMalicious(t *testing.T, body string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	name := "deno"
	if osWindows() {
		name = "deno.exe"
	}
	for _, evil := range []string{"../../evil", "/tmp/evil-abs"} {
		w, err := zw.Create(evil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "malicious"); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestExtractIgnoresTraversalEntries(t *testing.T) {
	cache := t.TempDir()
	zipBytes, sha := fakeZipWithMalicious(t, "#!/bin/sh\necho fake-deno\n")
	calls := 0
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: sha}, func() int64 { return 1700000000000 }, nil, nil)

	path, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// (a) The legit binary still extracts.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed deno: %v", err)
	}
	if !strings.Contains(string(got), "fake-deno") {
		t.Fatalf("installed body wrong: %q", got)
	}
	// (b) Nothing escaped the managed dir: no traversal sibling and no absolute
	// path written. Only the managed deno dir's own files exist under the cache.
	if _, err := os.Stat(filepath.Join(cache, "evil")); !os.IsNotExist(err) {
		t.Errorf("traversal entry escaped to %s", filepath.Join(cache, "evil"))
	}
	if _, err := os.Stat("/tmp/evil-abs"); err == nil {
		t.Errorf("absolute-path entry was written outside the managed dir")
	}
}

func TestEnsureRefusesChecksumMismatch(t *testing.T) {
	cache := t.TempDir()
	zipBytes, _ := fakeZip(t, "tampered")
	calls := 0
	// Pin a wrong checksum: install must refuse and write nothing.
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: strings.Repeat("00", 32)}, nil, nil, nil)
	if _, err := m.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum-mismatch refusal, got %v", err)
	}
	if _, err := os.Stat(m.Path()); !os.IsNotExist(err) {
		t.Errorf("no binary should be installed on mismatch")
	}
}

func TestAutoUpdateOnVersionMismatch(t *testing.T) {
	cache := t.TempDir()
	zipBytes, sha := fakeZip(t, "v2-body")
	calls := 0
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{Version: "v9.9.9", SHA256: sha}, nil, nil, nil)
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 download, got %d", calls)
	}
	// Simulate the pin moving: a Manager wanting a different version has its own
	// (empty) version+target slot, so it is not up to date and must download.
	zip2, sha2 := fakeZip(t, "v10-body")
	calls2 := 0
	m2 := NewManager(cache, serveBytes(zip2, &calls2), Config{Version: "v10.0.0", SHA256: sha2}, nil, nil, nil)
	st := m2.Status()
	if st.UpToDate {
		t.Error("a version mismatch must report not-up-to-date")
	}
	if _, err := m2.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after pin bump: %v", err)
	}
	if calls2 != 1 {
		t.Errorf("pin mismatch must trigger re-download, got %d", calls2)
	}
	body, _ := os.ReadFile(m2.Path())
	if !strings.Contains(string(body), "v10-body") {
		t.Errorf("re-download did not replace the binary: %q", body)
	}
}

func TestUpdateForcesRedownload(t *testing.T) {
	cache := t.TempDir()
	zipBytes, sha := fakeZip(t, "body")
	calls := 0
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: sha}, nil, nil, nil)
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 2 {
		t.Errorf("Update must force a re-download even when current, got %d calls", calls)
	}
}

func TestRunArgs(t *testing.T) {
	m := NewManager(t.TempDir(), nil, Config{}, nil, nil, nil)
	perms := []string{"--no-prompt", "--allow-net=127.0.0.1"}
	bin, argv := m.RunArgs("/path/bundle.mjs", perms, "run", "--db", "x.db")
	if bin != m.Path() {
		t.Errorf("RunArgs bin = %q, want %q", bin, m.Path())
	}
	// `run`, then the caller-supplied permission flags, then the bundle + its args.
	want := []string{"run", "--no-prompt", "--allow-net=127.0.0.1", "/path/bundle.mjs", "run", "--db", "x.db"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("RunArgs argv = %v, want %v", argv, want)
	}

	// nil perms ⇒ fully sandboxed: no permission flags between `run` and the bundle.
	_, argv = m.RunArgs("/path/bundle.mjs", nil)
	if strings.Join(argv, " ") != "run /path/bundle.mjs" {
		t.Errorf("RunArgs with nil perms = %v, want [run /path/bundle.mjs]", argv)
	}
}

// TestRunEnvAndReload covers the run-time module cache plumbing: RunEnv pins
// DENO_DIR inside the shared cache (so module fetching stays self-contained), and
// ReloadArgs builds the `deno cache --reload <src>` refresh `sandbox update` uses.
func TestRunEnvAndReload(t *testing.T) {
	cache := t.TempDir()
	m := NewManager(cache, nil, Config{}, nil, nil, nil)

	env := m.RunEnv()
	wantEnv := "DENO_DIR=" + filepath.Join(cache, "deno-modules")
	if len(env) != 1 || env[0] != wantEnv {
		t.Errorf("RunEnv = %v, want [%q]", env, wantEnv)
	}
	const src = "https://docs.digitalx.miraeasset.com/digitalx-sandbox.mjs"
	if m.HasModuleCache(src) {
		t.Error("HasModuleCache should be false before anything is fetched")
	}

	bin, argv := m.ReloadArgs("https://example.com/digitalx-sandbox.mjs")
	if bin != m.Path() {
		t.Errorf("ReloadArgs bin = %q, want %q", bin, m.Path())
	}
	want := []string{"cache", "--reload", "https://example.com/digitalx-sandbox.mjs"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("ReloadArgs argv = %v, want %v", argv, want)
	}
}

// TestHasModuleCacheIsPerSource pins that the cached-bundle check is keyed by
// the source URL the way Deno's own module cache is
// (<DENO_DIR>/remote/<scheme>/<host>[_PORT<port>]): a bundle fetched from one
// URL must not make a different URL look cached, and a non-http(s) source is
// never in that cache.
func TestHasModuleCacheIsPerSource(t *testing.T) {
	cache := t.TempDir()
	m := NewManager(cache, nil, Config{}, nil, nil, nil)
	remote := filepath.Join(m.ModuleCacheDir(), "remote")

	// A bundle already fetched from the legacy host.
	legacyHost := filepath.Join(remote, "https", "docs.korbit.co.kr")
	if err := os.MkdirAll(legacyHost, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyHost, "deadbeef"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !m.HasModuleCache("https://docs.korbit.co.kr/korbit-sandbox.mjs") {
		t.Error("the fetched source should read as cached")
	}
	if m.HasModuleCache("https://docs.digitalx.miraeasset.com/digitalx-sandbox.mjs") {
		t.Error("a different host must NOT read as cached — Deno keys its cache by URL")
	}
	if m.HasModuleCache("http://docs.korbit.co.kr/korbit-sandbox.mjs") {
		t.Error("a different scheme must NOT read as cached")
	}

	// An explicit non-default port is a separate host directory ("_PORT<port>").
	ported := filepath.Join(remote, "http", "127.0.0.1_PORT8771")
	if err := os.MkdirAll(ported, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ported, "cafe"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.HasModuleCache("http://127.0.0.1:8771/digitalx-sandbox.mjs") {
		t.Error("a ported source should read as cached from its _PORT directory")
	}
	if m.HasModuleCache("http://127.0.0.1:9999/digitalx-sandbox.mjs") {
		t.Error("a different port must NOT read as cached")
	}

	// Sources that are never in Deno's remote cache.
	for _, src := range []string{"", "/abs/path/digitalx-sandbox.mjs", "file:///tmp/digitalx-sandbox.mjs", "://nonsense"} {
		if m.HasModuleCache(src) {
			t.Errorf("HasModuleCache(%q) must be false — not a remote http(s) source", src)
		}
	}

	// An empty host directory is not a cache hit either.
	if err := os.MkdirAll(filepath.Join(remote, "https", "empty.example.test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if m.HasModuleCache("https://empty.example.test/digitalx-sandbox.mjs") {
		t.Error("an empty host directory must not read as cached")
	}
}

// TestPinnedChecksumsCoverEveryTarget asserts the generated checksums map has an
// entry for every platform korbit-cli maps a Deno target for, so a version bump
// (scripts/update-deno.sh) can't silently drop a platform and leave it
// unverifiable at runtime.
func TestPinnedChecksumsCoverEveryTarget(t *testing.T) {
	for plat := range targets {
		sha, ok := checksums[plat]
		if !ok {
			t.Errorf("no pinned checksum for target platform %q", plat)
			continue
		}
		if len(sha) != 64 {
			t.Errorf("checksum for %q is not a 32-byte sha256 hex: %q", plat, sha)
		}
		if _, err := hex.DecodeString(sha); err != nil {
			t.Errorf("checksum for %q is not valid hex: %v", plat, err)
		}
	}
	// And no stray checksum without a target mapping.
	for plat := range checksums {
		if _, ok := targets[plat]; !ok {
			t.Errorf("checksum for unknown platform %q (not in targets map)", plat)
		}
	}
}

func TestTargetCurrentPlatform(t *testing.T) {
	if _, err := Target(); err != nil {
		// Only musl Linux legitimately has no managed build; the CI/dev hosts here
		// (darwin, glibc linux, windows) must resolve.
		t.Skipf("no managed Deno target for this platform: %v", err)
	}
}

func TestVersionParsing(t *testing.T) {
	parse := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v2.8.3", [3]int{2, 8, 3}, true},
		{"2.8.3", [3]int{2, 8, 3}, true},
		{"v10.0.0", [3]int{10, 0, 0}, true},
		{"v2.9.0-rc.1", [3]int{}, false}, // prerelease/canary — incomparable
		{"v2.8", [3]int{}, false},        // too few parts
		{"v2.8.3.1", [3]int{}, false},    // too many parts
		{"v2.x.3", [3]int{}, false},      // non-numeric
		{"v-1.0.0", [3]int{}, false},     // negative
		{"", [3]int{}, false},
	}
	for _, c := range parse {
		got, ok := parseVersion(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseVersion(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}

	if !older([3]int{2, 8, 3}, [3]int{2, 9, 0}) {
		t.Error("2.8.3 should be older than 2.9.0")
	}
	if older([3]int{2, 9, 0}, [3]int{2, 9, 0}) {
		t.Error("equal versions are not older")
	}
	if older([3]int{3, 0, 0}, [3]int{2, 9, 0}) {
		t.Error("3.0.0 is not older than 2.9.0")
	}

	// parseSlotName must split on the FIRST underscore so a target that itself
	// contains underscores (x86_64-…) round-trips.
	slot := []struct {
		in     string
		v      [3]int
		target string
		ok     bool
	}{
		{"v2.8.3_aarch64-apple-darwin", [3]int{2, 8, 3}, "aarch64-apple-darwin", true},
		{"v2.8.3_x86_64-unknown-linux-gnu", [3]int{2, 8, 3}, "x86_64-unknown-linux-gnu", true},
		{"noUnderscore", [3]int{}, "", false},
		{"bad_aarch64-apple-darwin", [3]int{}, "", false}, // unparseable version
		{"v2.8.3_", [3]int{}, "", false},                  // empty target
	}
	for _, c := range slot {
		v, target, ok := parseSlotName(c.in)
		if ok != c.ok || v != c.v || target != c.target {
			t.Errorf("parseSlotName(%q) = %v,%q,%v; want %v,%q,%v", c.in, v, target, ok, c.v, c.target, c.ok)
		}
	}
}

// TestPruneRemovesOnlyOlderSameTarget pins that a successful install deletes only
// strictly-older slots for the SAME target, keeping newer/equal slots, other
// targets, and unparseable dir names.
func TestPruneRemovesOnlyOlderSameTarget(t *testing.T) {
	target, err := Target()
	if err != nil {
		t.Skipf("no managed Deno target: %v", err)
	}
	cache := t.TempDir()
	root := filepath.Join(cache, "deno")
	seed := func(name string) string {
		d := filepath.Join(root, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	olderSlot := seed("v1.0.0_" + target)
	newerSlot := seed("v3.0.0_" + target)
	otherTarget := seed("v1.0.0_some-other-target")
	unparseable := seed("scratch_" + target)

	zipBytes, sha := fakeZip(t, "body")
	m := NewManager(cache, serveBytes(zipBytes, new(int)), Config{Version: "v2.0.0", SHA256: sha}, nil, nil, nil)
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if _, err := os.Stat(olderSlot); !os.IsNotExist(err) {
		t.Errorf("strictly-older same-target slot should be pruned")
	}
	for _, d := range []string{newerSlot, otherTarget, unparseable} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("slot %s should be kept: %v", d, err)
		}
	}
	if _, err := os.Stat(m.Path()); err != nil {
		t.Errorf("current binary missing after install: %v", err)
	}
}

// TestSteadyStateNoThrash pins the core property: after one collision, two CLI
// versions sharing a cache settle — the newer is never re-downloaded, and the
// older re-downloads exactly once (after the newer pruned it), not on every run.
func TestSteadyStateNoThrash(t *testing.T) {
	if _, err := Target(); err != nil {
		t.Skipf("no managed Deno target: %v", err)
	}
	cache := t.TempDir()
	zOld, sOld := fakeZip(t, "old")
	zNew, sNew := fakeZip(t, "new")
	oldCalls, newCalls := 0, 0
	mOld := NewManager(cache, serveBytes(zOld, &oldCalls), Config{Version: "v2.8.3", SHA256: sOld}, nil, nil, nil)
	mNew := NewManager(cache, serveBytes(zNew, &newCalls), Config{Version: "v2.9.0", SHA256: sNew}, nil, nil, nil)

	ensure := func(m *Manager) {
		if _, err := m.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	ensure(mOld)       // oldCalls=1
	ensure(mNew)       // newCalls=1, prunes v2.8.3
	ensure(mOld)       // oldCalls=2 (its slot was pruned → one re-download)
	ensure(mNew)       // up to date, no download
	ensure(mOld)       // up to date, no download
	if oldCalls != 2 { // exactly one re-download, then stable
		t.Errorf("old version: got %d downloads, want 2 (one collision, then stable)", oldCalls)
	}
	if newCalls != 1 {
		t.Errorf("newer version must never be re-downloaded, got %d", newCalls)
	}
	// Both binaries coexist.
	if _, err := os.Stat(mOld.Path()); err != nil {
		t.Errorf("old binary missing: %v", err)
	}
	if _, err := os.Stat(mNew.Path()); err != nil {
		t.Errorf("new binary missing: %v", err)
	}
}

// TestSelfHealOnDrift pins that a corrupted cache never gets stuck: a truncated
// binary or a corrupt meta reads as not-up-to-date and is re-downloaded.
func TestSelfHealOnDrift(t *testing.T) {
	target, err := Target()
	if err != nil {
		t.Skipf("no managed Deno target: %v", err)
	}
	t.Run("truncated binary", func(t *testing.T) {
		cache := t.TempDir()
		z, s := fakeZip(t, "full-length-body")
		calls := 0
		m := NewManager(cache, serveBytes(z, &calls), Config{SHA256: s}, nil, nil, nil)
		if _, err := m.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.Path(), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if m.Status().UpToDate {
			t.Error("a truncated binary must report not-up-to-date")
		}
		if _, err := m.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Errorf("size drift must trigger re-download, got %d calls", calls)
		}
	})
	t.Run("corrupt meta", func(t *testing.T) {
		cache := t.TempDir()
		z, s := fakeZip(t, "body")
		calls := 0
		m := NewManager(cache, serveBytes(z, &calls), Config{SHA256: s}, nil, nil, nil)
		if _, err := m.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.metaPath(target), []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if m.Status().UpToDate {
			t.Error("a corrupt meta must report not-up-to-date")
		}
		if _, err := m.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Errorf("corrupt meta must trigger re-download, got %d calls", calls)
		}
	})
}
