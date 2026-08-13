// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/keystore"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/stream"
)

// runWithDeps runs the CLI with both a stub Doer and a stub IP prober — doctor
// needs both (live whoami/time over the Doer, allowlist over the prober).
func runWithDeps(args []string, env map[string]string, doer korbit.Doer,
	probe func(context.Context, string, string, string, int) (string, error)) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv: func(k string) string { return env[k] },
		Stdout: &out,
		Stderr: &errb,
		Doer:   doer,
		Now:    func() int64 { return 1700000000000 },
		// The allowlist diagnosis replays the whoami over each family; reuse the
		// injected doer so tests stay off the real network (same outcome per call).
		FamilyDoer: func(string, int) korbit.Doer { return doer },
		IPProbe:    probe,
		// Doctor's WebSocket reachability check dials this; a stub keeps the suite
		// hermetic (an unset WSDial would default to the real network dialer).
		WSDial: dialFrames(),
	})
	return out.String(), errb.String(), code
}

// runWithClock is runWithDeps with an injectable clock, for tests that need
// Now() to advance between calls (e.g. the clock-skew midpoint).
func runWithClock(args []string, env map[string]string, doer korbit.Doer,
	probe func(context.Context, string, string, string, int) (string, error), now func() int64) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:     func(k string) string { return env[k] },
		Stdout:     &out,
		Stderr:     &errb,
		Doer:       doer,
		Now:        now,
		FamilyDoer: func(string, int) korbit.Doer { return doer },
		IPProbe:    probe,
		WSDial:     dialFrames(), // hermetic WS dial for doctor's reachability check
	})
	return out.String(), errb.String(), code
}

// routeDoer answers per request path: currentKeyInfo and /v2/time get their own
// canned bodies; a whoami transport error can be injected. Fresh responses are
// built per call so bodies aren't consumed across the two requests.
type routeDoer struct {
	whoamiStatus int
	whoamiBody   string
	whoamiErr    error
	timeBody     string
}

func (d routeDoer) Do(r *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(r.URL.Path, "currentKeyInfo"):
		if d.whoamiErr != nil {
			return nil, d.whoamiErr
		}
		return resp(d.whoamiStatus, d.whoamiBody, nil), nil
	case strings.Contains(r.URL.Path, "/v2/time"):
		return resp(200, d.timeBody, nil), nil
	}
	return resp(404, `{"success":false}`, nil), nil
}

// capturingDoer records the query string of the signed whoami request so tests
// can assert what was actually sent (e.g. recvWindow).
type capturingDoer struct {
	whoamiBody  string
	timeBody    string
	whoamiQuery string
}

func (d *capturingDoer) Do(r *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(r.URL.Path, "currentKeyInfo"):
		d.whoamiQuery = r.URL.RawQuery
		return resp(200, d.whoamiBody, nil), nil
	case strings.Contains(r.URL.Path, "/v2/time"):
		return resp(200, d.timeBody, nil), nil
	}
	return resp(404, `{"success":false}`, nil), nil
}

// runWithFamilyDeps runs doctor with a per-family stub Doer for the allowlist
// diagnosis (in addition to the default-connection doer used for the first
// whoami and /v2/time). family maps "tcp4"/"tcp6" to the Doer that family's
// replay should see.
func runWithFamilyDeps(args []string, env map[string]string, doer korbit.Doer,
	family map[string]korbit.Doer, probe func(context.Context, string, string, string, int) (string, error)) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:     func(k string) string { return env[k] },
		Stdout:     &out,
		Stderr:     &errb,
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		FamilyDoer: func(network string, _ int) korbit.Doer { return family[network] },
		IPProbe:    probe,
	})
	return out.String(), errb.String(), code
}

func seedUnboundKey(t *testing.T, home, name string) {
	t.Helper()
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	if _, err := m.Add(name, "", ""); err != nil {
		t.Fatal(err)
	}
}

const okTime = `{"success":true,"data":{"time":1700000000100}}` // ~100ms skew

func TestDoctorNoKeysFails(t *testing.T) {
	home := t.TempDir()
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, routeDoer{timeBody: okTime}, fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if !strings.Contains(out, `"ok":false`) || !strings.Contains(out, "no keys configured") {
		t.Fatalf("expected a failing 'no keys' report: %s", out)
	}
	// The key-independent environment checks still run without a key configured:
	// clock skew, public IP, and WebSocket reachability need no credential.
	for _, want := range []string{`"name":"clock skew"`, `"name":"public IP"`, `"name":"websocket"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in the keyless report: %s", want, out)
		}
	}
	// A blocking config fault must not read as healthy just because the env checks
	// are OK — the "no keys" fail keeps the verdict failing.
	if !strings.Contains(out, `"name":"public IP","status":"ok"`) {
		t.Fatalf("expected an OK public IP check even without a key: %s", out)
	}
}

// TestDoctorUnboundKeyRunsEnvChecks: an unbound key blocks the live signed
// checks (exit 4), but the key-independent clock-skew/public-IP/WebSocket checks
// still run.
func TestDoctorUnboundKeyRunsEnvChecks(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "k1")
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, routeDoer{timeBody: okTime}, fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "no API key id bound") {
		t.Fatalf("expected an unbound-key failure: %s", out)
	}
	for _, want := range []string{`"name":"clock skew"`, `"name":"public IP"`, `"name":"websocket"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in the unbound-key report: %s", want, out)
		}
	}
}

