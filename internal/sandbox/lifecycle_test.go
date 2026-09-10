// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/sandbox/deno"
)

func TestResolveRuntimePrecedence(t *testing.T) {
	withDeno := func(string) (string, error) { return "/usr/bin/deno", nil }
	noDeno := func(string) (string, error) { return "", fmt.Errorf("not found") }

	// Auto defaults to the managed Deno wherever a managed-Deno build exists for
	// the platform, regardless of a system deno; only on a platform with no
	// managed-Deno build (musl) does it fall back to a system deno. The test host
	// has a build, so assert the managed-Deno default; the no-build branch is
	// covered by the precedence logic itself.
	_, noTarget := deno.Target()
	if noTarget == nil {
		// A build exists → managed wins even when a system deno is present.
		if rt, err := resolveRuntime("", withDeno, nil); err != nil || rt.Kind != RuntimeManagedDeno {
			t.Errorf("auto = %v, %v; want managed-deno", rt.Kind, err)
		}
	} else {
		// No build → fall back to a system deno when present.
		if rt, err := resolveRuntime("", withDeno, nil); err != nil || rt.Kind != RuntimeSystemDeno {
			t.Errorf("auto (no build) = %v, %v; want deno", rt.Kind, err)
		}
	}
	// Auto without a system deno → managed-deno (downloaded where a build exists;
	// otherwise still returned so Ensure surfaces the no-build message).
	if rt, err := resolveRuntime("", noDeno, nil); err != nil || rt.Kind != RuntimeManagedDeno {
		t.Errorf("auto+no-deno = %v, %v; want managed-deno", rt.Kind, err)
	}
	// Explicit deno without a system deno present → error.
	if _, err := resolveRuntime("deno", noDeno, nil); err == nil {
		t.Error("explicit --runtime deno with no deno should error")
	}
	// Explicit deno with one present → system deno.
	if rt, err := resolveRuntime("deno", withDeno, nil); err != nil || rt.Kind != RuntimeSystemDeno {
		t.Errorf("explicit deno = %v, %v; want deno", rt.Kind, err)
	}
	// Explicit managed-deno regardless of a system deno.
	if rt, err := resolveRuntime("managed-deno", withDeno, nil); err != nil || rt.Kind != RuntimeManagedDeno {
		t.Errorf("explicit managed-deno = %v, %v", rt.Kind, err)
	}
	// Unknown preference.
	if _, err := resolveRuntime("bun", withDeno, nil); err == nil {
		t.Error("unknown runtime should error")
	}
}

// fakeDeno writes a shell script that emulates the managed/system Deno running
// the bundle's relevant subcommands when invoked as
// `deno run <perms…> <bundleRef> <subcommand> <flags>`. It returns the script
// path (used as the `deno` resolved via LookPath). seededPEM/apiKey (and any
// market pairs) are embedded into the `status --json` output. A non-`run`
// invocation (e.g. `cache --reload`) is a successful no-op.
func fakeDeno(t *testing.T, dbPidPath, seededPEM, apiKey string, pairs ...statusPair) string {
	t.Helper()
	return fakeDenoMarkets(t, dbPidPath, seededPEM, apiKey, statusMarkets{Pairs: pairs})
}

