// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"net/http"
	"strings"
	"testing"
)

const etwBody = `{"success":false,"error":{"code":400,"message":"EXCEED_TIME_WINDOW"}}`

// whoamiOKBody is a live, trade-capable, activated key — enough for the doctor
// whoami checks to pass once the clock lets the request through.
const whoamiOKBody = `{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["writeOrders"]}}`

// TestDoctorReactivelyCorrectsClock: under the default --time-sync (auto), a
// whoami rejected with EXCEED_TIME_WINDOW must resync the clock and retry to
// success — so a fixable skew does not mask the key diagnosis. Crucially the
// clock-skew check still reports the drift separately (a raw measurement), so the
// operator is told about it even though correction let the whoami through. The
// server clock here is 8s ahead of the (fixed) local clock, past the 5s window.
func TestDoctorReactivelyCorrectsClock(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &pathDoer{
		onTime: func() (*http.Response, error) { return timeResp(1700000008000) }, // +8s vs local
		onEndpoint: func(n int) (*http.Response, error) {
			if n == 1 {
				return resp(400, etwBody, nil), nil // first attempt: outside the window
			}
			return resp(200, whoamiOKBody, nil), nil // after resync
		},
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("doctor should auto-correct the clock and pass: exit=%d — %s", code, out)
	}
	if !strings.Contains(out, `"name":"live whoami","status":"ok"`) {
		t.Fatalf("expected live whoami OK after the reactive resync: %s", out)
	}
	if doer.endpoint < 2 {
		t.Fatalf("expected the whoami to be resent after the resync, got %d endpoint call(s)", doer.endpoint)
	}
	// The correction must NOT hide the drift: the clock-skew check reports it raw.
	if !strings.Contains(out, `"name":"clock skew","status":"warn"`) {
		t.Fatalf("expected the clock-skew check to still WARN about the 8s drift: %s", out)
	}
}

// TestDoctorTimeSyncOnProactivelySyncs: --time-sync on measures the server clock
// up front, so a /v2/time probe precedes the whoami (unlike the reactive default,
// where the whoami is sent first and only resyncs on rejection).
func TestDoctorTimeSyncOnProactivelySyncs(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &pathDoer{
		onTime:     func() (*http.Response, error) { return timeResp(1700000000000) },
		onEndpoint: func(int) (*http.Response, error) { return resp(200, whoamiOKBody, nil), nil },
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact", "--time-sync", "on"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d — %s", code, out)
	}
	sawTime := false
	for _, r := range doer.reqs {
		if strings.Contains(r.URL.Path, "/v2/time") {
			sawTime = true
		}
		if strings.Contains(r.URL.Path, "currentKeyInfo") {
			if !sawTime {
				t.Fatalf("--time-sync on must probe /v2/time before the whoami (proactive sync)")
			}
			return
		}
	}
	t.Fatalf("expected a whoami request; reqs=%d", len(doer.reqs))
}

// TestDoctorTimeSyncOffSurfacesETW: --time-sync off opts out of clock
// correction, so an EXCEED_TIME_WINDOW whoami is sent once and surfaces raw
// (exit non-zero) rather than being resynced away.
func TestDoctorTimeSyncOffSurfacesETW(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &pathDoer{
		onTime:     func() (*http.Response, error) { return timeResp(1700000000000) },
		onEndpoint: func(int) (*http.Response, error) { return resp(400, etwBody, nil), nil },
	}
	out, _, code := runWithDeps([]string{"doctor", "--compact", "--time-sync", "off"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer, fakeProbe("203.0.113.7", ""))
	if code == 0 {
		t.Fatalf("--time-sync off must not auto-correct: expected a failing report, got exit 0 — %s", out)
	}
	if !strings.Contains(out, "EXCEED_TIME_WINDOW") {
		t.Fatalf("expected the raw EXCEED_TIME_WINDOW to surface: %s", out)
	}
	if doer.endpoint != 1 {
		t.Fatalf("--time-sync off should send the whoami once (no resync retry), got %d", doer.endpoint)
	}
}