func TestDoctorHealthy(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","readOrders","writeOrders"],"expiration":1900000000000}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — %s", code, out)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("expected ok report: %s", out)
	}
	if !strings.Contains(out, "key is live") {
		t.Fatalf("expected a live whoami check: %s", out)
	}
}

func TestDoctorMissingWriteOrdersWarnsButPasses(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","readOrders"]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a warning must not fail doctor: exit = %d — %s", code, out)
	}
	if !strings.Contains(out, "cannot place orders") {
		t.Fatalf("expected a writeOrders warning: %s", out)
	}
}

func TestDoctorIPBlockedFails(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "IP_NOT_ALLOWED") || !strings.Contains(out, "allowlist") {
		t.Fatalf("expected an IP-allowlist failure with a fix: %s", out)
	}
}

// TestDoctorIPAllowlistOneFamilyAccepted: when the default connection is
// rejected for an IP-allowlist reason but forcing one family succeeds, doctor
// names which family works, surfaces the configured whitelist (from the
// successful currentKeyInfo), and tells the user the exact entry to add for the
// rejected family (or to pin the bot to the working one).
func TestDoctorIPAllowlistOneFamilyAccepted(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	ipErr := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
	}
	v4OK := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["writeOrders"],"whitelist":"1.2.3.4,5.6.7.8"}}`,
	}
	defaultDoer := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithFamilyDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, defaultDoer,
		map[string]korbit.Doer{"tcp4": v4OK, "tcp6": ipErr},
		fakeProbe("203.0.113.7", "2001:db8::1"))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "ip allowlist") || !strings.Contains(out, "allowlisted over IPv4") {
		t.Fatalf("expected an ip-allowlist diagnosis naming the working family: %s", out)
	}
	if !strings.Contains(out, "configured allowlist: 1.2.3.4,5.6.7.8") {
		t.Fatalf("expected the configured whitelist to be surfaced: %s", out)
	}
	if !strings.Contains(out, "2001:db8::/64") || !strings.Contains(out, "IPv4 only") {
		t.Fatalf("expected the fix to name the IPv6 entry to add or to pin IPv4: %s", out)
	}
}

// TestDoctorHonorsAndReportsFamily: under --family ipv6, doctor must not probe
// IPv4 (no leak), must report the active restriction, and must advise dropping
// --family for a full diagnosis.
func TestDoctorHonorsAndReportsFamily(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	ok := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["writeOrders"]}}`,
		timeBody:     okTime,
	}
	seen := map[string]bool{}
	probe := func(_ context.Context, network, _, _ string, _ int) (string, error) {
		seen[network] = true // capped to one family here, so a single prober goroutine writes
		if network == "tcp4" {
			return "203.0.113.7", nil
		}
		return "2001:db8::1", nil
	}
	out, _, code := runWithFamilyDeps([]string{"doctor", "--family", "ipv6", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, ok,
		map[string]korbit.Doer{"tcp4": ok, "tcp6": ok}, probe)
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, out)
	}
	if seen["tcp4"] {
		t.Fatal("doctor --family ipv6 must not probe IPv4")
	}
	if !strings.Contains(out, "outbound restricted to IPv6") {
		t.Fatalf("expected the restriction warning: %s", out)
	}
	if !strings.Contains(out, "run without --family or --bind") {
		t.Fatalf("expected advice to lift the family restriction: %s", out)
	}
	if strings.Contains(out, "203.0.113.7") {
		t.Fatalf("--family ipv6 must not surface a v4 address: %s", out)
	}
}

// TestDoctorIPAllowlistNeitherFamilyAccepted: when neither family is
// allowlisted, doctor points the user to the portal with the IP entries to add.
func TestDoctorIPAllowlistNeitherFamilyAccepted(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	ipErr := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
	}
	defaultDoer := ipErr
	defaultDoer.timeBody = okTime
	out, _, code := runWithFamilyDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, defaultDoer,
		map[string]korbit.Doer{"tcp4": ipErr, "tcp6": ipErr},
		fakeProbe("203.0.113.7", "2001:db8::1"))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "your IPv4/IPv6 connection is not allowlisted") {
		t.Fatalf("expected a neither-family-allowlisted diagnosis: %s", out)
	}
	if !strings.Contains(out, "developers.korbit.co.kr") || !strings.Contains(out, "203.0.113.7") || !strings.Contains(out, "2001:db8::/64") {
		t.Fatalf("expected portal guidance with the IP entries to add: %s", out)
	}
}

