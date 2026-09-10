// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/cli/probe"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
)

type fnDoer func(*http.Request) (*http.Response, error)

func (f fnDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

// TestProbeEndpoints covers the smoke-test classification, including the subtle
// case where a refused WebSocket upgrade still means the host is reachable.
func TestProbeEndpoints(t *testing.T) {
	rest200 := fnDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	rest500 := fnDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
	})
	restErr := fnDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	dialOK := func(context.Context, string, http.Header) (stream.Conn, error) { return stubConn{}, nil }
	dialRejected := func(context.Context, string, http.Header) (stream.Conn, error) {
		return nil, &stream.UpgradeError{Status: 403}
	}
	dialErr := func(context.Context, string, http.Header) (stream.Conn, error) {
		return nil, errors.New("no route to host")
	}

	// REST: 200 reachable, 500 reachable-with-warning, transport error unreachable.
	if c := probe.REST(context.Background(), rest200, "https://api-test.digitalx.miraeasset.com"); !c.Reachable {
		t.Errorf("200 should be reachable: %+v", c)
	}
	if c := probe.REST(context.Background(), rest500, "https://api-test.digitalx.miraeasset.com"); !c.Reachable {
		t.Errorf("500 host is still reachable: %+v", c)
	}
	if c := probe.REST(context.Background(), restErr, "https://api-test.digitalx.miraeasset.com"); c.Reachable {
		t.Errorf("transport error should be unreachable: %+v", c)
	}

	// WS: a clean dial is reachable; a refused upgrade is still reachable (the
	// host answered); a transport error is unreachable.
	if c := probe.WS(context.Background(), dialOK, "wss://ws-api-test.digitalx.miraeasset.com"); !c.Reachable {
		t.Errorf("clean dial should be reachable: %+v", c)
	}
	if c := probe.WS(context.Background(), dialRejected, "wss://ws-api-test.digitalx.miraeasset.com"); !c.Reachable {
		t.Errorf("refused upgrade means the host answered (reachable): %+v", c)
	}
	if c := probe.WS(context.Background(), dialErr, "wss://ws-api-test.digitalx.miraeasset.com"); c.Reachable {
		t.Errorf("transport error should be unreachable: %+v", c)
	}

	// Nil deps are reported "not checked", never as a failure.
	v := probe.Endpoints(nil, nil, "https://api-test.digitalx.miraeasset.com", "wss://ws-api-test.digitalx.miraeasset.com", 1000)
	if !v.REST.Reachable || !v.WS.Reachable {
		t.Errorf("nil deps must not be reported unreachable: %+v", v)
	}
}

// TestResolveDepsDefaultsDoer guards the smoke test: production wires only
// Stdout/Stderr, so a nil Doer must default to a working client (mirroring
// apiclient.Client) or the REST probe would silently no-op as "not checked".
func TestResolveDepsDefaultsDoer(t *testing.T) {
	if resolveDeps(Deps{}).Doer == nil {
		t.Fatal("resolveDeps must default a nil Doer so the REST smoke test can run")
	}
}

// stubConn is a no-op stream.Conn for the clean-dial probe path.
type stubConn struct{}

func (stubConn) Read(context.Context) ([]byte, error) { return nil, nil }
func (stubConn) Write(context.Context, []byte) error  { return nil }
func (stubConn) Ping(context.Context) error           { return nil }
func (stubConn) Close() error                         { return nil }

// TestDeriveWSBaseURL pins the REST→WebSocket host convention: a first label
// starting with "api" becomes "ws-api" with everything up to the first "-"
// dropped and any "-suffix" kept; non-api hosts (the local sandbox) keep their
// host; the scheme maps http→ws and https→wss.
func TestDeriveWSBaseURL(t *testing.T) {
	cases := []struct{ rest, want string }{
		{"https://api.digitalx.miraeasset.com", "wss://ws-api.digitalx.miraeasset.com"},
		{"https://api-test.digitalx.miraeasset.com", "wss://ws-api-test.digitalx.miraeasset.com"},
		{"https://apix.example.com", "wss://ws-api.example.com"},           // trailing chunk dropped
		{"https://api2-test.example.com", "wss://ws-api-test.example.com"}, // digit dropped, suffix kept
		{"http://127.0.0.1:9999", "ws://127.0.0.1:9999"},                   // sandbox: same host, port kept
		{"https://localhost:8443", "wss://localhost:8443"},                 // non-api host unchanged
		{"https://api-test.digitalx.miraeasset.com/", "wss://ws-api-test.digitalx.miraeasset.com"},
		{"http://[::1]:9999", "ws://[::1]:9999"},                                                          // IPv6 literal: re-bracketed, port kept
		{"https://APIx.example.com", "wss://ws-api.example.com"},                                          // prefix match is case-insensitive
		{"https://user:pw@api-test.digitalx.miraeasset.com", "wss://ws-api-test.digitalx.miraeasset.com"}, // userinfo dropped
		{"https://api", "wss://ws-api"},                                                                   // single label
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := probe.DeriveWSBaseURL(c.rest); got != c.want {
			t.Errorf("probe.DeriveWSBaseURL(%q) = %q, want %q", c.rest, got, c.want)
		}
	}
}

// TestValidateWSBaseURL covers the scheme rule and the "don't sign over
// plaintext ws to a non-loopback host" guard.
func TestValidateWSBaseURL(t *testing.T) {
	// Bad scheme / shape always rejected.
	for _, bad := range []string{"https://ws-api.example.test", "ftp://x", "notaurl", "wss://"} {
		if err := probe.ValidateWSBaseURL(bad, "--ws-base-url", false); err == nil {
			t.Errorf("probe.ValidateWSBaseURL(%q) should fail", bad)
		}
	}
	// wss to a remote host is fine, signed or not.
	if err := probe.ValidateWSBaseURL("wss://ws-api.example.test", "--ws-base-url", true); err != nil {
		t.Errorf("wss remote should pass: %v", err)
	}
	// Plaintext ws to a remote host is rejected only when signing.
	if err := probe.ValidateWSBaseURL("ws://ws-api.example.test", "--ws-base-url", true); err == nil {
		t.Error("signed ws:// to remote host should be rejected")
	}
	if err := probe.ValidateWSBaseURL("ws://ws-api.example.test", "--ws-base-url", false); err != nil {
		t.Errorf("unsigned ws:// remote should pass: %v", err)
	}
	// Plaintext ws to loopback is allowed even when signing (the sandbox).
	if err := probe.ValidateWSBaseURL("ws://127.0.0.1:9999", "--ws-base-url", true); err != nil {
		t.Errorf("signed ws:// to loopback should pass: %v", err)
	}
}
