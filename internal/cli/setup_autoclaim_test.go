// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/cli"
	"github.com/digitalx-official/digitalx-cli/internal/cli/setupui"
	"github.com/digitalx-official/digitalx-cli/internal/keys"
)

// claimDoer routes the three endpoints auto-claim setup touches: the keyless
// GET /v2/keys/claim, the post-bind /v2/currentKeyInfo (wait-for-key + doctor),
// and /v2/time. claim/whoami are supplied per test as scripted closures; the
// doer also captures and verifies the claim request's signing scheme.
type claimDoer struct {
	claim  func(call int, r *http.Request) *http.Response
	whoami func(call int, r *http.Request) *http.Response

	claimCalls  int32
	whoamiCalls int32

	// Captured from the first /v2/keys/claim request.
	sawKAPIKeyHeader bool
	sigVerified      bool
	typeParam        string
	claimReqURL      string
	whoamiReqURL     string
}

func (d *claimDoer) Do(r *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(r.URL.Path, "/v2/keys/claim"):
		n := int(atomic.AddInt32(&d.claimCalls, 1))
		if n == 1 {
			d.captureClaimReq(r)
			d.claimReqURL = r.URL.String()
		}
		return d.claim(n, r), nil
	case strings.Contains(r.URL.Path, "currentKeyInfo"):
		n := int(atomic.AddInt32(&d.whoamiCalls, 1))
		if n == 1 {
			d.whoamiReqURL = r.URL.String()
		}
		return d.whoami(n, r), nil
	case strings.Contains(r.URL.Path, "/v2/time"):
		return resp(200, `{"success":true,"data":{"time":1700000000000}}`, nil), nil
	}
	return resp(404, `{"success":false}`, nil), nil
}

// captureClaimReq records whether the claim request leaked an X-KAPI-KEY header
// and whether its signature verifies against the documented scheme: sign over
// the query with the signature param removed, using the publicKey it carries.
func (d *claimDoer) captureClaimReq(r *http.Request) {
	d.sawKAPIKeyHeader = r.Header.Get("X-KAPI-KEY") != "" || r.Header.Get("x-kapi-key") != ""
	d.typeParam = r.URL.Query().Get("type")
	pubB64 := r.URL.Query().Get("publicKey")
	sig := r.URL.Query().Get("signature")
	der, err := base64.RawURLEncoding.DecodeString(pubB64)
	if err != nil {
		return
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return
	}
	d.sigVerified = ed25519.Verify(pub, []byte(removeSigParam(r.URL.RawQuery)), sigBytes)
}

// removeSigParam strips the signature parameter, preserving order — the message
// the server verifies. Mirrors middleware/auth.removeSignature.
func removeSigParam(rawQuery string) string {
	parts := strings.Split(rawQuery, "&")
	kept := parts[:0]
	for _, p := range parts {
		if !strings.HasPrefix(p, "signature=") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "&")
}

const (
	claimReadyBody     = `{"success":true,"data":{"apiKey":"KEYID-CLAIMED","type":"ed25519","label":"l","createdAtMs":1700000000000}}`
	claimPendingBody   = `{"success":false,"error":{"code":404,"message":"KEY_CLAIM_PENDING","description":"not yet"}}`
	claimConflictBody  = `{"success":false,"error":{"code":409,"message":"KEY_CLAIM_CONFLICT","description":"collision"}}`
	claimBadReqBody    = `{"success":false,"error":{"code":400,"message":"BAD_REQUEST","description":"bad"}}`
	whoamiActivatedRaw = `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readOrders","writeOrders"]}}`
	whoamiNotFoundBody = `{"success":false,"error":{"code":400,"message":"KEY_NOT_FOUND","description":"no such key"}}`
)

