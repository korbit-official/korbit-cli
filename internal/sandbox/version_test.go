// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

func TestParseSandboxVersion(t *testing.T) {
	cases := map[string]string{
		"0.1.0\n":                                "0.1.0",
		"noise\n0.2.3\nmore":                     "0.2.3",
		"1.2.3-dev\n":                            "1.2.3-dev",
		"SANDBOX_VERSION_TOO_OLD: this is 0.1.0": "", // not a bare-version line
		"no version here":                        "",
	}
	for in, want := range cases {
		if got := parseSandboxVersion(in); got != want {
			t.Errorf("parseSandboxVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionTooOldFrom(t *testing.T) {
	// Output carrying the refusal prefix → typed error with the parsed version.
	out := "0.0.1\n" + VersionTooOldPrefix + " this sandbox bundle is 0.0.1, but the tool requires 9.9.9 or newer\n"
	vt := versionTooOldFrom(out)
	if vt == nil {
		t.Fatal("expected a versionTooOldError")
	}
	if vt.have != "0.0.1" {
		t.Errorf("have = %q, want 0.0.1", vt.have)
	}
	if !isVersionTooOld(vt) {
		t.Error("isVersionTooOld should recognize the error")
	}
	if !strings.Contains(vt.Error(), MinSandboxVersion) {
		t.Errorf("Error() should mention the required min %q: %q", MinSandboxVersion, vt.Error())
	}

	// Ordinary output → not a version refusal.
	if versionTooOldFrom("digitalx-sandbox listening on http://127.0.0.1:9999") != nil {
		t.Error("non-refusal output should not classify as version-too-old")
	}
	if isVersionTooOld(fmt.Errorf("some other error")) {
		t.Error("an unrelated error must not be classified as version-too-old")
	}
}

// fakeDenoVersioned is like fakeDeno but enforces the min-version gate: while the
// state file holds "old" and DIGITALX_SANDBOX_MIN_VERSION is set, init-db/run
// print a bare version + the refusal prefix and exit non-zero. A `cache`
// invocation (the bundle update) flips the state to "new" when flipOnCache is
// set, so a retry then succeeds; with it clear the refresh keeps serving the same
// old bundle (what a pinned bundle source does). It refuses outright if the two
// spellings of the name disagree — the manager must set both to the same value.
func fakeDenoVersioned(t *testing.T, dbPidPath, stateFile, seededPEM, apiKey string, flipOnCache bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-runtime lifecycle test is unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "deno")
	statusJSON, _ := json.Marshal(statusDoc{Users: []statusUser{{Keys: []statusKey{
		{APIKey: apiKey, Type: "ed25519", Secret: seededPEM},
	}}}})
	onCache := `exit 0`
	if flipOnCache {
		onCache = `echo new > "$STATE_FILE"; exit 0`
	}
	body := fmt.Sprintf(`#!/bin/sh
mode="$1"; shift
STATE_FILE="%s"
# The bundle "update" (deno cache --reload): whether it makes a newer bundle
# available depends on what the source serves.
if [ "$mode" = "cache" ]; then %s; fi
if [ "$mode" != "run" ]; then exit 0; fi
while [ "$#" -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
shift              # drop the bundle ref
sub="$1"; shift
db=""; port="9999"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --db) db="$2"; shift 2 ;;
    --port) port="$2"; shift 2 ;;
    *) shift ;;
  esac
done
state=$(cat "$STATE_FILE" 2>/dev/null || echo old)
if [ "$DIGITALX_SANDBOX_MIN_VERSION" != "$KORBIT_SANDBOX_MIN_VERSION" ]; then
  echo "MIN_VERSION env names disagree: '$DIGITALX_SANDBOX_MIN_VERSION' vs '$KORBIT_SANDBOX_MIN_VERSION'" 1>&2
  exit 9
fi
if [ -n "$DIGITALX_SANDBOX_MIN_VERSION" ] && [ "$state" = "old" ]; then
  case "$sub" in
    init-db|run)
      echo "0.0.1"
      echo "SANDBOX_VERSION_TOO_OLD: this sandbox bundle is 0.0.1, but the tool requires $DIGITALX_SANDBOX_MIN_VERSION or newer" 1>&2
      exit 7
      ;;
  esac
fi
case "$sub" in
  init-db) : > "$db" ;;
  run)
    if [ "$port" = "0" ]; then port=45999; fi
    printf '{"pid":%%s,"port":%%s}' "$$" "$port" > "%s"
    echo "digitalx-sandbox listening on http://127.0.0.1:$port"
    while true; do sleep 1; done
    ;;
  status)
    cat <<'JSON'
%s
JSON
    ;;
esac
`, stateFile, onCache, dbPidPath, string(statusJSON))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// versionGateManager wires a Manager over the versioned fake deno. url pins the
// bundle source (empty ⇒ the Official Source); flipOnCache says whether the
// bundle update makes a newer bundle available.
func versionGateManager(t *testing.T, skip bool, logs *[]string, url string, flipOnCache bool) *Manager {
	t.Helper()
	home := t.TempDir()
	cacheDir := t.TempDir()
	kp, err := apiclient.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	apiKey := keys.SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	dbPid := filepath.Join(home, "sandbox", "digitalx-sandbox.db-pid")
	stateFile := filepath.Join(cacheDir, "version_state") // defaults to "old" (absent)
	denoBin := fakeDenoVersioned(t, dbPid, stateFile, kp.PrivatePEM, apiKey, flipOnCache)
	denoDir := filepath.Dir(denoBin)
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	deps := Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
	}
	if logs != nil {
		deps.Log = func(s string) { *logs = append(*logs, s) }
	}
	return New(Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno", SkipVersionCheck: skip, URL: url}, deps)
}

func TestWithRuntimeEnvControlsSandboxNamespace(t *testing.T) {
	// Stray user-exported bundle knobs across BOTH config namespaces the CLI
	// owns, plus an ambient non-sandbox var that must be forwarded untouched.
	t.Setenv(MinVersionEnv, "99.0.0")
	t.Setenv(LegacyMinVersionEnv, "99.0.0")
	t.Setenv(sandboxEnvPrefix+"STRAY", "boom")
	t.Setenv(legacySandboxEnvPrefix+"STRAY", "boom")
	t.Setenv("DIGITALX_CLI_SANDBOX_NOT_PREFIXED", "keep") // not the bundle prefix
	t.Setenv("LANG", "ko_KR.UTF-8")                       // ambient: must pass through

	// Non-gated invocation (extra carries no bundle vars): every inherited var in
	// either namespace is dropped, so the CLI fully controls them.
	cmd := exec.Command("true")
	withRuntimeEnv(cmd, []string{"DENO_DIR=/tmp/x"})
	var sawLang, sawNotPrefixed bool
	for _, e := range cmd.Env {
		for _, prefix := range sandboxEnvPrefixes {
			if strings.HasPrefix(e, prefix) {
				t.Fatalf("inherited %s* var should have been stripped, got %q", prefix, e)
			}
		}
		switch e {
		case "LANG=ko_KR.UTF-8":
			sawLang = true
		case "DIGITALX_CLI_SANDBOX_NOT_PREFIXED=keep":
			sawNotPrefixed = true
		}
	}
	if !sawLang {
		t.Error("ambient LANG should be forwarded to the child")
	}
	if !sawNotPrefixed {
		t.Error("a var that merely contains SANDBOX but lacks the bundle prefix must be forwarded")
	}

	// Gated invocation: only the manager-injected values reach the bundle.
	cmd2 := exec.Command("true")
	withRuntimeEnv(cmd2, append([]string{"DENO_DIR=/tmp/x"}, bundleEnv(MinVersionEnv, LegacyMinVersionEnv, "0.1.0")...))
	var found []string
	for _, e := range cmd2.Env {
		if strings.HasPrefix(e, MinVersionEnv+"=") || strings.HasPrefix(e, LegacyMinVersionEnv+"=") {
			found = append(found, e)
		}
	}
	want := []string{MinVersionEnv + "=0.1.0", LegacyMinVersionEnv + "=0.1.0"}
	if strings.Join(found, " ") != strings.Join(want, " ") {
		t.Fatalf("manager-injected min-version should survive under both names, got %v want %v", found, want)
	}
}

// TestVersionGateEnvSetsBothNames pins the dual-name contract for the version
// gate: the child env carries the same MinSandboxVersion under the current and
// the earlier spelling, so a bundle reading either one is gated. Skipping the
// check sets neither.
func TestVersionGateEnvSetsBothNames(t *testing.T) {
	m := New(Config{Home: t.TempDir()}, Deps{})
	got := m.versionGateEnv()
	want := []string{
		MinVersionEnv + "=" + MinSandboxVersion,
		LegacyMinVersionEnv + "=" + MinSandboxVersion,
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("versionGateEnv() = %v, want %v", got, want)
	}
	if MinVersionEnv == LegacyMinVersionEnv {
		t.Fatal("the two min-version env names must be distinct spellings")
	}
	skipped := New(Config{Home: t.TempDir(), SkipVersionCheck: true}, Deps{})
	if env := skipped.versionGateEnv(); len(env) != 0 {
		t.Errorf("--skip-version-check must arm no gate, got %v", env)
	}
}

func TestStartVersionGateUpdatesAndRetries(t *testing.T) {
	var logs []string
	m := versionGateManager(t, false, &logs, "", true)

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start should recover by updating the bundle: %v", err)
	}
	if res.Port != 9999 || !res.Imported {
		t.Errorf("unexpected start result: %+v", res)
	}
	// It announced the update-and-retry.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "updating the bundle and retrying once") {
		t.Errorf("expected an update-and-retry log line, got:\n%s", joined)
	}
	_, _ = m.Stop(context.Background())
}

func TestStartSkipVersionCheck(t *testing.T) {
	// With the check skipped, MinVersionEnv is never set, so the bundle never
	// refuses — start succeeds on the first attempt, no update.
	var logs []string
	m := versionGateManager(t, true, &logs, "", true)
	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start with --skip-version-check: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("port = %d", res.Port)
	}
	if strings.Contains(strings.Join(logs, "\n"), "updating the bundle") {
		t.Error("skip-version-check must not trigger an update")
	}
	_, _ = m.Stop(context.Background())
}

