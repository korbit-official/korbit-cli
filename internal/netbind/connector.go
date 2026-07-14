// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DialContext is the one seam every bound client is built from — the signature
// net.Dialer, http.Transport.DialContext, and golang.org/x/net/proxy.ContextDialer
// all share. It applies the configured IP family (substituting tcp4/tcp6 for the
// network http hands it) and the per-family source binding. Because every bound
// client routes through this one method, a forward-dialer proxy can wrap it
// without changing the transport builders below.
func (b *Binder) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch b.family {
	case FamilyV4:
		return b.dialerFor(b.v4).DialContext(ctx, "tcp4", addr)
	case FamilyV6:
		return b.dialerFor(b.v6).DialContext(ctx, "tcp6", addr)
	default:
		// Dual-stack. By construction (Resolve narrows any single-family source to
		// that family, and refuses a per-family source for both families), the only
		// thing bound here is a device-bind control shared by both families — never
		// a per-family LocalAddr. So one stdlib dialer dials "tcp" and gets the
		// real Happy-Eyeballs, with the device pin (if any) applied per candidate.
		d := &net.Dialer{ControlContext: firstControl(b.v4, b.v6)}
		return d.DialContext(ctx, network, addr)
	}
}

func (b *Binder) dialerFor(fb famBind) *net.Dialer {
	return &net.Dialer{LocalAddr: fb.localAddr, ControlContext: fb.control}
}

// Transport returns the bound HTTP transport for REST calls: a clone of the
// stdlib default (preserving its connection pooling and HTTP/2 attempt) with the
// dial seam swapped in and Proxy set explicitly to http.ProxyFromEnvironment.
func (b *Binder) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = b.DialContext
	return t
}

// WSTransport is the bound transport for the WebSocket handshake. The Upgrade is
// HTTP/1.1, so HTTP/2 is disabled — a hand-built transport with a custom
// DialContext does not auto-enable it, and this makes that explicit.
func (b *Binder) WSTransport() *http.Transport {
	t := b.Transport()
	t.ForceAttemptHTTP2 = false
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
		t.TLSClientConfig.NextProtos = nil
	}
	return t
}

// Doer is the bound HTTP client for REST calls (no client-level timeout; the
// per-attempt deadline is carried on the request context, matching the default
// client this replaces).
func (b *Binder) Doer() *http.Client { return &http.Client{Transport: b.Transport()} }

// WSClient is the bound HTTP client for the WebSocket handshake.
func (b *Binder) WSClient() *http.Client { return &http.Client{Transport: b.WSTransport()} }

// FamilyDoer returns an HTTP client whose connections are pinned to the given TCP
// family ("tcp4"/"tcp6") and bound to b's source for that family. b may be nil
// (no source binding — just the family pin). It is the family-pinned dialer the
// ip/doctor diagnostics use to probe one family at a time; the family pin lives
// here so all dial construction stays in this package. The caller supplies the
// per-probe timeout (the command layer resolves --timeout / its default). Like the
// REST/WS transports it honors the standard HTTP(S)_PROXY/NO_PROXY environment, so
// the probes report what the proxied calls actually see (the family pin then
// applies to the connection to the proxy).
//
// When b pins a single family (a v4/v6 --bind or --family), a request for the
// OTHER family is refused at dial time rather than dialing it out the default
// source — the no-leak guarantee is enforced here, not left to every caller to
// cap. (b == nil carries no such constraint: it is only a family pin, with no
// source to leak.) Callers already cap to b.Family().Networks(); this makes a
// miss a clean "family unavailable" dial error instead of a silent leak.
func FamilyDoer(b *Binder, network string, timeoutMs int) *http.Client {
	dialer := &net.Dialer{Timeout: time.Duration(timeoutMs) * time.Millisecond}
	excluded := false
	if b != nil {
		fb := b.famFor(network)
		dialer.LocalAddr, dialer.ControlContext = fb.localAddr, fb.control
		excluded = !b.family.permits(network)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				if excluded {
					return nil, fmt.Errorf("netbind: refusing to dial %s — bound to %s only", network, b.family.label())
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
		Timeout: time.Duration(timeoutMs) * time.Millisecond,
	}
}

func firstControl(a, b famBind) controlFunc {
	if a.control != nil {
		return a.control
	}
	return b.control
}