// driveAutoClaim returns a setup-UI stub that runs only the background auto-claim
// poll (no paste): it drains updates until a terminal one and records them.
func driveAutoClaim(got *[]setupui.ClaimUpdate) func(setupui.Config) error {
	return func(cfg setupui.Config) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		for u := range cfg.AutoClaim(ctx) {
			*got = append(*got, u)
			if u.Done || u.Stop {
				break
			}
		}
		return nil
	}
}

func boundID(t *testing.T, home, name string) (bound bool, id string) {
	t.Helper()
	s, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show(name)
	if err != nil {
		t.Fatalf("Show(%q): %v", name, err)
	}
	if s.APIKeyID == nil {
		return s.Bound, ""
	}
	return s.Bound, *s.APIKeyID
}

// TestAutoClaimPendingThenReadyBinds: the poll keeps going on KEY_CLAIM_PENDING,
// then binds the issued id once the key is registered — and the request it signs
// carries no X-KAPI-KEY and verifies under the documented scheme.
func TestAutoClaimPendingThenReadyBinds(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim: func(call int, _ *http.Request) *http.Response {
			if call < 3 {
				return resp(404, claimPendingBody, nil)
			}
			return resp(200, claimReadyBody, nil)
		},
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("auto-claim setup must succeed: exit=%d — %s", code, stderr)
	}
	last := got[len(got)-1]
	if !last.Done {
		t.Fatalf("expected a terminal Done update, got %+v", got)
	}
	if !strings.Contains(out, "is configured") || !strings.Contains(out, "KEYID-CLAIMED") {
		t.Fatalf("expected the configured result on stdout: %s", out)
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
	if doer.typeParam != "ed25519" {
		t.Fatalf("expected type=ed25519 param, got %q", doer.typeParam)
	}
}

// TestAutoClaimUsesConfiguredBaseURL: the claim poll and the post-bind
// wait/health-check must hit the configured base URL (DIGITALX_CLI_BASE_URL here),
// not the prod default — same resolution the advisory doctor uses.
func TestAutoClaimUsesConfiguredBaseURL(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const base = "https://alt.example.test"
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home, "DIGITALX_CLI_BASE_URL": base},
		doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.HasPrefix(doer.claimReqURL, base+"/v2/keys/claim") {
		t.Fatalf("claim must hit the configured base URL, got %q", doer.claimReqURL)
	}
	if !strings.HasPrefix(doer.whoamiReqURL, base+"/v2/currentKeyInfo") {
		t.Fatalf("wait/health-check must hit the configured base URL, got %q", doer.whoamiReqURL)
	}
}

// TestAutoClaimUsesKeyPinnedBaseURL: with no flag/env override, the claim poll
// and the post-bind whoami must hit the base URL pinned on the key (e.g. set via
// `key set-base-url`), not the prod default — the per-key tier doctor also uses.
func TestAutoClaimUsesKeyPinnedBaseURL(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const base = "https://pinned.example.test"
	km := keys.NewManager(home, "file", func() int64 { return 1 }, nil)
	if err := km.SetBaseURL("default", base, ""); err != nil {
		t.Fatalf("SetBaseURL: %v", err)
	}
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, // no DIGITALX_CLI_BASE_URL, no --base-url
		doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.HasPrefix(doer.claimReqURL, base+"/v2/keys/claim") {
		t.Fatalf("claim must hit the key-pinned base URL, got %q", doer.claimReqURL)
	}
	if !strings.HasPrefix(doer.whoamiReqURL, base+"/v2/currentKeyInfo") {
		t.Fatalf("whoami must hit the key-pinned base URL, got %q", doer.whoamiReqURL)
	}
}

