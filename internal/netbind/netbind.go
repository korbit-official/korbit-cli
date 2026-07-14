// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package netbind

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// Family is the requested outbound IP family.
type Family int

const (
	// FamilyDual dials dual-stack (the default, OS Happy-Eyeballs behavior).
	FamilyDual Family = iota
	// FamilyV4 forces IPv4 (A records only).
	FamilyV4
	// FamilyV6 forces IPv6 (AAAA records only).
	FamilyV6
)

// ParseFamily maps a flag/env string to a Family. "" and dualstack/dual map to
// FamilyDual; ipv4/v4/4 and ipv6/v6/6 to the pinned families.
func ParseFamily(s string) (Family, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "dual", "dualstack", "auto":
		return FamilyDual, nil
	case "ipv4", "v4", "4":
		return FamilyV4, nil
	case "ipv6", "v6", "6":
		return FamilyV6, nil
	default:
		return FamilyDual, fmt.Errorf("--family: invalid value %q (want dualstack, ipv4, or ipv6)", s)
	}
}

// Config is the unresolved networking request from the CLI flags/env.
type Config struct {
	// Bind is the raw --bind value: a SINGLE source IP or interface name
	// (optionally "addr!"/"host!"/"if!"-prefixed). An address binds its own family;
	// an interface binds both families when device binding is available, else its
	// source IP for one family. Empty = OS default source.
	Bind string
	// Family is the requested IP family.
	Family Family
}

// IsZero reports whether the config requests no customization at all (default
// source, dual-stack) — Resolve then returns a nil Binder.
func (c Config) IsZero() bool { return strings.TrimSpace(c.Bind) == "" && c.Family == FamilyDual }

// controlFunc matches net.Dialer.ControlContext.
type controlFunc = func(ctx context.Context, network, address string, c syscall.RawConn) error

// famBind is the resolved source binding for one IP family. A nil localAddr +
// nil control means "OS default source for this family".
type famBind struct {
	localAddr *net.TCPAddr // bound source address (nil = OS default)
	control   controlFunc  // device-bind hook applied pre-connect (nil = none)
}

func (fb famBind) set() bool { return fb.localAddr != nil || fb.control != nil }

// Binder is the resolved outbound networking customization. It is consumed two
// ways: Transport/WSTransport/Doer/WSClient build the bound HTTP+WS clients
// (family chosen by Config.Family), and FamilyDoer(b, network, …) builds a
// client pinned to one explicit family for the ip/doctor probes. A nil *Binder
// means "use the OS defaults" — callers skip overriding their dialers, and
// FamilyDoer(nil, …) still pins the family without a source binding.
type Binder struct {
	family Family
	v4, v6 famBind
}

// Resolve validates a Config and returns the Binder it describes, or a nil
// Binder when the config requests nothing (default source, dual-stack). warn
// receives non-fatal advisories (e.g. the unprivileged-Linux device-bind
// fallback); it may be nil. All detectable mistakes — a multi-source list (--bind
// is a single source), a family/literal mismatch, an unknown or down interface,
// an interface with no address in the wanted family, and a dual-stack interface
// where only a single-family source IP is possible — are returned as errors here
// so they fail fast instead of surfacing as a confusing dial-time failure later.
func Resolve(cfg Config, warn func(string)) (*Binder, error) {
	if cfg.IsZero() {
		return nil, nil
	}
	if warn == nil {
		warn = func(string) {}
	}
	b := &Binder{family: cfg.Family}

	if bind := strings.TrimSpace(cfg.Bind); bind != "" {
		// --bind is a SINGLE source (one IP or one interface). One token keeps the
		// dual case simple: an address binds exactly its own family, and an
		// interface either device-binds both families (one control) or is refused
		// when only a per-family source IP is possible (see applySpec). Either way
		// no dial ever needs two distinct per-family sources.
		if strings.ContainsRune(bind, ',') {
			return nil, fmt.Errorf("--bind takes a single source IP or interface, not a list (%q); bind an interface for dual-stack", bind)
		}
		if err := b.applySpec(bind, cfg.Family, warn); err != nil {
			return nil, err
		}
	}

	// A bind that covers only one family pins the binder to that family, so the
	// other can't silently dial out the default source — "bind to this source"
	// means exactly that. This covers a single-family address (--bind <v4>) and an
	// interface whose only usable address is one family. A device bind sets control
	// on BOTH families, so it stays dual-stack (the device pin keeps the
	// unsupported family from leaking on its own).
	if b.family == FamilyDual {
		switch {
		case b.v4.set() && !b.v6.set():
			b.family = FamilyV4
		case b.v6.set() && !b.v4.set():
			b.family = FamilyV6
		}
	}
	return b, nil
}

