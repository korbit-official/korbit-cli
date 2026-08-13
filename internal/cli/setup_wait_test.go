// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/cli/setupui"
	"github.com/korbit-official/korbit-cli/internal/keys"
)

// The `setup --wait` headless auto-claim path reuses the same claim/whoami doer
// and bodies as the interactive auto-claim tests (setup_autoclaim_test.go); these
// exercise it WITHOUT an injected setup UI (runWithDeps has no TTY and no
// SetupUIRun), proving --wait drives setup to completion with no prompt and no
// terminal — the agent flow.

// TestSetupWaitClaimsAndBindsHeadless: a fresh `setup --wait` generates the key,
// polls in the background, and binds + health-checks the issued id automatically
// once it appears — no prompt, no TTY, the user never pastes the id back. The
// claim request must carry no X-KAPI-KEY and verify under the documented scheme.
func TestSetupWaitClaimsAndBindsHeadless(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	doer := &claimDoer{
		claim: func(call int, _ *http.Request) *http.Response {
			if call < 3 {
				return resp(404, claimPendingBody, nil)
			}
			return resp(200, claimReadyBody, nil)
		},
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--wait"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("headless --wait setup must succeed: exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-CLAIMED") {
		t.Fatalf("expected the configured result on stdout: %s", out)
	}
	// The headless path has no live prompt, so it renders the health check as the
	// complementary-doctor section, set off from the headline by a blank line.
	if !strings.Contains(out, "\n\nComplementary health check (doctor)") {
		t.Fatalf("expected the complementary health check separated by a blank line: %s", out)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("key not auto-bound to the claimed id: bound=%v id=%q", bound, id)
	}
	if doer.sawKAPIKeyHeader {
		t.Fatal("the claim request must NOT send an X-KAPI-KEY header")
	}
	if !doer.sigVerified {
		t.Fatal("the claim request signature must verify under the documented scheme")
	}
}

// TestSetupWaitStopFallsBackToResume: a claim conflict stops the headless poll;
// setup prints the reason and emits the resume document (exit 0, key left unbound)
// so the user can finish later with --api-key.
func TestSetupWaitStopFallsBackToResume(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(409, claimConflictBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--wait"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a stop must still exit 0 (resumable): exit=%d — %s", code, stderr)
	}
	if !strings.Contains(stderr, "Couldn't auto-detect your key") {
		t.Fatalf("expected a conflict fall-back notice on stderr: %s", stderr)
	}
	if !strings.Contains(out, "isn't registered") {
		t.Fatalf("expected the resume document on stdout: %s", out)
	}
	// The registration guidance (with the link) is the awaiting document, emitted on
	// stdout exactly ONCE — up front, before the poll — and never re-emitted by the
	// fallback (a stop only adds the reason on stderr).
	if n := strings.Count(out, "manage/create"); n != 1 {
		t.Fatalf("registration guidance must be on stdout exactly once, got %d:\n%s", n, out)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a conflict stop must leave the key unbound")
	}
}

// TestSetupWaitTimesOutResumes: when the key never registers within --wait-timeout,
// setup gives up, prints how to resume, and emits the resume document (exit 0,
// unbound).
func TestSetupWaitTimesOutResumes(t *testing.T) {
	defer cli.SetClaimTimingForTest(20, 20, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(404, claimPendingBody, nil) }, // never ready
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--wait", "--wait-timeout", "1s"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a timeout must exit 0 (resumable): exit=%d — %s", code, stderr)
	}
	if !strings.Contains(stderr, "Still not registered") {
		t.Fatalf("expected the timeout resume notice on stderr: %s", stderr)
	}
	if !strings.Contains(out, "isn't registered") {
		t.Fatalf("expected the resume document on stdout: %s", out)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a timed-out wait must leave the key unbound")
	}
}

