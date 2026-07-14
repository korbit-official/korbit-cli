// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/cli/setupui"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// skewThenOKDoer returns EXCEED_TIME_WINDOW on the first whoami and 200 after,
// with a valid /v2/time, so the pre-bind verify must perform the corrective
// resync to succeed. whoamiCalls counts the signed currentKeyInfo requests.
type skewThenOKDoer struct{ whoamiCalls *int }

func (d skewThenOKDoer) Do(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/v2/time") {
		return resp(200, `{"success":true,"data":{"time":1700000000100}}`, nil), nil
	}
	*d.whoamiCalls++
	if *d.whoamiCalls == 1 {
		return resp(400, `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW","description":"skew"}}`, nil), nil
	}
	return resp(200, `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readOrders","writeOrders"]}}`, nil), nil
}

// runWithSetupUI is runWithDeps plus a stubbed interactive setup runner.
// Injecting a SetupUIRun forces the interactive path on (bypassing the TTY gate),
// so the wiring runs in-process against buffers.
func runWithSetupUI(args []string, env map[string]string, doer korbit.Doer,
	probe func(context.Context, string, string, string, int) (string, error),
	setupUI func(setupui.Config) error) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:     func(k string) string { return env[k] },
		Stdout:     &out,
		Stderr:     &errb,
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		FamilyDoer: func(string, int) korbit.Doer { return doer },
		IPProbe:    probe,
		WSDial:     dialFrames(),
		SetupUIRun: setupUI,
	})
	return out.String(), errb.String(), code
}

// pasteKey returns a stub runner that simulates the user pasting token and
// confirming. It records the lines Submit returned (the health-check view).
func pasteKey(token string, gotLines *[]string) func(setupui.Config) error {
	return func(cfg setupui.Config) error {
		lines, _, err := cfg.Submit(token)
		if err != nil {
			return err
		}
		*gotLines = lines
		return nil
	}
}

// TestSetupInteractiveBindsUnboundKey: on a TTY, `setup` for an existing unbound
// key prompts for the issued id, binds it, and runs the health check in one
// session — the result document still lands on stdout.
func TestSetupInteractiveBindsUnboundKey(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	var lines []string
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""),
		pasteKey("KEYID-IX", &lines))
	if code != 0 {
		t.Fatalf("interactive setup must succeed: exit = %d — %s", code, stderr)
	}
	// The interactive UI is human-mode only; the result document lands on stdout
	// as the configured summary.
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-IX") {
		t.Fatalf("expected a freshly-configured, bound result on stdout: %s", out)
	}
	// The prompt shows only a brief health-check status; the full checklist is the
	// canonical stdout result, so the two surfaces never duplicate it.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "health check") {
		t.Fatalf("expected a brief in-UI health-check status: %v", lines)
	}
	if strings.Contains(joined, "Complementary health check") {
		t.Fatalf("the prompt must not carry the full complementary health-check section: %v", lines)
	}
	if !strings.Contains(out, "Complementary health check") {
		t.Fatalf("stdout result must carry the full health check: %s", out)
	}
	// The key is actually bound in the keystore.
	s, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if err != nil || !s.Bound || s.APIKeyID == nil || *s.APIKeyID != "KEYID-IX" {
		t.Fatalf("key was not bound to the pasted id: %+v (err %v)", s, err)
	}
}