// TestDoctorIPAllowlistBothFamiliesAccepted: the default connection was rejected
// for an allowlist reason, but forcing each family individually now succeeds —
// the rejection didn't reproduce, so the diagnosis warns rather than prescribing
// an allowlist edit. The failing default whoami still sets exit 4.
func TestDoctorIPAllowlistBothFamiliesAccepted(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	ok := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["writeOrders"],"whitelist":"1.2.3.4"}}`,
	}
	defaultDoer := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithFamilyDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, defaultDoer,
		map[string]korbit.Doer{"tcp4": ok, "tcp6": ok},
		fakeProbe("203.0.113.7", "2001:db8::1"))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "accepted over IPv4/IPv6") || !strings.Contains(out, "re-run") {
		t.Fatalf("expected a transient-rejection warning suggesting a re-run: %s", out)
	}
}

// TestDoctorIPAllowlistFamilyReplayNetworkError: when neither family replay can
// reach the API (transport error, e.g. a black-holed route), the diagnosis can't
// isolate the family and warns instead of mis-claiming the allowlist state.
func TestDoctorIPAllowlistFamilyReplayNetworkError(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	defaultDoer := routeDoer{
		whoamiStatus: 400,
		whoamiBody:   `{"success":false,"error":{"code":400,"message":"IP_NOT_ALLOWED","description":"blocked"}}`,
		timeBody:     okTime,
	}
	dead := routeDoer{whoamiErr: errors.New("dial tcp: no route to host")}
	out, _, code := runWithFamilyDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, defaultDoer,
		map[string]korbit.Doer{"tcp4": dead, "tcp6": dead},
		fakeProbe("203.0.113.7", "2001:db8::1"))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "could not replay the signed request") {
		t.Fatalf("expected a could-not-isolate warning: %s", out)
	}
}

func TestDoctorNetworkOnlyIsExit1(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{whoamiErr: errors.New("dial tcp: no route"), timeBody: okTime}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (network only) — %s", code, out)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("a network-only failure should not flip ok=false: %s", out)
	}
	// The "(network error)" summary carries the raw transport error as separate
	// context (unified with the WS probe).
	if !strings.Contains(out, "(network error)") {
		t.Fatalf("a network-only whoami failure should carry the (network error) tag: %s", out)
	}
	if !strings.Contains(out, `"context":"`) || !strings.Contains(out, "dial tcp: no route") {
		t.Fatalf("the raw transport error should ride as context: %s", out)
	}
}

func TestDoctorUnboundKeyFails(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "bot")
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, routeDoer{}, fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 — %s", code, out)
	}
	if !strings.Contains(out, "no API key id bound") {
		t.Fatalf("expected a binding failure: %s", out)
	}
}

// TestDoctorClockSkewUsesRequestMidpoint pins that skew is measured against the
// midpoint of the two local clock readings that bracket the /v2/time request, so
// round-trip latency isn't charged to the clock. The injected clock advances
// 1000ms per call; in a healthy run the calls before the bracket are: whoami
// timestamp (t=…000), then t0 (…1000), then t1 (…2000) → midpoint …1500. Setting
// the server time exactly to that midpoint must report ~0ms; a t0- or t1-based
// (or un-halved) calculation would not.
func TestDoctorClockSkewUsesRequestMidpoint(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	var n int64
	clock := func() int64 { v := int64(1700000000000) + n*1000; n++; return v }
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["writeOrders"]}}`,
		timeBody:     `{"success":true,"data":{"time":1700000001500}}`, // = (t0+t1)/2
	}
	out, _, code := runWithClock([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""), clock)
	if code != 0 {
		t.Fatalf("exit = %d — %s", code, out)
	}
	if !strings.Contains(out, "within ~0ms") {
		t.Fatalf("midpoint skew should be ~0ms (latency discounted); got: %s", out)
	}
}

func TestDoctorClockSkewWarns(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","permissions":["writeOrders"]}}`,
		timeBody:     `{"success":true,"data":{"time":1700000009000}}`, // 9s ahead
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a clock-skew warning must not fail doctor: exit = %d — %s", code, out)
	}
	if !strings.Contains(out, "recvWindow") {
		t.Fatalf("expected a clock-skew warning: %s", out)
	}
}