// applySpec resolves the single --bind token into the per-family bindings on b.
func (b *Binder) applySpec(raw string, fam Family, warn func(string)) error {
	kind, val := classifySpec(raw)

	if kind == specAddr {
		addr, err := netip.ParseAddr(val)
		if err != nil {
			return fmt.Errorf("--bind: %q is not a valid IP address", val)
		}
		af := familyOf(addr)
		if (fam == FamilyV4 && af == FamilyV6) || (fam == FamilyV6 && af == FamilyV4) {
			return fmt.Errorf("--family %s conflicts with --bind %s (an %s address)", fam.label(), val, af.label())
		}
		b.byFamily(af).localAddr = tcpAddr(addr)
		return nil
	}

	// Interface spec.
	iface, err := net.InterfaceByName(val)
	if err != nil {
		return fmt.Errorf("--bind: interface %q not found", val)
	}
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("--bind: interface %q is down", val)
	}
	has4, has6 := interfaceFamilies(iface)
	if fam == FamilyV4 && !has4 {
		return fmt.Errorf("--family ipv4 set but interface %q has no IPv4 address", val)
	}
	if fam == FamilyV6 && !has6 {
		return fmt.Errorf("--family ipv6 set but interface %q has no IPv6 address", val)
	}

	if deviceBindSupported() {
		// True device binding covers BOTH families through one control hook (it
		// reads the resolved network to pick the per-family socket option), so a
		// dual-stack interface stays dual-stack and nothing escapes via another
		// route. The wanted families follow --family.
		ctrl := deviceControl(iface)
		if fam != FamilyV6 {
			b.v4.control = ctrl
		}
		if fam != FamilyV4 {
			b.v6.control = ctrl
		}
		return nil
	}

	// Source-IP fallback (device binding unavailable / unprivileged): bind the
	// interface's own source address. Only ONE family's source can be bound this
	// way, so a dual-stack request against an interface that has both families is
	// refused rather than silently leaking one family out the default source — the
	// user must grant privilege for true device binding, or choose a family.
	if fam == FamilyDual && has4 && has6 {
		return fmt.Errorf("--bind %s: binding a dual-stack interface needs elevated privileges here "+
			"(root or CAP_NET_RAW); grant it, bind a source IP address instead (--bind <ip>), "+
			"or restrict to one family with --family ipv4 / ipv6", val)
	}
	// Bind the interface's own source address for the wanted family. If no usable
	// source is found, fail rather than fall through unbound — an unbound binder
	// would dial the default source, defeating --bind.
	v4src := interfaceAddr(iface, FamilyV4)
	v6src := interfaceAddr(iface, FamilyV6)
	useV4 := fam != FamilyV6 && v4src != nil
	useV6 := fam != FamilyV4 && v6src != nil
	if !useV4 && !useV6 {
		return fmt.Errorf("--bind: interface %q has no usable IP address to bind", val)
	}
	warn(fmt.Sprintf("--bind %s: binding to a device needs elevated privileges here; "+
		"using the interface's source IP instead (egress still follows the routing table)", val))
	if useV4 {
		b.v4.localAddr = v4src
	}
	if useV6 {
		b.v6.localAddr = v6src
	}
	return nil
}