// TestSetupInteractiveNewKey: a fresh `setup` generates the key, prompts, and
// binds the pasted id in the same session.
func TestSetupInteractiveNewKey(t *testing.T) {
	home := t.TempDir()
	var lines []string
	out, stderr, code := runWithSetupUI([]string{"setup", "--name", "fresh"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""),
		pasteKey("KEYID-FRESH", &lines))
	if code != 0 {
		t.Fatalf("interactive setup must succeed: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-FRESH") {
		t.Fatalf("expected the new key configured + bound: %s", out)
	}
	s, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("fresh")
	if err != nil || !s.Bound || s.APIKeyID == nil || *s.APIKeyID != "KEYID-FRESH" {
		t.Fatalf("new key was not bound to the pasted id: %+v (err %v)", s, err)
	}
}

// TestSetupInteractiveQuitWithoutFinishing: if the user quits before pasting an
// id, the generated key stays and setup reports awaitingRegistration (the same
// document a non-interactive run would emit), so it is resumable later.
func TestSetupInteractiveQuitWithoutFinishing(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	quit := func(cfg setupui.Config) error { return nil } // never calls Submit
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), quit)
	if code != 0 {
		t.Fatalf("quitting interactive setup must not error: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, "isn't registered yet") {
		t.Fatalf("expected the resumable (awaiting-registration) result: %s", out)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if s.Bound {
		t.Fatalf("a quit-without-finishing must leave the key unbound: %+v", s)
	}
}

// TestSetupInteractiveCtrlCEmitsNothing: aborting with Ctrl-C before pasting an id
// (stub returns setupui.ErrInterrupted) emits no document and exits 0 — matching
// the non-interactive SIGINT abort, which prints nothing — and leaves the key
// unbound.
func TestSetupInteractiveCtrlCEmitsNothing(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	abort := func(setupui.Config) error { return setupui.ErrInterrupted }
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), abort)
	if code != 0 {
		t.Fatalf("Ctrl-C abort must exit 0: %d — %s", code, stderr)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("Ctrl-C must emit no result document, got: %q", out)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if s.Bound {
		t.Fatalf("abort before submit must leave the key unbound: %+v", s)
	}
}

// TestSetupInteractiveCtrlCAfterBindEmitsNothing: Ctrl-C wins even if a bind
// already happened — no document is emitted (like the non-interactive abort), but
// the bound state persists, so a re-run recovers idempotently.
func TestSetupInteractiveCtrlCAfterBindEmitsNothing(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	abortAfterBind := func(cfg setupui.Config) error {
		if _, done, err := cfg.Submit("KEYID-MID"); err != nil || !done {
			t.Fatalf("submit should bind: done=%v err=%v", done, err)
		}
		return setupui.ErrInterrupted
	}
	out, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""), abortAfterBind)
	if code != 0 {
		t.Fatalf("Ctrl-C after bind must exit 0: %d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("Ctrl-C must emit nothing, got: %q", out)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if !s.Bound || s.APIKeyID == nil || *s.APIKeyID != "KEYID-MID" {
		t.Fatalf("a bind before the abort must persist: %+v", s)
	}
}

// TestSetupInteractiveRejectsUnregisteredKey: when the pre-bind whoami is
// rejected (KEY_NOT_FOUND), the id is reported as retryable and never bound.
func TestSetupInteractiveRejectsUnregisteredKey(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := routeDoer{whoamiStatus: 400, whoamiBody: `{"success":false,"error":{"code":400,"message":"KEY_NOT_FOUND","description":"no such key"}}`}
	var gotErr error
	stub := func(cfg setupui.Config) error {
		_, done, err := cfg.Submit("WRONG-ID")
		if done {
			t.Fatal("a server-rejected id must not complete setup")
		}
		gotErr = err
		return setupui.ErrInterrupted // the user gives up after the rejection
	}
	out, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a rejected+aborted setup must emit nothing: %q", out)
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "KEY_NOT_FOUND") {
		t.Fatalf("expected a KEY_NOT_FOUND rejection message: %v", gotErr)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if s.Bound {
		t.Fatalf("a server-rejected id must never be bound: %+v", s)
	}
}

// TestSetupInteractiveNetworkFailureDoesNotBind: if the verify whoami can't reach
// the API, the id is reported as retryable ("could not verify") and not bound.
func TestSetupInteractiveNetworkFailureDoesNotBind(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := routeDoer{whoamiErr: errors.New("dial tcp: connection refused")}
	var gotErr error
	stub := func(cfg setupui.Config) error {
		_, done, err := cfg.Submit("SOME-ID")
		if done {
			t.Fatal("a verify network failure must not complete setup")
		}
		gotErr = err
		return setupui.ErrInterrupted
	}
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "could not verify") {
		t.Fatalf("expected the could-not-verify message: %v", gotErr)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if s.Bound {
		t.Fatalf("a verify network failure must not bind: %+v", s)
	}
}

