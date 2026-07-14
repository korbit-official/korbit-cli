// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// TestTransportsHonorEnvProxy pins that every outbound transport (REST, WS, and
// the family-pinned probe — both bound and unbound) wires Proxy to
// http.ProxyFromEnvironment, so HTTP(S)_PROXY/NO_PROXY are honored consistently.
func TestTransportsHonorEnvProxy(t *testing.T) {
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	isEnvProxy := func(p func(*http.Request) (*url.URL, error)) bool {
		return p != nil && reflect.ValueOf(p).Pointer() == want
	}
	b, err := Resolve(Config{Family: FamilyV4}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cases := map[string]*http.Transport{
		"REST":            b.Transport(),
		"WS":              b.WSTransport(),
		"FamilyDoer":      FamilyDoer(b, "tcp4", 2000).Transport.(*http.Transport),
		"FamilyDoer(nil)": FamilyDoer(nil, "tcp4", 2000).Transport.(*http.Transport),
	}
	for name, tr := range cases {
		if !isEnvProxy(tr.Proxy) {
			t.Errorf("%s transport: Proxy is not http.ProxyFromEnvironment", name)
		}
	}
}

// TestFamilyDoerPinsFamily: a FamilyDoer pinned to tcp4 reaches the IPv4 loopback
// test server, while one pinned to tcp6 cannot — the family pin is enforced.
func TestFamilyDoerPinsFamily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := FamilyDoer(nil, "tcp4", 2000).Get(srv.URL)
	if err != nil {
		t.Fatalf("tcp4 should reach the IPv4 loopback server: %v", err)
	}
	resp.Body.Close()

	if resp, err := FamilyDoer(nil, "tcp6", 2000).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("tcp6 to an IPv4 loopback server should fail")
	}
}

// TestFamilyDoerRefusesExcludedFamily: a FamilyDoer built from a single-family
// binder refuses a dial of the OTHER family rather than leaking it out the
// default source. The refusal is at dial time (the source has no binding for the
// excluded family), and the server is reachable so only the family gate can
// cause the failure.
func TestFamilyDoerRefusesExcludedFamily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A v4 --bind narrows the binder to IPv4; a tcp6 request must be refused.
	b, err := Resolve(Config{Bind: "127.0.0.1"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resp, err := FamilyDoer(b, "tcp6", 2000).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("tcp6 on a v4-bound binder must be refused, not dialed out the default source")
	}
	if !strings.Contains(err.Error(), "refusing to dial tcp6") {
		t.Fatalf("want a netbind refusal error, got: %v", err)
	}

	// The permitted family still dials normally (loopback server is IPv4).
	resp, err = FamilyDoer(b, "tcp4", 2000).Get(srv.URL)
	if err != nil {
		t.Fatalf("tcp4 on a v4-bound binder should connect: %v", err)
	}
	resp.Body.Close()
}

func TestParseFamily(t *testing.T) {
	cases := map[string]Family{
		"":          FamilyDual,
		"dualstack": FamilyDual,
		"dual":      FamilyDual,
		"AUTO":      FamilyDual,
		"ipv4":      FamilyV4,
		"v4":        FamilyV4,
		"4":         FamilyV4,
		"ipv6":      FamilyV6,
		"V6":        FamilyV6,
		"6":         FamilyV6,
	}
	for in, want := range cases {
		got, err := ParseFamily(in)
		if err != nil || got != want {
			t.Errorf("ParseFamily(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	if _, err := ParseFamily("bogus"); err == nil {
		t.Error("ParseFamily(bogus) = nil error; want error")
	}
}

func TestClassifySpec(t *testing.T) {
	cases := []struct {
		in   string
		kind specKind
		val  string
	}{
		{"192.0.2.10", specAddr, "192.0.2.10"},
		{"2001:db8::1", specAddr, "2001:db8::1"},
		{"eth0", specIface, "eth0"},
		{"10", specIface, "10"},     // numeric, not an IP -> interface name
		{"addr!10", specAddr, "10"}, // forced address (will fail to parse later)
		{"host!192.0.2.5", specAddr, "192.0.2.5"},
		{"if!192.0.2.5", specIface, "192.0.2.5"}, // forced interface name
	}
	for _, c := range cases {
		k, v := classifySpec(c.in)
		if k != c.kind || v != c.val {
			t.Errorf("classifySpec(%q) = %v,%q; want %v,%q", c.in, k, v, c.kind, c.val)
		}
	}
}

func TestResolveZeroConfigIsNil(t *testing.T) {
	b, err := Resolve(Config{}, nil)
	if err != nil || b != nil {
		t.Fatalf("Resolve(zero) = %v, %v; want nil, nil", b, err)
	}
}

func TestResolveValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{"family-v6-vs-v4-literal", Config{Bind: "192.0.2.10", Family: FamilyV6}, "conflicts with"},
		{"family-v4-vs-v6-literal", Config{Bind: "2001:db8::1", Family: FamilyV4}, "conflicts with"},
		{"bad-address", Config{Bind: "addr!nope"}, "not a valid IP"},
		{"multiple-sources", Config{Bind: "192.0.2.10,2001:db8::10"}, "single source"},
		{"unknown-interface", Config{Bind: "if!nosuchif0"}, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Resolve(c.cfg, nil)
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("Resolve(%+v) err = %v; want containing %q", c.cfg, err, c.wantSub)
			}
		})
	}
}