// TestDoctorFastClockSuggestsTimeSync: when the local clock is AHEAD of the
// server (negative offset), doctor must name the direction and point to
// --time-sync as the fix.
func TestDoctorFastClockSuggestsTimeSync(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","permissions":["writeOrders"]}}`,
		timeBody:     `{"success":true,"data":{"time":1699999991000}}`, // 9s BEHIND local => local is fast
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a clock-skew warning must not fail doctor: exit = %d — %s", code, out)
	}
	if !strings.Contains(out, "AHEAD") || !strings.Contains(out, "time-sync") {
		t.Fatalf("expected a fast-clock fix pointing to --time-sync: %s", out)
	}
}

// TestDoctorDefaultRunSkipsClockConfig: without --diagnose-clock the default run
// never inspects the OS time-sync configuration (no "clock config"/"clock
// service" line), and a skew warning points the user at --diagnose-clock.
func TestDoctorDefaultRunSkipsClockConfig(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","permissions":["writeOrders"]}}`,
		timeBody:     `{"success":true,"data":{"time":1700000009000}}`, // 9s skew -> warns
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — %s", code, out)
	}
	for _, unwanted := range []string{`"name":"clock config"`, `"name":"clock service"`, `"name":"clock source"`} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("default run must not run the OS clock diagnosis, found %s: %s", unwanted, out)
		}
	}
	if !strings.Contains(out, "--diagnose-clock") {
		t.Fatalf("a skew warning should point at --diagnose-clock: %s", out)
	}
}

// TestDoctorDiagnoseClockUnsupported: on a platform without an OS time-sync
// diagnosis yet, --diagnose-clock adds one advisory "clock config" line saying
// so — never failing the run. Windows and Linux have real implementations
// (different check names / live probes), so this asserts only a platform where
// it's still a stub (darwin here); the Linux logic is covered by the pure-logic
// TestEvaluateTimedatectl in package doctorcmd.
func TestDoctorDiagnoseClockUnsupported(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Windows and Linux have real OS time-sync diagnoses; this covers the unsupported stub (darwin)")
	}
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","permissions":["writeOrders"]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--diagnose-clock", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("the OS clock diagnosis must never fail doctor: exit = %d — %s", code, out)
	}
	if !strings.Contains(out, `"name":"clock config"`) || !strings.Contains(out, "isn't supported on "+runtime.GOOS) {
		t.Fatalf("expected an unsupported-platform clock-config line: %s", out)
	}
	// The macOS manual-check hint, not a combined hardcode.
	if !strings.Contains(out, "System Settings") || strings.Contains(out, "timedatectl") {
		t.Fatalf("expected only the macOS hint: %s", out)
	}
}

func TestDoctorRefusesSignedPlaintextBaseURL(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &capturingDoer{whoamiBody: `{"success":true,"data":{}}`, timeBody: okTime}
	_, stderr, code := runWithDeps([]string{"doctor", "--base-url", "http://non-local.example", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage) — %s", code, stderr)
	}
	if !strings.Contains(stderr, "plaintext http") {
		t.Fatalf("expected a plaintext-http refusal: %s", stderr)
	}
	if doer.whoamiQuery != "" {
		t.Fatalf("a signed request must NOT have been sent: query=%q", doer.whoamiQuery)
	}
}

func TestDoctorDeactivatedKeyFails(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"deactivated","permissions":["writeOrders"]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("a deactivated key must fail doctor: exit = %d — %s", code, out)
	}
	if !strings.Contains(out, "not activated") {
		t.Fatalf("expected a key-status failure: %s", out)
	}
}

// ---- setup re-run UX ----