// TestSetupInteractiveResyncsOnClockSkew: a skewed local clock must not reject a
// valid id — the verify performs the corrective EXCEED_TIME_WINDOW resync and then
// binds. (Under a non-pre-exec retry policy the first rejection would strand a
// valid key, so this also guards that the resync actually fires.)
func TestSetupInteractiveResyncsOnClockSkew(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	calls := 0
	doer := skewThenOKDoer{whoamiCalls: &calls}
	var lines []string
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), pasteKey("KEYID-OK", &lines))
	if code != 0 {
		t.Fatalf("a clock-skew resync must let a valid id succeed: exit=%d — %s", code, stderr)
	}
	if calls < 2 {
		t.Fatalf("expected a corrective whoami retry after EXCEED_TIME_WINDOW, got %d call(s)", calls)
	}
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-OK") {
		t.Fatalf("expected the key bound after resync: %s", out)
	}
}

// TestSetupNoInteractiveSkipsPrompt: --no-interactive prints the link and exits
// even when an interactive runner is wired — the stub must never be called.
func TestSetupNoInteractiveSkipsPrompt(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	called := false
	stub := func(setupui.Config) error { called = true; return nil }
	// Human mode (no --compact), so only --no-interactive can suppress the prompt.
	out, stderr, code := runWithSetupUI([]string{"setup", "--no-interactive"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("setup --no-interactive must succeed: exit = %d — %s", code, stderr)
	}
	if called {
		t.Fatalf("--no-interactive must not invoke the interactive prompt")
	}
	if !strings.Contains(out, "isn't registered yet") {
		t.Fatalf("expected print-and-exit (awaiting registration): %s", out)
	}
}

// TestSetupCompactSkipsPrompt: --compact is JSON mode too (jsonOut || compact),
// so the prompt is suppressed even with a runner wired.
func TestSetupCompactSkipsPrompt(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	called := false
	stub := func(setupui.Config) error { called = true; return nil }
	out, _, code := runWithSetupUI([]string{"setup", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("setup --compact must succeed: exit = %d", code)
	}
	if called {
		t.Fatalf("--compact (JSON mode) must not invoke the interactive prompt")
	}
	if !strings.Contains(out, `"status":"awaitingRegistration"`) {
		t.Fatalf("expected the compact JSON awaitingRegistration document: %s", out)
	}
}

// TestSetupInteractiveSubmitValidation: the Submit closure rejects an empty and a
// sandbox-prefixed id as retryable (no bind), and a valid id after those rejections
// still binds — the keystore reflects only the accepted id.
func TestSetupInteractiveSubmitValidation(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	stub := func(cfg setupui.Config) error {
		if _, done, err := cfg.Submit("   "); err == nil || done {
			t.Fatalf("empty id must be a retryable error: done=%v err=%v", done, err)
		}
		if _, done, err := cfg.Submit("SANDBOX_ED25519_KEY_00000001_0000002"); err == nil || done {
			t.Fatalf("sandbox id must be rejected: done=%v err=%v", done, err)
		}
		if _, done, err := cfg.Submit("KEYID-OK"); err != nil || !done {
			t.Fatalf("a valid id must complete: done=%v err=%v", done, err)
		}
		return nil
	}
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("interactive setup must succeed after a valid retry: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-OK") {
		t.Fatalf("expected configured with the accepted id: %s", out)
	}
	s, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("default")
	if !s.Bound || s.APIKeyID == nil || *s.APIKeyID != "KEYID-OK" {
		t.Fatalf("keystore must reflect only the accepted id: %+v", s)
	}
}

// TestSetupJSONSkipsPrompt: --json output is machine-readable, so the prompt is
// suppressed even with a runner wired.
func TestSetupJSONSkipsPrompt(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	called := false
	stub := func(setupui.Config) error { called = true; return nil }
	out, _, code := runWithSetupUI([]string{"setup", "--json"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("setup --json must succeed: exit = %d", code)
	}
	if called {
		t.Fatalf("--json must not invoke the interactive prompt")
	}
	if !strings.Contains(out, `"status": "awaitingRegistration"`) {
		t.Fatalf("expected the JSON awaitingRegistration document: %s", out)
	}
}