func TestResolveSingleAddressNarrowsFamily(t *testing.T) {
	// A single v4 source pins the binder to IPv4, so v6 can't fall back to the
	// default source — "bind to this source" means exactly that family.
	b, err := Resolve(Config{Bind: "192.0.2.10"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if b.family != FamilyV4 {
		t.Errorf("family = %v; want v4 (narrowed)", b.family)
	}
	if la := b.famFor("tcp4").localAddr; la == nil {
		t.Error("famFor(tcp4) = nil; want bound")
	}
	if la := b.famFor("tcp6").localAddr; la != nil {
		t.Errorf("famFor(tcp6) = %v; want nil (unbound)", la)
	}
}

// TestDialContextFamily exercises real (loopback) dialing: a forced family and a
// bound loopback source must connect to an IPv4 listener, while forcing IPv6
// must fail to reach it.
func TestDialContextFamily(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	ctx := context.Background()

	// Forced IPv4 connects.
	b4, _ := Resolve(Config{Family: FamilyV4}, nil)
	if c, err := b4.DialContext(ctx, "tcp", addr); err != nil {
		t.Errorf("ipv4 dial: %v", err)
	} else {
		c.Close()
	}

	// A single v4 source narrows to IPv4, so this is the single-family path.
	bSrc, _ := Resolve(Config{Bind: "127.0.0.1"}, nil)
	if c, err := bSrc.DialContext(ctx, "tcp", addr); err != nil {
		t.Errorf("bound-source dial: %v", err)
	} else {
		c.Close()
	}

	// Forced IPv6 cannot reach the IPv4 listener.
	b6, _ := Resolve(Config{Family: FamilyV6}, nil)
	if c, err := b6.DialContext(ctx, "tcp", addr); err == nil {
		c.Close()
		t.Error("ipv6 dial to v4 listener: want error, got nil")
	}
}

func TestDeviceBindFallbackWarns(t *testing.T) {
	// A single-family interface bind exercises the source-IP fallback on a host
	// where device binding is unavailable/unprivileged (it must warn); where device
	// binding is supported it binds the device instead (no warn). --family ipv4
	// keeps it single-family so a dual-stack loopback doesn't hit the refusal below.
	iface := loopbackName(t)
	var warned bool
	_, err := Resolve(Config{Bind: "if!" + iface, Family: FamilyV4}, func(string) { warned = true })
	if err != nil {
		t.Fatalf("Resolve(if!%s): %v", iface, err)
	}
	if !deviceBindSupported() && !warned {
		t.Error("unprivileged interface bind did not warn about the source-IP fallback")
	}
}

// TestResolveDualStackInterfaceFallbackErrors: where device binding is
// unavailable, binding a dual-stack interface without --family is refused (it
// would otherwise need two per-family sources). Skipped where device binding works.
func TestResolveDualStackInterfaceFallbackErrors(t *testing.T) {
	if deviceBindSupported() {
		t.Skip("device binding available — no source-IP fallback to exercise")
	}
	iface := dualStackLoopback(t)
	_, err := Resolve(Config{Bind: "if!" + iface}, nil)
	if err == nil || !strings.Contains(err.Error(), "privileges") {
		t.Fatalf("Resolve(if!%s) dual = %v; want a privileges error", iface, err)
	}
}

func loopbackName(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 && ifc.Flags&net.FlagUp != 0 {
			if has4, _ := interfaceFamilies(&ifc); has4 {
				return ifc.Name
			}
		}
	}
	t.Skip("no up loopback interface with an IPv4 address")
	return ""
}

// dualStackLoopback returns an up loopback interface that has both an IPv4 and an
// IPv6 address, skipping the test when none exists.
func dualStackLoopback(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 && ifc.Flags&net.FlagUp != 0 {
			if has4, has6 := interfaceFamilies(&ifc); has4 && has6 {
				return ifc.Name
			}
		}
	}
	t.Skip("no up dual-stack loopback interface")
	return ""
}