// fakeDenoMarkets is fakeDeno taking the full markets document — so a test can
// also set `initializedSource` (the paper-trading gating field).
func fakeDenoMarkets(t *testing.T, dbPidPath, seededPEM, apiKey string, markets statusMarkets) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-runtime lifecycle test is unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "deno")
	statusJSON, _ := json.Marshal(statusDoc{
		Users:   []statusUser{{Keys: []statusKey{{APIKey: apiKey, Type: "ed25519", Secret: seededPEM}}}},
		Markets: markets,
	})
	body := fmt.Sprintf(`#!/bin/sh
# $1 = deno subcommand (run|cache|…). Only `+"`run`"+` is emulated; others no-op.
mode="$1"; shift
if [ "$mode" != "run" ]; then exit 0; fi
# Skip the --allow-*/--no-prompt permission flags, then the bundle ref, to reach
# the bundle subcommand.
while [ "$#" -gt 0 ]; do
  case "$1" in
    --*) shift ;;
    *) break ;;
  esac
done
shift              # drop the bundle ref (URL or path)
sub="$1"; shift
db=""
port="9999"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --db) db="$2"; shift 2 ;;
    --port) port="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case "$sub" in
  init-db)
    : > "$db"
    ;;
  run)
    # Emulate --port 0 → pick a fixed test port (no real bind needed; the test's
    # Doer fakes /v2/time readiness).
    if [ "$port" = "0" ]; then port=45999; fi
    printf '{"pid":%%s,"port":%%s}' "$$" "$port" > "%s"
    echo "digitalx-sandbox listening on http://127.0.0.1:$port"
    # Stay alive until signalled.
    while true; do sleep 1; done
    ;;
  status)
    cat <<'JSON'
%s
JSON
    ;;
  license)
    # Echo the footer-command override under both env names so a test can assert
    # the env wiring; the real bundle renders the framed notice here.
    echo "LICENSE_CMD=$DIGITALX_SANDBOX_LICENSE_CMD"
    echo "LEGACY_LICENSE_CMD=$KORBIT_SANDBOX_LICENSE_CMD"
    ;;
esac
`, dbPidPath, string(statusJSON))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestStartStopWithFakeRuntime(t *testing.T) {
	home := t.TempDir()
	cacheDir := t.TempDir()

	// Seed a real ED25519 PEM as the sandbox's "secret".
	kp, err := apiclient.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	apiKey := keys.SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"

	// Build the key manager over the home (real file keystore).
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)

	dbPid := filepath.Join(home, "sandbox", DBDefaultName+"-pid")
	denoBin := fakeDeno(t, dbPid, kp.PrivatePEM, apiKey)
	denoDir := filepath.Dir(denoBin)

	// Doer fakes /v2/time as always-ready (the fake deno doesn't actually bind).
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})

	// Use a system `deno` (the fake on PATH) so no managed download is needed.
	cfg := Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}
	deps := Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
	}
	m := New(cfg, deps)

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("expected default port 9999, got %d", res.Port)
	}
	if res.RestBaseURL != "http://127.0.0.1:9999" {
		t.Errorf("rest url = %q", res.RestBaseURL)
	}
	if !res.Imported || res.APIKeyID != apiKey {
		t.Errorf("key import: imported=%v apiKey=%q", res.Imported, res.APIKeyID)
	}

	// The key landed as a non-default sandbox key, pinned to loopback.
	show, err := km.Show("sandbox")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !show.IsSandbox {
		t.Error("imported key should be a sandbox key")
	}
	if show.IsDefault {
		t.Error("sandbox key must NOT be the default")
	}
	if show.BaseURL != "http://127.0.0.1:9999" || show.WSBaseURL != "ws://127.0.0.1:9999" {
		t.Errorf("key not pinned to loopback: base=%q ws=%q", show.BaseURL, show.WSBaseURL)
	}
	if def, _ := km.DefaultKeyName(); def != "" {
		t.Errorf("no default should be set, got %q", def)
	}

	// Status reflects the running server and imported key.
	st := m.Status(context.Background())
	if !st.Server.Running || st.Server.Port != 9999 {
		t.Errorf("status server = %+v", st.Server)
	}
	if !st.KeyImported {
		t.Error("status should report the key imported")
	}

	// Stop terminates it.
	stop, err := m.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stop.Stopped {
		t.Error("Stop should report stopped")
	}
}

// TestStartReusesRunningInstance: a second Start against a DB that already has a
// live server must reuse it (same pid/port, key re-pinned) rather than orphan it
// by spawning a second one.
func TestStartReusesRunningInstance(t *testing.T) {
	home := t.TempDir()
	cacheDir := t.TempDir()
	kp, err := apiclient.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	apiKey := keys.SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	dbPid := filepath.Join(home, "sandbox", DBDefaultName+"-pid")
	denoBin := fakeDeno(t, dbPid, kp.PrivatePEM, apiKey)
	denoDir := filepath.Dir(denoBin)
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	m := New(Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}, Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
	})

	res1, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	defer func() { _, _ = m.Stop(context.Background()) }()

	res2, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start #2 should reuse, not error: %v", err)
	}
	if res2.PID != res1.PID || res2.Port != res1.Port {
		t.Errorf("re-start must reuse the running instance, got #1=%d/%d #2=%d/%d",
			res1.PID, res1.Port, res2.PID, res2.Port)
	}
	if res2.Imported {
		t.Error("re-start of an existing sandbox key should re-pin (Imported=false), not import anew")
	}
	// The pidfile must still point at the original (reused) instance.
	if pf, perr := m.readPidfile(); perr != nil || pf.PID != res1.PID {
		t.Errorf("pidfile should still track the original instance %d, got %+v (err %v)", res1.PID, pf, perr)
	}
}