// TestAutoClaimConflictFallsBackToPaste: a KEY_CLAIM_CONFLICT stops the poll with
// a notice, leaves the key unbound, and the manual paste path still works.
func TestAutoClaimConflictFallsBackToPaste(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(409, claimConflictBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	var pasteDone bool
	stub := func(cfg setupui.Config) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		for u := range cfg.AutoClaim(ctx) {
			got = append(got, u)
			if u.Stop {
				break
			}
			if u.Done {
				t.Fatal("conflict must not complete the claim")
			}
		}
		// Manual paste must still work after the poll stops.
		_, done, err := cfg.Submit("KEYID-PASTED")
		if err != nil || !done {
			t.Fatalf("manual paste after conflict must bind: done=%v err=%v", done, err)
		}
		pasteDone = true
		return nil
	}
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if len(got) == 0 || !got[len(got)-1].Stop {
		t.Fatalf("expected a terminal Stop update, got %+v", got)
	}
	if !strings.Contains(got[len(got)-1].Status, "Couldn't auto-detect your key") {
		t.Fatalf("expected a conflict fall-back notice: %q", got[len(got)-1].Status)
	}
	if !pasteDone {
		t.Fatal("manual paste fallback did not run")
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-PASTED" {
		t.Fatalf("expected the pasted id bound after fallback: bound=%v id=%q", bound, id)
	}
	if !strings.Contains(out, "KEYID-PASTED") {
		t.Fatalf("expected the pasted id in the result: %s", out)
	}
}

// TestAutoClaimNonTransientErrorStops: a non-transient request error (BAD_REQUEST)
// stops the poll with a notice naming the code and leaves the key unbound.
func TestAutoClaimNonTransientErrorStops(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(400, claimBadReqBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	last := got[len(got)-1]
	if !last.Stop || !strings.Contains(last.Status, "BAD_REQUEST") {
		t.Fatalf("expected a Stop naming BAD_REQUEST, got %+v", got)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a hard error must leave the key unbound")
	}
}

// TestAutoClaimIPNotAllowedStops: the server now gates the claim on the key's IP
// allowlist; an IP_NOT_ALLOWED stop guides the user with this machine's PROBED
// public IP(s) — like doctor — not the server text, and leaves the key unbound.
func TestAutoClaimIPNotAllowedStops(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const ipBody = `{"success":false,"error":{"code":403,"message":"IP_NOT_ALLOWED","description":"ip address not in whitelist"}}`
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(403, ipBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	last := got[len(got)-1]
	if !last.Stop {
		t.Fatalf("expected a Stop, got %+v", got)
	}
	// The notice names the probed IP and points at the allowlist (doctor-style).
	if !strings.Contains(last.Status, "203.0.113.7") || !strings.Contains(last.Status, "allowlist") {
		t.Fatalf("expected the probed IP + allowlist guidance in the notice: %q", last.Status)
	}
	if last.Prefill != "" {
		t.Fatalf("a claim-gate stop has no id to prefill, got %q", last.Prefill)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("an IP-blocked claim must leave the key unbound")
	}
}

// TestAutoClaimDeactivatedStops: a KEY_DEACTIVATED (the user deleted the key)
// stops with a clear error and falls back to manual; the key stays unbound.
func TestAutoClaimDeactivatedStops(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const deBody = `{"success":false,"error":{"code":401,"message":"KEY_DEACTIVATED","description":"키가 삭제되었습니다."}}`
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(401, deBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	last := got[len(got)-1]
	if !last.Stop || !strings.Contains(last.Status, "deactivated") {
		t.Fatalf("expected a Stop naming deactivation, got %+v", got)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a deactivated key must not be bound")
	}
}

// TestAutoClaimPrefillsIdWhenKeyGoesBadAfterClaim: if the claim returns the id
// (200) but the post-claim whoami then finds the key deactivated (the user
// deactivated it in the window), auto-bind aborts and the retrieved id is offered
// as a prefill so the user can finish manually without re-finding it.
func TestAutoClaimPrefillsIdWhenKeyGoesBadAfterClaim(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	const deBody = `{"success":false,"error":{"code":401,"message":"KEY_DEACTIVATED","description":"키가 삭제되었습니다."}}`
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(401, deBody, nil) }, // deactivated post-claim
	}
	var got []setupui.ClaimUpdate
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	last := got[len(got)-1]
	if !last.Stop || !strings.Contains(last.Status, "deactivated") {
		t.Fatalf("expected a Stop naming deactivation, got %+v", got)
	}
	if last.Prefill != "KEYID-CLAIMED" {
		t.Fatalf("expected the retrieved id offered as prefill, got %q", last.Prefill)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a deactivated key must not be bound even when its id was retrieved")
	}
}

// TestAutoClaimExitDuringDoctorReportsConfigured: once the auto-claim lane has
// bound the key, exiting the dialog ("finish later") while the complementary
// health check is still running must report the key CONFIGURED — never the
// resume/awaiting-registration document — because the keystore is already bound.
// This is the bind-then-doctor-then-exit window: final is published atomically
// with the bind (before doctor), so the caller's post-UI read always sees it.
func TestAutoClaimExitDuringDoctorReportsConfigured(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doctorStarted := make(chan struct{})
	releaseDoctor := make(chan struct{})
	var doctorSignaled int32
	doer := &claimDoer{
		claim: func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(call int, _ *http.Request) *http.Response {
			// Call 1 is the pre-bind verify; call 2+ is the post-bind doctor. Block the
			// doctor so the dialog can exit while the bind is already persisted.
			if call >= 2 {
				if atomic.CompareAndSwapInt32(&doctorSignaled, 0, 1) {
					close(doctorStarted)
				}
				<-releaseDoctor
			}
			return resp(200, whoamiActivatedRaw, nil)
		},
	}
	stub := func(cfg setupui.Config) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := cfg.AutoClaim(ctx)
		go func() { // drain so the goroutine's status/Done sends never block
			for range ch {
			}
		}()
		select {
		case <-doctorStarted: // the bind has landed; doctor is mid-flight
		case <-time.After(3 * time.Second):
			t.Error("doctor never started — bind did not happen")
		}
		close(releaseDoctor)
		return nil // ≈ Esc "finish later" while the health check runs
	}
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !strings.Contains(out, "is configured") {
		t.Fatalf("a bound key must report configured, not the resume document: %s", out)
	}
	if strings.Contains(out, "awaitingRegistration") || strings.Contains(out, "isn't registered") {
		t.Fatalf("must not emit the resume/awaiting document over a bound key: %s", out)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("key must be bound after the bind landed: bound=%v id=%q", bound, id)
	}
}

