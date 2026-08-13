// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
)

// TestKeyAddPinsBaseURL: `key add --base-url` persists the host on the new key,
// deriving the WebSocket companion from it.
func TestKeyAddPinsBaseURL(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	_, stderr, code := runCLI([]string{"key", "add", "bot", "--base-url", "https://api-test.korbit.co.kr/", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("key add --base-url exit=%d — %s", code, stderr)
	}
	out, _, _ := runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"baseUrl":"https://api-test.korbit.co.kr"`) ||
		!strings.Contains(out, `"wsBaseUrl":"wss://ws-api-test.korbit.co.kr"`) {
		t.Fatalf("key add did not pin the base URL: %s", out)
	}
}

// TestKeyAddPinsExplicitWSBaseURL: an explicit --ws-base-url is stored verbatim
// rather than derived.
func TestKeyAddPinsExplicitWSBaseURL(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	_, stderr, code := runCLI([]string{"key", "add", "bot",
		"--base-url", "https://api-test.korbit.co.kr", "--ws-base-url", "wss://stream.example.test/", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	out, _, _ := runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"wsBaseUrl":"wss://stream.example.test"`) {
		t.Fatalf("explicit ws base URL not stored: %s", out)
	}
}

// TestKeyAddHMACPinsBaseURL: the hmac-sha256 create path (a distinct call site)
// also pins --base-url.
func TestKeyAddHMACPinsBaseURL(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	secretFile := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretFile, []byte("a-shared-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI([]string{"key", "add", "hmac-bot", "--type", "hmac-sha256", "--api-key", "KEYID-H",
		"--secret-file", secretFile, "--base-url", "https://api-test.korbit.co.kr", "--compact"}, env, &stubDoer{})
	if code != 0 {
		t.Fatalf("key add hmac --base-url exit=%d — %s", code, stderr)
	}
	out, _, _ := runCLI([]string{"key", "show", "hmac-bot", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"baseUrl":"https://api-test.korbit.co.kr"`) {
		t.Fatalf("hmac key add did not pin the base URL: %s", out)
	}
}

// TestKeyAddInvalidBaseURLRejectedBeforeCreate: an invalid --base-url fails up
// front (usage error) and creates no key.
func TestKeyAddInvalidBaseURLRejectedBeforeCreate(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	_, stderr, code := runCLI([]string{"key", "add", "bot", "--base-url", "ftp://nope", "--compact"}, env, &stubDoer{})
	if code != 2 {
		t.Fatalf("invalid --base-url must be a usage error (exit 2), got %d — %s", code, stderr)
	}
	if _, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("bot"); err == nil {
		t.Fatalf("an invalid --base-url must not create the key")
	}
}

// TestKeyAddWSBaseURLWithoutBaseURL: --ws-base-url alone is a usage error (it
// needs a REST host to anchor to) — rejected up front before any key is created,
// not silently dropped with a stderr-only note an stdout consumer would miss.
func TestKeyAddWSBaseURLWithoutBaseURL(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	out, stderr, code := runCLI([]string{"key", "add", "bot", "--ws-base-url", "wss://stream.example.test", "--compact"}, env, &stubDoer{})
	if code != 2 {
		t.Fatalf("--ws-base-url without --base-url must be a usage error, exit=%d out=%s stderr=%s", code, out, stderr)
	}
	if !strings.Contains(stderr, "--ws-base-url needs --base-url") {
		t.Fatalf("expected a usage error pointing at --base-url: %s", stderr)
	}
	// The key must not have been created by a rejected invocation.
	if showOut, _, _ := runCLI([]string{"key", "show", "bot", "--compact"}, env, &stubDoer{}); strings.Contains(showOut, `"name":"bot"`) {
		t.Fatalf("a rejected --ws-base-url create must not leave a key: %s", showOut)
	}
}

// TestSetupPinsBaseURLOnCreate: `setup --base-url` pins the host when it first
// creates the key.
func TestSetupPinsBaseURLOnCreate(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	_, stderr, code := runWithDeps([]string{"setup", "--name", "fresh", "--base-url", "https://api-test.korbit.co.kr", "--compact"},
		env, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("setup --base-url exit=%d — %s", code, stderr)
	}
	out, _, _ := runCLI([]string{"key", "show", "fresh", "--compact"}, env, &stubDoer{})
	if !strings.Contains(out, `"baseUrl":"https://api-test.korbit.co.kr"`) {
		t.Fatalf("setup did not pin the base URL on create: %s", out)
	}
}

// TestSetupReRunIgnoresBaseURL: re-running setup on an existing key does not
// change its base URL — pinning applies only on first create.
func TestSetupReRunIgnoresBaseURL(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}
	seedUnboundKey(t, home, "default") // exists, no base URL
	_, stderr, code := runWithDeps([]string{"setup", "--base-url", "https://api-test.korbit.co.kr", "--compact"},
		env, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("setup re-run exit=%d — %s", code, stderr)
	}
	out, _, _ := runCLI([]string{"key", "show", "default", "--compact"}, env, &stubDoer{})
	if strings.Contains(out, "api-test.korbit.co.kr") {
		t.Fatalf("a setup re-run must not pin the base URL: %s", out)
	}
}