func TestSetupResumesUnboundKey(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	out, stderr, code := runWithDeps([]string{"setup", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("re-running setup on an unbound key must not error: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"status":"awaitingRegistration"`) {
		t.Fatalf("expected awaitingRegistration status: %s", out)
	}
	if !strings.Contains(out, "developers.korbit.co.kr/manage/create") {
		t.Fatalf("resume must re-print the registration link: %s", out)
	}
}

func TestSetupReportsAlreadyConfigured(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home) // creates bound key "bot"
	// A re-run on a bound key now runs a complementary doctor for it (a signed
	// whoami), so a stub Doer keeps it hermetic.
	out, stderr, code := runWithDeps([]string{"setup", "--name", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("re-running setup on a bound key must not error: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"status":"alreadyConfigured"`) {
		t.Fatalf("expected alreadyConfigured status: %s", out)
	}
	// The replacement options ride the result's `next`, and the complementary health
	// check its `doctor` — both on stdout; --json writes nothing to stderr.
	if !strings.Contains(out, "korbit key remove bot") || !strings.Contains(out, "korbit key add bot") {
		t.Fatalf("expected guidance offering replacement via remove+add: %s", out)
	}
	if !strings.Contains(out, `"doctor":`) {
		t.Fatalf("expected the doctor report embedded in the setup result: %s", out)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("setup --compact must write nothing to stderr: %s", stderr)
	}
}

func TestSetupReportsAlreadyConfiguredHMAC(t *testing.T) {
	home := t.TempDir()
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	if _, err := m.AddHMAC("hmac-bot", "secret", "KEYID-HMAC", ""); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := runWithDeps([]string{"setup", "--name", "hmac-bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("re-running setup on a bound hmac key must not error: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"status":"alreadyConfigured"`) {
		t.Fatalf("expected alreadyConfigured status: %s", out)
	}
	if !strings.Contains(out, "key remove hmac-bot") || !strings.Contains(out, "key add hmac-bot --type hmac-sha256") {
		t.Fatalf("expected hmac replacement guidance in the result `next`: %s", out)
	}
}

// TestSetupBindsAPIKeyAndRunsDoctor: `setup --api-key` on an existing UNBOUND key
// binds the issued id and runs the complementary health check, exiting 0.
func TestSetupBindsAPIKeyAndRunsDoctor(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "trading-bot")
	out, stderr, code := runWithDeps([]string{"setup", "--name", "trading-bot", "--api-key", "KEYID-NEW", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("setup --api-key on an unbound key must succeed: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"status":"configured"`) || !strings.Contains(out, `"bound":true`) {
		t.Fatalf("expected a freshly-configured, bound result: %s", out)
	}
	if !strings.Contains(out, `"apiKeyId":"KEYID-NEW"`) {
		t.Fatalf("expected the bound api key id in the result: %s", out)
	}
	if !strings.Contains(out, `"doctor":`) {
		t.Fatalf("expected the embedded doctor report: out=%s", out)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("setup --compact must write nothing to stderr: %s", stderr)
	}
}

// TestSetupRejectsSandboxAPIKey: `setup --api-key SANDBOX_...` is refused — setup
// generates a fresh keypair, which can never be a sandbox key, so it never creates
// a stranded/default sandbox key.
func TestSetupRejectsSandboxAPIKey(t *testing.T) {
	home := t.TempDir()
	out, stderr, code := runWithDeps([]string{"setup", "--name", "sandbox-bot", "--api-key", "SANDBOX_ED25519_KEY_00000001_0000002", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
	if code != 2 {
		t.Fatalf("expected a usage error (exit 2), got %d — %s%s", code, out, stderr)
	}
	if !strings.Contains(stderr, "does not create sandbox keys") {
		t.Fatalf("expected the sandbox-reject message: %s", stderr)
	}
	// Nothing should have been created.
	if _, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("sandbox-bot"); err == nil {
		t.Fatalf("a rejected sandbox setup must not create a key")
	}
}

// TestSetupRejectsAPIKeyOnNewKey: --api-key while generating a NEW key is refused
// — a freshly generated public key isn't registered, so no id could belong to it.
// Nothing is created.
func TestSetupRejectsAPIKeyOnNewKey(t *testing.T) {
	home := t.TempDir()
	out, stderr, code := runWithDeps([]string{"setup", "--name", "fresh", "--api-key", "KEYID-9", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
	if code != 2 {
		t.Fatalf("expected a usage error (exit 2), got %d — %s%s", code, out, stderr)
	}
	if !strings.Contains(stderr, "already-generated key") {
		t.Fatalf("expected guidance to generate-then-bind: %s", stderr)
	}
	if _, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("fresh"); err == nil {
		t.Fatalf("a rejected setup must not create the key")
	}
}

// TestSetupWithInlineCredential: when a credential is supplied inline via the
// environment there is no stored key to create — setup reports the active
// credential and runs doctor against it (exit 0), and rejects --api-key.
func TestSetupWithInlineCredential(t *testing.T) {
	inlineEnv := func(home string) map[string]string {
		return map[string]string{
			"KORBIT_CLI_HOME":           home,
			"KORBIT_CLI_API_KEY_ID":     "KEYID-ENV",
			"KORBIT_CLI_API_KEY_SECRET": "secret",
			"KORBIT_CLI_API_KEY_TYPE":   "hmac-sha256",
		}
	}
	t.Run("reports env credential and runs doctor", func(t *testing.T) {
		home := t.TempDir()
		out, stderr, code := runWithDeps([]string{"setup", "--compact"},
			inlineEnv(home), healthyWhoami, fakeProbe("203.0.113.7", ""))
		if code != 0 {
			t.Fatalf("inline setup must succeed: exit %d — %s%s", code, out, stderr)
		}
		if !strings.Contains(out, `"status":"configuredViaEnvironment"`) {
			t.Fatalf("expected configuredViaEnvironment status: %s", out)
		}
		// The doctor report is embedded in the result document on stdout; --json
		// writes no narration to stderr.
		if !strings.Contains(out, `"doctor":`) {
			t.Fatalf("expected the embedded doctor report: out=%s", out)
		}
		if strings.TrimSpace(stderr) != "" {
			t.Fatalf("setup --compact must write nothing to stderr: %s", stderr)
		}
		// No stored key was created.
		if list, _ := keys.NewManager(home, "file", func() int64 { return 1 }, nil).List(); len(list) != 0 {
			t.Fatalf("inline setup must not create a stored key: %+v", list)
		}
	})
	t.Run("--api-key is rejected", func(t *testing.T) {
		home := t.TempDir()
		out, stderr, code := runWithDeps([]string{"setup", "--api-key", "KEYID-X", "--compact"},
			inlineEnv(home), healthyWhoami, fakeProbe("203.0.113.7", ""))
		if code != 2 {
			t.Fatalf("expected a usage error (exit 2), got %d — %s%s", code, out, stderr)
		}
		if !strings.Contains(stderr, "inline credential") {
			t.Fatalf("expected the --api-key/inline refusal: %s", stderr)
		}
	})
	t.Run("doctor-failure warning uses plain doctor, not --key (environment)", func(t *testing.T) {
		home := t.TempDir()
		rejecting := routeDoer{whoamiStatus: 401, whoamiBody: `{"success":false,"error":{"code":401,"message":"INVALID_API_KEY"}}`, timeBody: okTime}
		// Human mode: the advisory health-check section (with its re-check command) is
		// part of the result on stdout.
		out, stderr, code := runWithDeps([]string{"setup"},
			inlineEnv(home), rejecting, fakeProbe("203.0.113.7", ""))
		if code != 0 {
			t.Fatalf("a failing complementary doctor must not fail setup: %d — %s", code, stderr)
		}
		if !strings.Contains(out, "Warning: the health check found") {
			t.Fatalf("expected a blocking-issue warning on stdout: %s", out)
		}
		if strings.Contains(out, "--key (environment)") {
			t.Fatalf("the re-check command must not pass --key (environment): %s", out)
		}
	})
	t.Run("incomplete inline credential is a config error", func(t *testing.T) {
		home := t.TempDir()
		// Only the id is set — the secret and type are missing, so the inline
		// credential does not resolve. setup must surface that as a failure, not
		// report "configured" with an advisory doctor warning.
		out, stderr, code := runWithDeps([]string{"setup", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home, "KORBIT_CLI_API_KEY_ID": "KEYID-ENV"},
			healthyWhoami, fakeProbe("203.0.113.7", ""))
		if code == 0 {
			t.Fatalf("a partial inline credential must not succeed: %s%s", out, stderr)
		}
		if strings.Contains(out, "configuredViaEnvironment") {
			t.Fatalf("must not claim configured on a partial inline credential: %s", out)
		}
		if !strings.Contains(stderr, "KORBIT_CLI_API_KEY_SECRET") {
			t.Fatalf("expected the missing-secret config error: %s", stderr)
		}
	})
}

// TestSetupAPIKeyOnBoundKey: --api-key on an already-bound key errors when the id
// differs (setup never rebinds), and is an idempotent no-op when the id matches —
// the existing binding is never altered either way.
func TestSetupAPIKeyOnBoundKey(t *testing.T) {
	t.Run("different id errors", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home) // "bot" bound to KEYID-1
		out, stderr, code := runWithDeps([]string{"setup", "--name", "bot", "--api-key", "KEYID-2", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
		if code != 2 {
			t.Fatalf("expected a usage error (exit 2), got %d — %s%s", code, out, stderr)
		}
		if !strings.Contains(stderr, "already bound to a different apiKeyId") {
			t.Fatalf("expected the rebind-refusal message: %s", stderr)
		}
		// The original binding must be intact.
		s, err := keys.NewManager(home, "file", func() int64 { return 1 }, nil).Show("bot")
		if err != nil || s.APIKeyID == nil || *s.APIKeyID != "KEYID-1" {
			t.Fatalf("binding must be unchanged (KEYID-1): %+v err=%v", s, err)
		}
	})
	t.Run("same id is an idempotent no-op", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, stderr, code := runWithDeps([]string{"setup", "--name", "bot", "--api-key", "KEYID-1", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""))
		if code != 0 {
			t.Fatalf("re-supplying the same id must succeed: exit %d — %s", code, stderr)
		}
		if !strings.Contains(out, `"status":"alreadyConfigured"`) {
			t.Fatalf("expected alreadyConfigured: %s", out)
		}
	})
}

// TestSetupAPIKeyDoctorFailureStillSucceeds: when the complementary doctor finds a
// blocking problem (the API rejects the signed whoami), setup STILL exits 0 — the
// health check is advisory — and surfaces the failure as a warning, with the
// failing report embedded.
func TestSetupAPIKeyDoctorFailureStillSucceeds(t *testing.T) {
	home := t.TempDir()
	seedUnboundKey(t, home, "trading-bot")
	rejecting := routeDoer{whoamiStatus: 401, whoamiBody: `{"success":false,"error":{"code":401,"message":"INVALID_API_KEY"}}`, timeBody: okTime}
	out, stderr, code := runWithDeps([]string{"setup", "--name", "trading-bot", "--api-key", "KEYID-NEW", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, rejecting, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("a failing complementary doctor must NOT fail setup: exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"status":"configured"`) {
		t.Fatalf("setup itself succeeded, so status must be configured: %s", out)
	}
	if !strings.Contains(out, `"ok":false`) {
		t.Fatalf("expected the embedded doctor report to record the failure (ok:false): %s", out)
	}
	// --json carries the failure structurally in the embedded report; the human
	// "Warning:" prose is a text-mode rendering and is not on stderr.
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("setup --compact must write nothing to stderr: %s", stderr)
	}
}

// healthyWhoami is a routeDoer whose signed whoami succeeds with a trading key
// and whose /v2/time has a small skew — used by the WS/header doctor tests.
var healthyWhoami = routeDoer{
	whoamiStatus: 200,
	whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","readOrders","writeOrders"]}}`,
	timeBody:     okTime,
}

// runDoctorWS is a doctor run with a WebSocket dialer wired (runWithDeps omits it).
func runDoctorWS(args []string, env map[string]string, doer korbit.Doer,
	probe func(context.Context, string, string, string, int) (string, error), dial stream.Dialer) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:     func(k string) string { return env[k] },
		Stdout:     &out,
		Stderr:     &errb,
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		FamilyDoer: func(string, int) korbit.Doer { return doer },
		IPProbe:    probe,
		WSDial:     dial,
	})
	return out.String(), errb.String(), code
}

// TestDoctorChecksWebSocket: a reachable WS host adds an OK "websocket" check; an
// unreachable one warns (non-fatal, exit 0) with the pin-it fix. The report and
// human header both carry the resolved WS base URL.
func TestDoctorChecksWebSocket(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, _, code := runDoctorWS([]string{"doctor", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""), dialFrames())
		if code != 0 {
			t.Fatalf("healthy doctor with reachable WS must exit 0, got %d — %s", code, out)
		}
		if !strings.Contains(out, `"wsBaseUrl":"wss://ws-api.korbit.co.kr"`) {
			t.Fatalf("report must carry the resolved WS base URL: %s", out)
		}
		if !strings.Contains(out, `"name":"websocket","status":"ok"`) {
			t.Fatalf("expected an OK websocket check: %s", out)
		}
	})

	t.Run("unreachable warns but does not fail", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		deadDial := func(context.Context, string, http.Header) (stream.Conn, error) {
			return nil, errors.New("dial tcp: connection refused")
		}
		out, _, code := runDoctorWS([]string{"doctor", "--compact"},
			map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""), deadDial)
		if code != 0 {
			t.Fatalf("an unreachable WS host is non-fatal (exit 0), got %d — %s", code, out)
		}
		if !strings.Contains(out, `"name":"websocket","status":"warn"`) {
			t.Fatalf("expected a websocket warning: %s", out)
		}
		if !strings.Contains(out, "set-base-url") {
			t.Fatalf("the fix must point at set-base-url: %s", out)
		}
		// Unified with the live-whoami network error: the "(network error)" summary
		// tag plus the raw transport error carried as separate context.
		if !strings.Contains(out, "(network error)") {
			t.Fatalf("an unreachable WS host should carry the (network error) tag: %s", out)
		}
		if !strings.Contains(out, `"context":"dial tcp: connection refused"`) {
			t.Fatalf("the raw transport error should ride as context: %s", out)
		}
	})
}

// TestDoctorHumanHeaderShowsEndpoints: the human checklist header surfaces the
// REST endpoint and WS host the run targeted, so a sandbox/per-key run is obvious.
func TestDoctorHumanHeaderShowsEndpoints(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	out, _, code := runDoctorWS([]string{"doctor"}, // human mode (no --compact)
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""), dialFrames())
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	if !strings.Contains(out, "endpoint: https://api.korbit.co.kr") {
		t.Fatalf("human header must show the REST endpoint: %s", out)
	}
	if !strings.Contains(out, "ws: wss://ws-api.korbit.co.kr") {
		t.Fatalf("human header must show the WS host: %s", out)
	}
	// An OK check's guidance is labeled "note", never "fix".
	if !strings.Contains(out, "note: ensure these are in the key's IP allowlist") {
		t.Fatalf("an OK check's guidance must be a note, not a fix: %s", out)
	}
}