func (b *Binder) byFamily(f Family) *famBind {
	if f == FamilyV6 {
		return &b.v6
	}
	return &b.v4
}

// famFor returns the resolved source binding for a family-pinned dial whose
// network is "tcp4" or "tcp6"; an unconfigured family yields the zero famBind
// (OS default source). It feeds FamilyDoer.
func (b *Binder) famFor(network string) famBind {
	if network == "tcp6" {
		return b.v6
	}
	return b.v4
}

// --- spec parsing ---

type specKind int

const (
	specAuto specKind = iota
	specAddr
	specIface
)

// classifySpec applies the curl-style disambiguation prefixes (addr!/host! force
// an address, if! forces an interface name) and otherwise auto-detects: a value
// that parses as an IP is an address, anything else is an interface name.
func classifySpec(raw string) (specKind, string) {
	switch {
	case strings.HasPrefix(raw, "addr!"):
		return specAddr, strings.TrimPrefix(raw, "addr!")
	case strings.HasPrefix(raw, "host!"):
		return specAddr, strings.TrimPrefix(raw, "host!")
	case strings.HasPrefix(raw, "if!"):
		return specIface, strings.TrimPrefix(raw, "if!")
	}
	if _, err := netip.ParseAddr(raw); err == nil {
		return specAddr, raw
	}
	return specIface, raw
}

// --- family helpers ---

func familyOf(a netip.Addr) Family {
	if a.Is4() {
		return FamilyV4
	}
	return FamilyV6
}

// Family returns the resolved IP family (after any dual→single narrowing in
// Resolve). The ip/doctor diagnostics read it to cap their per-family probing to
// what --family permits — no request may go out a family the user excluded.
func (b *Binder) Family() Family { return b.family }

// Networks lists the TCP networks ("tcp4"/"tcp6") this family permits — dual
// permits both. It is the single source for capping per-family probing.
func (f Family) Networks() []string {
	switch f {
	case FamilyV4:
		return []string{"tcp4"}
	case FamilyV6:
		return []string{"tcp6"}
	default:
		return []string{"tcp4", "tcp6"}
	}
}

// permits reports whether a dial of the given TCP network ("tcp4"/"tcp6") is
// allowed by this family — dual permits both, a pinned family only its own. It
// is the no-leak gate FamilyDoer applies so an excluded family can never be
// dialed out the default source.
func (f Family) permits(network string) bool {
	switch f {
	case FamilyV4:
		return network != "tcp6"
	case FamilyV6:
		return network != "tcp4"
	default:
		return true
	}
}

func (f Family) label() string {
	switch f {
	case FamilyV4:
		return "ipv4"
	case FamilyV6:
		return "ipv6"
	default:
		return "dualstack"
	}
}

func tcpAddr(a netip.Addr) *net.TCPAddr {
	return &net.TCPAddr{IP: a.AsSlice(), Zone: a.Zone()}
}

// interfaceFamilies reports which IP families the interface has a unicast
// address in.
func interfaceFamilies(iface *net.Interface) (has4, has6 bool) {
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		ip := ipOf(a)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			has4 = true
		} else {
			has6 = true
		}
	}
	return
}

// interfaceAddr picks a source address on the interface for the given family,
// preferring a global unicast address over a link-local one.
func interfaceAddr(iface *net.Interface, fam Family) *net.TCPAddr {
	addrs, _ := iface.Addrs()
	var fallback *net.TCPAddr
	for _, a := range addrs {
		ip := ipOf(a)
		if ip == nil {
			continue
		}
		is4 := ip.To4() != nil
		if (fam == FamilyV4) != is4 {
			continue
		}
		ta := &net.TCPAddr{IP: ip}
		if ip.IsLinkLocalUnicast() {
			if fam == FamilyV6 {
				ta.Zone = iface.Name
			}
			if fallback == nil {
				fallback = ta
			}
			continue
		}
		if ip.IsGlobalUnicast() {
			return ta
		}
		if fallback == nil {
			fallback = ta
		}
	}
	return fallback
}

func ipOf(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}