// TestAutoClaim429BacksOffThenBinds: an HTTP 429 backs off (without stopping) and
// the poll continues to a successful bind.
func TestAutoClaim429BacksOffThenBinds(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim: func(call int, _ *http.Request) *http.Response {
			if call == 1 {
				return resp(429, `{"success":false,"error":{"code":429,"message":"TOO_MANY_REQUESTS"}}`, nil)
			}
			return resp(200, claimReadyBody, nil)
		},
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	var got []setupui.ClaimUpdate
	_, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !got[len(got)-1].Done {
		t.Fatalf("expected Done after the back-off, got %+v", got)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("expected bind after back-off: bound=%v id=%q", bound, id)
	}
}

// TestAutoClaimWaitsForKeyBeforeDoctor: after the claim, the pre-bind verify
// (wait=true) rides out KEY_NOT_FOUND until the key is active, then binds and runs
// the health check. The bind succeeds regardless.
func TestAutoClaimWaitsForKeyBeforeDoctor(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 2, 2*time.Second)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim: func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(call int, _ *http.Request) *http.Response {
			if call < 3 {
				return resp(400, whoamiNotFoundBody, nil)
			}
			return resp(200, whoamiActivatedRaw, nil)
		},
	}
	var got []setupui.ClaimUpdate
	out, stderr, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), driveAutoClaim(&got))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, stderr)
	}
	if !got[len(got)-1].Done {
		t.Fatalf("expected Done, got %+v", got)
	}
	if atomic.LoadInt32(&doer.whoamiCalls) < 2 {
		t.Fatalf("expected the wait to retry KEY_NOT_FOUND, got %d whoami call(s)", doer.whoamiCalls)
	}
	if bound, id := boundID(t, home, "default"); !bound || id != "KEYID-CLAIMED" {
		t.Fatalf("expected bind despite the wait: bound=%v id=%q", bound, id)
	}
	if !strings.Contains(out, "is configured") {
		t.Fatalf("expected configured result: %s", out)
	}
}