func TestStartRefusesClobberingRealKey(t *testing.T) {
	home := t.TempDir()
	cacheDir := t.TempDir()
	kp, _ := apiclient.GenerateKeypair()
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	// Pre-create a REAL key named "sandbox".
	if _, err := km.Add("sandbox", kp.PrivatePEM, "file"); err != nil {
		t.Fatal(err)
	}
	if err := km.Bind("sandbox", "live-key-1"); err != nil {
		t.Fatal(err)
	}

	dbPid := filepath.Join(home, "sandbox", DBDefaultName+"-pid")
	denoBin := fakeDeno(t, dbPid, kp.PrivatePEM, keys.SandboxAPIKeyPrefix+"K1")
	denoDir := filepath.Dir(denoBin)
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	m := New(Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}, Deps{
		Doer: doer, Now: func() int64 { return 1 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
	})
	_, err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a sandbox key") {
		t.Fatalf("Start should refuse to clobber a real key, got %v", err)
	}
	// Clean up the spawned process (Start spawned before the import refusal).
	if pf, perr := m.readPidfile(); perr == nil {
		_, _ = m.Stop(context.Background())
		_ = pf
	}
}

// TestStartRejectsNonSandboxKeyNameEarly verifies that a --key-name lacking the
// sandbox token fails before anything is spawned — no runtime resolution, no
// process, no filesystem state — so a bad name never leaves a running server
// behind that the key import would then refuse.
func TestStartRejectsNonSandboxKeyNameEarly(t *testing.T) {
	home := t.TempDir()
	// No Doer/LookPath/KeyManager wired: if Start got past the early check it
	// would fail differently (or panic), proving the check fired first.
	m := New(Config{Home: home, CacheDir: t.TempDir(), KeyName: "prod"}, Deps{})
	_, err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("Start with --key-name prod should be refused early, got %v", err)
	}
	// Nothing should have been created under the home's sandbox state dir.
	if _, statErr := os.Stat(filepath.Join(home, "sandbox")); statErr == nil {
		t.Error("Start should not have created the state dir before the key-name check")
	}
}

// TestDenoRunPermsLeastPrivilege pins the tightened managed-Deno permission set:
// it grants exactly the scopes the bundle needs and never the blanket --allow-all
// or the capabilities the bundle never uses (run/ffi/import/sys). This is the
// guard against silently widening back to --allow-all.
func TestDenoRunPermsLeastPrivilege(t *testing.T) {
	home := t.TempDir()
	m := New(Config{Home: home, CacheDir: t.TempDir()}, Deps{})
	perms := m.denoRunPerms()
	joined := strings.Join(perms, " ")

	want := []string{
		"--no-prompt",
		"--allow-read=" + m.stateDir(),
		"--allow-write=" + m.stateDir(),
		"--allow-net=[::1],127.0.0.1,*.digitalx.miraeasset.com,*.korbit.co.kr",
		"--allow-env=DIGITALX_SANDBOX_*,KORBIT_SANDBOX_*,NODE_OPTIONS,LANG,LANGUAGE,LC_ALL,LC_MESSAGES",
		"--allow-sys=osRelease,cpus,systemMemoryInfo",
	}
	if joined != strings.Join(want, " ") {
		t.Errorf("denoRunPerms = %v\nwant %v", perms, want)
	}
	for _, banned := range []string{"--allow-all", "--allow-run", "--allow-ffi", "--allow-import"} {
		if strings.Contains(joined, banned) {
			t.Errorf("denoRunPerms must not grant %s, got %v", banned, perms)
		}
	}
	// The bundle reads its config knobs under both namespaces, so --allow-env must
	// grant both prefixes — otherwise a knob set under the other spelling is
	// unreadable to it.
	for _, prefix := range sandboxEnvPrefixes {
		if !strings.Contains(joined, prefix+"*") {
			t.Errorf("--allow-env must grant %s*, got %v", prefix, perms)
		}
	}
}

