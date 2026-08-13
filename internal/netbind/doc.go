// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package netbind resolves the outbound networking flags (--bind / --family)
// into a Binder that pins where the CLI's connections come from and which IP
// family they use, so a customer on a multi-homed host can pin traffic to a link
// (reliability) and spread calls across source IPs to raise the effective rate
// limit (the public endpoints are rate-limited per source IP).
//
// # The governing rule
//
// --bind is a SINGLE source — one IP address or one interface; --family says
// "use only this IP family". Keeping --bind to one token keeps dual-stack simple:
// the binder never needs two distinct per-family source addresses, so every dial
// delegates to the stdlib dialer (no hand-rolled Happy-Eyeballs). The consequence:
//   - an address binds exactly its own family, so --bind <v4-address> narrows to
//     IPv4 (a v6 connection can't use a v4 source, and must not silently fall back
//     to the default source);
//   - an interface device-bind (see below) covers BOTH families through one
//     control hook, so the interface is the way to bind dual-stack;
//   - where device binding is unavailable (unprivileged Linux, other platforms)
//     only a single-family source IP is possible, so a dual-stack interface there
//     is refused rather than half-bound (see the table).
//
// # --family × --bind behavior
//
//	--family    --bind                     effective   source
//	--------    ------                     ---------   ------
//	dualstack   (none)                     dualstack   OS default (unchanged)
//	dualstack   203.0.113.7 (v4)           ipv4*       v4 = that address
//	dualstack   2001:db8::5 (v6)           ipv6*       v6 = that address
//	dualstack   eth0 (device-bind)         dualstack   both families bound to eth0
//	dualstack   eth0 (fallback, one fam)   that fam*   that family's source IP
//	dualstack   eth0 (fallback, v4+v6)     ERROR       needs privilege/addr/family
//	ipv4        (none)                      ipv4        default v4 source
//	ipv4        203.0.113.7                 ipv4        v4 = that address
//	ipv4        eth0 (has a v4 addr)        ipv4        v4 via eth0
//	ipv6        ... (symmetric to ipv4)     ipv6        ...
//
// Entries marked * are narrowed from dualstack by the single-family source.
// Rejected up front (exit 2): a list (--bind a,b — bind an interface for
// dual-stack); --family ipv4 with a v6 address or v6-only interface (and vice
// versa); an unknown or down interface; --family ipvN with an interface that has
// no address in family N; and the fallback dual-stack-interface case above (the
// fix names grant-privilege / bind-an-address / pick-a-family). A device-bound
// interface stays dual-stack — it is the one --bind that covers both families;
// a family the device lacks simply fails to dial there (it never leaks).
//
// # Source-binding mechanics
//
// A source IP is bound with net.Dialer.LocalAddr. An interface is bound at the
// device level where that is possible and unprivileged — macOS
// (IP_BOUND_IF/IPV6_BOUND_IF) and Windows (IP_UNICAST_IF/IPV6_UNICAST_IF)
// always, Linux (SO_BINDTODEVICE) only with root/CAP_NET_RAW. Without that
// privilege (and on other platforms) it falls back to binding the interface's
// own source IP; egress then follows the routing table rather than the device,
// and Resolve discloses this through warn. The IP family is applied by
// substituting tcp4/tcp6 for the dialed network.
//
// # The one seam
//
// Everything is built from Binder.DialContext — the signature net.Dialer,
// http.Transport, and golang.org/x/net/proxy.ContextDialer all share.
// Transport/WSTransport and Doer/WSClient build the REST and WebSocket clients;
// FamilyDoer builds a client pinned to one explicit TCP family (with the binder's
// source for that family) for the ip/doctor probes that test families
// independently; when the binder pins a single family it refuses the other at
// dial time, so the family the caller asks for can never escape the binding.
// Because the transport builders consume only that one dial seam, a
// forward-dialer proxy can wrap DialContext with no caller change.
//
// # Validation and the no-leak guarantee
//
// Resolve front-loads every detectable mistake (the rejected combinations above)
// so they fail fast with a fix-it message instead of surfacing later as a
// confusing dial error; only genuinely route-dependent failures (no route to
// host) reach dial time. Outbound never uses a family or source the config
// excluded — the narrowing rule closes the per-family default-source fallback,
// the family pin closes cross-family leaks, a fallback that finds no usable
// source address errors rather than dialing the default source, and FamilyDoer
// refuses an explicit family the binder excludes rather than dial it unbound. The one
// documented exception is DNS: name resolution goes out the default path (the
// OS/stdlib resolver), and only the resulting TCP connect honors --bind/--family.
//
// # Proxies
//
// Every outbound transport — REST (Transport), WebSocket (WSTransport), and the
// ip/doctor probes (FamilyDoer) — sets Proxy: http.ProxyFromEnvironment, so the
// standard HTTP(S)_PROXY/NO_PROXY environment is honored uniformly. When a proxy
// is in use, --bind/--family apply to the connection to the proxy (the proxy
// then originates the connection to the API).
//
// A zero Config (no --bind, dual-stack) resolves to a nil *Binder: callers leave
// their default dialers in place, so the package is inert until a flag is set.
package netbind