// TestDoctorWarnsPerKeyBackendUnavailable: a key whose record points at an
// unreachable backend gets a keystore warning naming it, with the per-key
// recovery migrate as the fix (and the private-key check fails for it).
func TestDoctorWarnsPerKeyBackendUnavailable(t *testing.T) {
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	defer keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home) // "bot", material in the file backend
	// The record claims the (unavailable) keychain.
	m := keys.NewManager(home, "file", nil, nil)
	if err := m.SetKeystoreBackend("bot", "keychain"); err != nil {
		t.Fatal(err)
	}

	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home},
		routeDoer{whoamiStatus: 200, whoamiBody: `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances"]}}`, timeBody: okTime},
		fakeProbe("203.0.113.7", ""))
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (private key unreachable)", code)
	}
	if !strings.Contains(out, "keychain keystore holding key(s) bot is not available") {
		t.Fatalf("expected per-key keystore warning: %s", out)
	}
	if !strings.Contains(out, "keystore migrate file bot") {
		t.Fatalf("fix should name the per-key recovery command: %s", out)
	}
}

func TestDoctorAccountSeqAllowed(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[1,2,3]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — %s", code, out)
	}
	if !strings.Contains(out, "default accountSeq") || !strings.Contains(out, "is in the key's allowed list") {
		t.Fatalf("expected accountSeq OK check: %s", out)
	}
}