// TestVersionGateExhaustedAdvice covers the advice the start path gives when the
// update-and-retry still meets a too-old bundle. With the Official Source the way
// out is --skip-version-check or a CLI update; with a PINNED bundle source neither
// can help (the refresh re-fetches the same source, and skipping the gate only
// defers the failure to a flag the old bundle lacks), so the message must name the
// pinned source, the override variable, and the supported floor.
func TestVersionGateExhaustedAdvice(t *testing.T) {
	t.Run("official source", func(t *testing.T) {
		var logs []string
		m := versionGateManager(t, false, &logs, "", false)
		_, err := m.Start(context.Background())
		if err == nil {
			t.Fatal("Start should fail: the bundle stays too old")
		}
		msg := err.Error()
		if !strings.Contains(msg, "--skip-version-check") || !strings.Contains(msg, progname.Name()) {
			t.Errorf("expected the skip/update advice, got: %s", msg)
		}
		if strings.Contains(msg, EnvSandboxURL) {
			t.Errorf("no source is pinned, so the message must not name %s: %s", EnvSandboxURL, msg)
		}
	})

	t.Run("pinned source", func(t *testing.T) {
		const pinned = "https://example.test/pinned-sandbox.mjs"
		var logs []string
		m := versionGateManager(t, false, &logs, pinned, false)
		_, err := m.Start(context.Background())
		if err == nil {
			t.Fatal("Start should fail: the pinned bundle stays too old")
		}
		msg := err.Error()
		for _, want := range []string{pinned, EnvSandboxURL, MinSandboxVersion, DefaultSandboxURL} {
			if !strings.Contains(msg, want) {
				t.Errorf("pinned-source advice should name %q, got: %s", want, msg)
			}
		}
		if strings.Contains(msg, "--skip-version-check") {
			t.Errorf("skipping the gate cannot help a pinned old bundle, so don't advise it: %s", msg)
		}
	})
}

func TestLicenseSetsFooterCommandEnv(t *testing.T) {
	// The License path (which backs `sandbox license` and the start banner) sets
	// the license-command override under both env names, so the bundle banner
	// footer names this CLI's own command whichever spelling it reads. Reuse the
	// standard fake (its license branch echoes both).
	home := t.TempDir()
	cacheDir := t.TempDir()
	dbPid := filepath.Join(home, "sandbox", "digitalx-sandbox.db-pid")
	denoBin := fakeDeno(t, dbPid, "pem", "SANDBOX_K")
	denoDir := filepath.Dir(denoBin)
	m := New(Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}, Deps{
		LookPath: func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
	})
	var out bytes.Buffer
	if err := m.License(context.Background(), []string{"--show-banner"}, &out, &out); err != nil {
		t.Fatalf("License: %v", err)
	}
	for _, want := range []string{
		"LICENSE_CMD=dgx-cli sandbox license",
		"LEGACY_LICENSE_CMD=dgx-cli sandbox license",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("expected %q in the child env, got: %q", want, out.String())
		}
	}
}