// TestAutoClaimWaitForKeyHonorsCancel: the pre-bind verify wait must abort
// promptly when the session is canceled (Ctrl-C/Esc) instead of blocking out the
// full budget, AND cancellation is terminal — the poll must NOT bind behind the
// user's back once the dialog has closed (or the keystore would say "bound" while
// the caller emits the "finish later" resume document). The whoami never reports
// the key active and the budget is set long (30s); a cancel must still close the
// channel within a few seconds and leave the key unbound.
func TestAutoClaimWaitForKeyHonorsCancel(t *testing.T) {
	defer cli.SetClaimTimingForTest(5, 5, 50, 30*time.Second)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(200, claimReadyBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(400, whoamiNotFoundBody, nil) }, // never active
	}
	stub := func(cfg setupui.Config) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := cfg.AutoClaim(ctx)
		// "Waiting for key…" is sent right before the verify wait begins (before any
		// bind) — its arrival proves the poll has reached the wait.
		select {
		case u := <-ch:
			if !strings.Contains(u.Status, "Waiting for key") {
				t.Fatalf("expected the waiting-for-key status first, got %+v", u)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out before the wait phase began")
		}
		cancel() // ≈ Ctrl-C/Esc during the wait
		// The wait must honor the cancel and close the channel well under the 30s
		// budget — proof it isn't blocking on the (untethered) budget.
		closed := make(chan struct{})
		go func() {
			for range ch {
			}
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("wait-for-key ignored cancellation (still running after 5s, budget 30s)")
		}
		return setupui.ErrInterrupted
	}
	_, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("a canceled session must exit 0: %d", code)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a session canceled during the wait must NOT bind behind the user's back")
	}
}

// TestAutoClaimCancelIsClean: canceling the session context while the poll is
// pending stops it promptly with no terminal update and no bind.
func TestAutoClaimCancelIsClean(t *testing.T) {
	defer cli.SetClaimTimingForTest(50, 50, 2, 200*time.Millisecond)()
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	doer := &claimDoer{
		claim:  func(int, *http.Request) *http.Response { return resp(404, claimPendingBody, nil) },
		whoami: func(int, *http.Request) *http.Response { return resp(200, whoamiActivatedRaw, nil) },
	}
	terminal := make(chan setupui.ClaimUpdate, 4)
	stub := func(cfg setupui.Config) error {
		ctx, cancel := context.WithCancel(context.Background())
		ch := cfg.AutoClaim(ctx)
		// Let the poll observe at least one pending response, then cancel.
		select {
		case u := <-ch:
			terminal <- u
		case <-time.After(2 * time.Second):
		}
		cancel()
		// Drain to completion; the channel must close without a terminal update.
		for u := range ch {
			terminal <- u
		}
		return setupui.ErrInterrupted // a cancel maps to the Ctrl-C abort
	}
	out, _, code := runWithSetupUI([]string{"setup"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("a canceled session must exit 0: %d", code)
	}
	close(terminal)
	for u := range terminal {
		if u.Done || u.Stop {
			t.Fatalf("cancel must not yield a terminal update, got %+v", u)
		}
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a Ctrl-C abort must emit nothing, got: %q", out)
	}
	if bound, _ := boundID(t, home, "default"); bound {
		t.Fatal("a canceled poll must not bind")
	}
}
