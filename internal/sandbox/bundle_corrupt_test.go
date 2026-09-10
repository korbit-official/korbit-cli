// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
)

func TestBundleCorruptFrom(t *testing.T) {
	// The parse error Deno prints when the cached body is an HTML page — the
	// DOCTYPE source frame is the tell.
	htmlTail := "error: SyntaxError: Expected ';', '}' or <eof>\n  |\n1 | <!DOCTYPE html>\n"
	if bundleCorruptFrom(htmlTail) == nil {
		t.Error("an HTML DOCTYPE in the parse error should classify as a corrupt bundle")
	}
	if bundleCorruptFrom(`1 | <html lang="ko">`) == nil {
		t.Error("a bare <html tag should classify as a corrupt bundle")
	}
	// Ordinary output, and a non-HTML syntax error, must not classify — reloading
	// on a genuine bundle bug would just fail identically after a wasted fetch.
	if bundleCorruptFrom("digitalx-sandbox listening on http://127.0.0.1:9999") != nil {
		t.Error("normal output must not classify as corrupt")
	}
	if bundleCorruptFrom("error: SyntaxError: Unexpected token 'x'") != nil {
		t.Error("a non-HTML syntax error must not classify as corrupt")
	}
}

// fakeDenoCorrupt models Deno holding an HTML page (not JS) in its module cache
// for the remote bundle URL: while the state file is absent/"bad", EVERY sub of
// the bundle (license, status, init-db, run alike) fails with the module-parse
// error Deno prints for an HTML body (the `<!DOCTYPE html>` source frame) — the
// cache entry is poisoned for the module, not for one subcommand. A `cache`
// invocation (deno cache --reload) refreshes the cache to a good bundle (state
// "good"), so the retry succeeds.
func fakeDenoCorrupt(t *testing.T, dbPidPath, stateFile, runCountFile, seededPEM, apiKey string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-runtime lifecycle test is unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "deno")
	statusJSON, _ := json.Marshal(statusDoc{Users: []statusUser{{Keys: []statusKey{
		{APIKey: apiKey, Type: "ed25519", Secret: seededPEM},
	}}}})
	body := fmt.Sprintf(`#!/bin/sh
mode="$1"; shift
STATE_FILE="%s"
RUN_COUNT="%s"
# The bundle "refresh" (deno cache --reload) replaces the cached HTML with real JS.
if [ "$mode" = "cache" ]; then echo good > "$STATE_FILE"; exit 0; fi
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
# Count every server-spawn attempt (the bundle run sub), pass or fail, so a test
# can assert the recovery does not waste an extra spawn on the port fallback.
if [ "$sub" = "run" ]; then echo x >> "$RUN_COUNT"; fi
state=$(cat "$STATE_FILE" 2>/dev/null || echo bad)
if [ "$state" = "bad" ]; then
  echo "error: SyntaxError: Expected ';', '}' or <eof>" 1>&2
  echo "  |" 1>&2
  echo "1 | <!DOCTYPE html>" 1>&2
  exit 1
fi
case "$sub" in
  license)
    echo "sandbox license notice"
    # State "license-fails": the notice is printed, then the invocation fails for
    # some reason that is NOT a corrupt bundle.
    if [ "$state" = "license-fails" ]; then echo "error: something else" 1>&2; exit 1; fi
    ;;
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
`, stateFile, runCountFile, dbPidPath, string(statusJSON))
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// corruptBundleManager wires a Manager over the corrupt-then-good fake deno. It
// returns the run-count file so a caller can assert how many server spawns the
// recovery took. A nil banner leaves deps.BannerOut unset, so `start` makes no
// license invocation and the corrupt cache is first met further down the
// sequence (the status read, init-db, or the detached run).
func corruptBundleManager(t *testing.T, logs *[]string, banner io.Writer) (*Manager, string) {
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
	stateFile := filepath.Join(cacheDir, "corrupt_state") // absent ⇒ "bad"
	runCount := filepath.Join(cacheDir, "run_count")
	denoBin := fakeDenoCorrupt(t, dbPid, stateFile, runCount, kp.PrivatePEM, apiKey)
	denoDir := filepath.Dir(denoBin)
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})
	deps := Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
		BannerOut:  banner,
	}
	if logs != nil {
		deps.Log = func(s string) { *logs = append(*logs, s) }
	}
	// URL empty ⇒ the remote Official Source, so the corrupt-cache recovery (which
	// only fires for a remote source) is eligible.
	return New(Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}, deps), runCount
}

// corruptStateFile is the fake runtime's cache-state file for a manager built by
// corruptBundleManager; writing "bad" into it re-poisons the module cache.
func corruptStateFile(m *Manager) string {
	return filepath.Join(m.cfg.CacheDir, "corrupt_state")
}

// runSpawnCount reports how many times the fake's server `run` sub was invoked.
func runSpawnCount(t *testing.T, runCountFile string) int {
	t.Helper()
	raw, err := os.ReadFile(runCountFile)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "x")
}

// TestStartRecoversFromCorruptBundle covers the runInitDB emit site: a fresh
// (uninitialized) db fails init-db while the cache holds HTML, then the reload +
// retry succeeds.
func TestStartRecoversFromCorruptBundle(t *testing.T) {
	var logs []string
	m, _ := corruptBundleManager(t, &logs, nil)

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start should recover by refreshing the cached bundle: %v", err)
	}
	if res.Port != 9999 || !res.Imported {
		t.Errorf("unexpected start result: %+v", res)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "refreshing the bundle and retrying once") {
		t.Errorf("expected a refresh-and-retry log line, got:\n%s", joined)
	}
	_, _ = m.Stop(context.Background())
}