// TestExecArgsInjectsDB pins that --db is injected right after the subcommand
// (the first non-flag token) and only when the user didn't name one — so a
// leading flag (e.g. a forwarded --help) is preserved as the bundle's first
// argument rather than displaced.
func TestExecArgsInjectsDB(t *testing.T) {
	m := New(Config{DB: "/tmp/sb.db"}, Deps{})
	eq := func(got, want []string) {
		t.Helper()
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	// Normal: subcommand first → --db right after it.
	eq(m.execArgs([]string{"set-balance", "--user", "1"}),
		[]string{"set-balance", "--db", "/tmp/sb.db", "--user", "1"})
	// Leading flag (forwarded --help): preserved first, --db appended.
	eq(m.execArgs([]string{"--help"}),
		[]string{"--help", "--db", "/tmp/sb.db"})
	// User-supplied --db: passed through untouched (no second --db).
	eq(m.execArgs([]string{"set-balance", "--db", "/other.db"}),
		[]string{"set-balance", "--db", "/other.db"})
	// User-supplied --db=value form: also detected, passed through untouched.
	eq(m.execArgs([]string{"set-balance", "--db=/other.db"}),
		[]string{"set-balance", "--db=/other.db"})
}

// TestExecForwardsArgs drives the real subprocess path (m.Exec → resolved Deno)
// with a recording fake deno, pinning that the bundle subcommand and its flags
// are forwarded verbatim with --db injected after the subcommand.
func TestExecForwardsArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-runtime exec test is unix-only")
	}
	home := t.TempDir()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	denoBin := filepath.Join(dir, "deno")
	// Record everything after `run` so the test sees the perms + bundle + tail.
	script := "#!/bin/sh\nshift\nprintf '%s\\n' \"$*\" > " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(denoBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.mjs")
	if err := os.WriteFile(bundle, []byte("// fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, CacheDir: t.TempDir(), RuntimePref: "deno", URL: bundle}
	deps := Deps{
		LookPath: func(name string) (string, error) { return filepath.Join(dir, name), nil },
	}
	m := New(cfg, deps)
	if err := m.Exec(context.Background(), []string{"set-balance", "--user", "1"}, os.Stderr, os.Stderr); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	rec, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(rec))
	wantTail := "set-balance --db " + m.dbPath() + " --user 1"
	if !strings.HasSuffix(got, wantTail) {
		t.Errorf("forwarded args = %q, want suffix %q", got, wantTail)
	}
	if !strings.Contains(got, bundle) {
		t.Errorf("forwarded args %q should reference the bundle %q", got, bundle)
	}
}

// TestLicenseForwardsNoDB pins that `License` runs the bundle's `license`
// subcommand with the forwarded --lang and, crucially, injects NO --db (license
// opens no database).
func TestLicenseForwardsNoDB(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake-runtime license test is unix-only")
	}
	home := t.TempDir()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	denoBin := filepath.Join(dir, "deno")
	script := "#!/bin/sh\nshift\nprintf '%s\\n' \"$*\" > " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(denoBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.mjs")
	if err := os.WriteFile(bundle, []byte("// fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, CacheDir: t.TempDir(), RuntimePref: "deno", URL: bundle}
	deps := Deps{LookPath: func(name string) (string, error) { return filepath.Join(dir, name), nil }}
	m := New(cfg, deps)
	if err := m.License(context.Background(), []string{"--lang", "ko"}, os.Stderr, os.Stderr); err != nil {
		t.Fatalf("License: %v", err)
	}
	got := strings.TrimSpace(string(rec(t, argsFile)))
	if !strings.HasSuffix(got, "license --lang ko") {
		t.Errorf("forwarded args = %q, want suffix %q", got, "license --lang ko")
	}
	if strings.Contains(got, "--db") {
		t.Errorf("license must not inject --db, got %q", got)
	}
}

func rec(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