func TestDoctorAccountSeqNotAllowed(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// Set the key's default to 5, which is NOT in allowedAccountSeqs.
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	if err := m.SetDefaultAccountSeq("bot", 5); err != nil {
		t.Fatal(err)
	}
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[1,2]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warning only) — %s", code, out)
	}
	if !strings.Contains(out, "default accountSeq") || !strings.Contains(out, "is not in the key's allowedAccountSeqs") {
		t.Fatalf("expected accountSeq WARN check: %s", out)
	}
}

func TestDoctorAccountSeqEmptyAllowedListWarns(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// An explicit empty allowedAccountSeqs means the key is valid, but cannot use
	// any account-scoped endpoint unless the portal permissions are updated.
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warning only) — %s", code, out)
	}
	if !strings.Contains(out, "default accountSeq") || !strings.Contains(out, "allowedAccountSeqs []") {
		t.Fatalf("expected accountSeq empty-list warning: %s", out)
	}
}

func TestDoctorInlineAccountSeqWarningDoesNotSuggestStoredKeyCommand(t *testing.T) {
	home := t.TempDir()
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"hmac-sha256","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[2]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{
			"KORBIT_CLI_HOME":           home,
			"KORBIT_CLI_API_KEY_ID":     "KEYID-ENV",
			"KORBIT_CLI_API_KEY_SECRET": "secret",
			"KORBIT_CLI_API_KEY_TYPE":   "hmac-sha256",
		}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warning only) — %s", code, out)
	}
	if !strings.Contains(out, "pass an allowed --account-seq") {
		t.Fatalf("inline accountSeq warning should suggest explicit --account-seq: %s", out)
	}
	if strings.Contains(out, "key set-default-account-seq") {
		t.Fatalf("inline accountSeq warning must not suggest a stored-key command: %s", out)
	}
}