// TestSetupWaitHardRejectResumes: the claim returns the id (200) but the post-claim
// whoami finds the key deactivated — a hard rejection. The headless path must NOT
// bind; it reports the reason and resumes.
func TestSetupWaitHardRejectResumes(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const deBody = `{"success":false,"error":{"code":401,"message":"KEY_DEACTIVATED","description":"키가 삭제되었습니다."}}`
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(401, deBody, nil) },
	}
	_, stderr, code := runWithDeps([]string{"setup", "--wait"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a hard reject must still exit 0 (resumable): exit=%d — %s", code, stderr)
	}
	if !strings.Contains(stderr, "deactivated") {
		t.Fatalf("expected the deactivation notice on stderr: %s", stderr)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a deactivated key must not be bound even when its id was retrieved")
	}
}

// TestSetupWaitOverridesInteractiveUIOnTTY: --wait selects the headless poll even
// when an interactive UI is wired (a TTY) — the prompt must NOT run; setup binds
// headless. This is the matrix row "--wait on a TTY → headless".
func TestSetupWaitOverridesInteractiveUIOnTTY(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	// runWithSetupUI injects a SetupUIRun, which normally FORCES the interactive
	// prompt on (bypassing the TTY gate). --wait must still override it: this stub
	// fails the test if the prompt is ever invoked.
	uiCalled := false
	stub := func(setupui.Config) error {
		uiCalled = true
		t.Error("the interactive prompt must not run when --wait is set")
		return nil
	}
	out, stderr, code := runWithSetupUI([]string{"setup", "--wait"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if uiCalled {
		t.Fatal("--wait did not override the interactive UI")
	}
	if !strings.Contains(out, "is configured") {
		t.Fatalf("expected headless bind result: %s", out)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("expected headless bind: bound=%v id=%q", bound, id)
	}
}

// TestSetupWaitWithNoInteractiveStillWaits: --no-interactive is redundant with
// --wait (which is already headless), not a conflict — the poll still runs and
// binds.
func TestSetupWaitWithNoInteractiveStillWaits(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--wait", "--no-interactive"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("--wait --no-interactive must work, not error: exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, "is configured") {
		t.Fatalf("expected the configured result: %s", out)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("expected bind: bound=%v id=%q", bound, id)
	}
}

// TestSetupWaitIsNoOpOnBoundKey: --wait on an already-configured (bound) key is a
// no-op — setup reports already-configured and never polls (the claim endpoint is
// not touched), matching --wait's "no effect on a finished key" rule.
func TestSetupWaitIsNoOpOnBoundKey(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedBoundKey(t, home) // creates a bound key "bot"
	doer := &claimDoer{
		claim: func(int, *http.Request) *http.Response {
			t.Error("a bound key must not trigger the claim poll")
			return resp(200, claimReadyBody, nil)
		},
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--name", "bot", "--wait"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, "already configured") {
		t.Fatalf("expected already-configured report, got: %s", out)
	}
	if n := atomic.LoadInt32(&doer.claimCalls); n != 0 {
		t.Fatalf("a bound key must not poll the claim endpoint, got %d call(s)", n)
	}
}

// TestSetupWaitIsNoOpWithInlineCredential: an inline credential
// (KORBIT_CLI_API_KEY_*) is a "finished" state — there is no stored key to claim,
// so --wait is a silent no-op: setup reports the env credential and never polls.
func TestSetupWaitIsNoOpWithInlineCredential(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	doer := &claimDoer{
		claim: func(int, *http.Request) *http.Response {
			t.Error("an inline credential must not trigger the claim poll")
			return resp(200, claimReadyBody, nil)
		},
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--wait"},
		map[string]string{
			"KORBIT_CLI_HOME":           home,
			"KORBIT_CLI_API_KEY_ID":     "KEYID-ENV",
			"KORBIT_CLI_API_KEY_SECRET": "secret",
			"KORBIT_CLI_API_KEY_TYPE":   "hmac-sha256",
		}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, "configured via the environment") {
		t.Fatalf("expected the inline-credential report, got: %s", out)
	}
	if n := atomic.LoadInt32(&doer.claimCalls); n != 0 {
		t.Fatalf("an inline credential must not poll the claim endpoint, got %d call(s)", n)
	}
}

// TestSetupPinsKeyToEnvBaseURL: a key created while KORBIT_CLI_BASE_URL is set is
// pinned to that host (like --base-url), so later commands reach it without the env
// var. (Applies to setup generally, exercised here via the non-interactive create.)
func TestSetupPinsKeyToEnvBaseURL(t *testing.T) {
	home := t.TempDir()
	const base = "https://api-uat.example.test"
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(404, claimPendingBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	_, stderr, code := runWithDeps([]string{"setup", "--name", "uatkey", "--no-interactive"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_BASE_URL": base},
		doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	km := keys.NewManager(home, "file", func() int64 { return 1 }, nil)
	if got := km.MetaBaseURL("uatkey"); got != base {
		t.Fatalf("key not pinned to the env base URL: got %q, want %q", got, base)
	}
}

// TestKeyAddPinsKeyToEnvBaseURL: `key add` also pins a newly created key to the
// KORBIT_CLI_BASE_URL env override (not just setup), so the env-host workflow is
// consistent across both create commands.
func TestKeyAddPinsKeyToEnvBaseURL(t *testing.T) {
	home := t.TempDir()
	const base = "https://api-uat.example.test"
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(404, claimPendingBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	_, stderr, code := runWithDeps([]string{"key", "add", "addedkey"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_BASE_URL": base},
		doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	km := keys.NewManager(home, "file", func() int64 { return 1 }, nil)
	if got := km.MetaBaseURL("addedkey"); got != base {
		t.Fatalf("key add did not pin the env base URL: got %q, want %q", got, base)
	}
}

// TestSetupPortalBaseURLOverride: KORBIT_CLI_PORTAL_BASE_URL redirects the
// registration link/guidance to an internal portal host (undocumented, internal
// testing) instead of the production developers portal.
func TestSetupPortalBaseURLOverride(t *testing.T) {
	home := t.TempDir()
	const portal = "https://dev-portal.internal.test"
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(404, claimPendingBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	out, stderr, code := runWithDeps([]string{"setup", "--no-interactive"},
		map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_PORTAL_BASE_URL": portal},
		doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, portal+"/manage/create") {
		t.Fatalf("registration link must point at the overridden portal host:\n%s", out)
	}
	if strings.Contains(out, "developers.korbit.co.kr") || strings.Contains(stderr, "developers.korbit.co.kr") {
		t.Fatalf("override must replace the production portal host entirely:\nstdout=%s\nstderr=%s", out, stderr)
	}
}

// TestSetupWaitConflictsWithApiKey: --wait and --api-key supply the issued id two
// contradictory ways — the combination is a usage error before any work.
func TestSetupWaitConflictsWithApiKey(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	_, stderr, code := runWithDeps([]string{"setup", "--wait", "--api-key", "KEYID-X"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 2 {
		t.Fatalf("--wait + --api-key must be a usage error (exit 2), got %d", code)
	}
	if !strings.Contains(stderr, "--wait can't be combined with --api-key") {
		t.Fatalf("expected the conflict message, got: %s", stderr)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a rejected invocation must not bind anything")
	}
}

// TestSetupWaitTimeoutRequiresWait: --wait-timeout without --wait is meaningless
// and rejected, so it can't silently do nothing.
func TestSetupWaitTimeoutRequiresWait(t *testing.T) {
	home := t.TempDir()
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	_, stderr, code := runWithDeps([]string{"setup", "--wait-timeout", "5m"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 2 {
		t.Fatalf("--wait-timeout without --wait must be a usage error (exit 2), got %d", code)
	}
	if !strings.Contains(stderr, "--wait-timeout has no effect without --wait") {
		t.Fatalf("expected the orphan-flag message, got: %s", stderr)
	}
}