// TestStartRecoversFromCorruptBundleAlreadyInitialized covers the spawnAndWait
// emit site: with the db already initialized, init-db is skipped so the corrupt
// cache is first detected on the detached `run` (classified from run.log). It also
// asserts the corrupt failure skips the ephemeral-port fallback — exactly two
// server spawns (the failing one + the post-reload success), not three.
func TestStartRecoversFromCorruptBundleAlreadyInitialized(t *testing.T) {
	var logs []string
	m, runCount := corruptBundleManager(t, &logs, nil)

	// Pre-create the db file so dbInitialized() is true and init-db is skipped.
	if err := os.MkdirAll(filepath.Dir(m.dbPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.dbPath(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start should recover on the run path: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("port = %d, want 9999", res.Port)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "refreshing the bundle and retrying once") {
		t.Error("expected the refresh-and-retry log line")
	}
	if got := runSpawnCount(t, runCount); got != 2 {
		t.Errorf("corrupt recovery spawned the server %d times, want 2 (a port-fallback spawn would make it 3)", got)
	}
	_, _ = m.Stop(context.Background())
}

// TestPaperStartRecoversFromCorruptBundle covers the paper-mode gate: with an
// existing db, `start --paper` reads `status --json` BEFORE any bring-up, so that
// read — not init-db or run — is the first invocation to meet the corrupt cache.
func TestPaperStartRecoversFromCorruptBundle(t *testing.T) {
	var logs []string
	m, _ := corruptBundleManager(t, &logs, nil)
	m.cfg.Paper = true

	// An existing db is what makes ensurePaperDB read the status doc at all.
	if err := os.MkdirAll(filepath.Dir(m.dbPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.dbPath(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("start --paper should recover on the paper-mode status read: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("port = %d, want 9999", res.Port)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "refreshing the bundle and retrying once") {
		t.Error("expected the refresh-and-retry log line")
	}
	_, _ = m.Stop(context.Background())
}

// TestReusedServerStartRecoversFromCorruptBundle covers the idempotent-restart
// path: a healthy server is reused, so nothing is spawned and the only bundle
// invocation left is the status read behind finishStart. The cache is re-poisoned
// after the first start to model a refresh that landed an HTML body while the
// server kept running off the module it had already loaded.
func TestReusedServerStartRecoversFromCorruptBundle(t *testing.T) {
	m, _ := corruptBundleManager(t, nil, nil)
	if _, err := m.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer func() { _, _ = m.Stop(context.Background()) }()

	if err := os.WriteFile(corruptStateFile(m), []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	m.deps.Log = func(s string) { logs = append(logs, s) }

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("restart of a running sandbox should recover on the status read: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("port = %d, want 9999", res.Port)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "reusing it") {
		t.Errorf("expected the reuse path, got:\n%s", joined)
	}
	if !strings.Contains(joined, "refreshing the bundle and retrying once") {
		t.Errorf("expected the refresh-and-retry log line, got:\n%s", joined)
	}
}

// TestStartBannerRecoversFromCorruptBundle covers `start`'s FIRST bundle
// invocation: the license banner. It heals the cache itself, so the notice still
// prints (a corrupt bundle must not silently cost the license notice).
func TestStartBannerRecoversFromCorruptBundle(t *testing.T) {
	var banner bytes.Buffer
	var logs []string
	m, _ := corruptBundleManager(t, &logs, &banner)

	res, err := m.Start(context.Background())
	if err != nil {
		t.Fatalf("Start should recover on the banner invocation: %v", err)
	}
	if res.Port != 9999 {
		t.Errorf("port = %d, want 9999", res.Port)
	}
	if !strings.Contains(banner.String(), "sandbox license notice") {
		t.Errorf("the license notice should print after the refresh, got %q", banner.String())
	}
	// The failed attempt's Deno stack trace must not ride along in front of the
	// notice — the buffer is reset per attempt, and only the survivor is relayed.
	if strings.Contains(banner.String(), "DOCTYPE") {
		t.Errorf("the failed attempt's output leaked into the banner: %q", banner.String())
	}
	// The banner's own recovery leaves a good cache, so nothing downstream had to
	// refresh again: exactly one refresh-and-retry for the whole start.
	if n := strings.Count(strings.Join(logs, "\n"), "refreshing the bundle and retrying once"); n != 1 {
		t.Errorf("refresh-and-retry happened %d times, want 1", n)
	}
	_, _ = m.Stop(context.Background())
}

// TestStartBannerSurvivesNonCorruptFailure pins that ONLY a corrupt bundle
// withholds the notice: a bundle that prints it and then exits non-zero for an
// unrelated reason still gets its output relayed, exactly as streaming did.
func TestStartBannerSurvivesNonCorruptFailure(t *testing.T) {
	var banner bytes.Buffer
	m, _ := corruptBundleManager(t, nil, &banner)
	if err := os.WriteFile(corruptStateFile(m), []byte("license-fails\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Start(context.Background()); err != nil {
		t.Fatalf("a banner failure must never block the start: %v", err)
	}
	if !strings.Contains(banner.String(), "sandbox license notice") {
		t.Errorf("the notice the bundle printed should still reach the user, got %q", banner.String())
	}
	_, _ = m.Stop(context.Background())
}