func TestDoctorAccountSeqMissingFieldSkipsDefensively(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// The live API always includes allowedAccountSeqs. If an older/malformed
	// fixture omits it, doctor skips the accountSeq check rather than inventing a
	// permission verdict.
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — %s", code, out)
	}
	if strings.Contains(out, "default accountSeq") {
		t.Fatalf("accountSeq check should be skipped when allowedAccountSeqs is missing: %s", out)
	}
}

func TestDoctorAccountSeqUsesMainWhenNotConfigured(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// No per-key default configured; allowedAccountSeqs includes 1 (Main).
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[1]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — %s", code, out)
	}
	if !strings.Contains(out, `sub-account 1 is in the key's allowed list`) {
		t.Fatalf("expected Main account check OK: %s", out)
	}
}

func TestDoctorAccountSeqMainNotAllowed(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// No per-key default configured; allowedAccountSeqs does NOT include 1.
	doer := routeDoer{
		whoamiStatus: 200,
		whoamiBody:   `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readBalances","writeOrders"],"allowedAccountSeqs":[2,3]}}`,
		timeBody:     okTime,
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warning only) — %s", code, out)
	}
	if !strings.Contains(out, "sub-account 1 is not in") {
		t.Fatalf("expected Main account WARN: %s", out)
	}
}
