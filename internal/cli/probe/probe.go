// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package probe holds the endpoint-soundness and reachability primitives the
// cli's onboarding and diagnostic commands share (ip, doctor, setup, key
// set-base-url, mcp): base-URL validation, the public-IP probe and its
// allowlist report, the REST/WebSocket reachability smoke test, and the
// IP-allowlist error classifier. It carries no command logic — each command
// composes these — so a command subpackage can reach them without importing
// cli.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/useragent"
)

const (
	// ProdBaseURL is the production REST host — the single source for the
	// production endpoint (the base-URL fallback and the always-production IP
	// probe both resolve to it).
	ProdBaseURL = "https://api.korbit.co.kr"

	// PortalURL is the developers portal — the single source for the URL where
	// users register a public key and manage a key's IP allowlist. The key-setup
	// link, the `ip` guidance, and doctor's allowlist fixes all point at it.
	PortalURL = "https://developers.korbit.co.kr"

	// DefaultTimeoutMs is the default per-probe timeout. It is shorter than the
	// regular request timeout: an unreachable target must fail fast (an
	// IPv6-less host during `setup`, a wrong WebSocket host in `doctor`) instead
	// of stalling the command. Overridable with --timeout.
	DefaultTimeoutMs = 5000
)

// ---- base-URL soundness ----

// ValidateBaseURL rejects a malformed base URL, and — when the request will be
// signed — refuses to send credentials over plaintext http to a non-loopback
// host (which would expose a replayable signed request). http to localhost is
// allowed for the sandbox.
func ValidateBaseURL(raw string, signed bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return output.Usagef("--base-url must be an http(s) URL (got %q)", raw)
	}
	if signed && u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) {
		return output.Usagef(
			"refusing to send a signed request over plaintext http to non-local host %q — use https (http is allowed only for a local sandbox)",
			u.Host)
	}
	return nil
}

// WSURLsFor turns a WebSocket base URL into the public and private channel URLs
// the stream layer dials.
func WSURLsFor(wsBase string) (publicURL, privateURL string) {
	wsBase = strings.TrimRight(wsBase, "/")
	return wsBase + "/v2/public", wsBase + "/v2/private"
}

// DeriveWSBaseURL maps a REST base URL to the matching WebSocket base URL using
// the convention that the WebSocket endpoint lives on a sibling `ws-api` host of
// the REST host. The transform:
//
//   - scheme: http → ws, https → wss.
//   - first DNS label: when it begins with "api" (matched case-insensitively),
//     everything from "api" up to the first "-" is replaced by "ws-api" and any
//     "-suffix" is kept — so a node/cluster discriminator after "api" (a digit,
//     letter, or word) is dropped. A label that does not begin with "api" (an
//     IP, "localhost", or any other host) is left unchanged, so a same-host
//     deployment (the local sandbox) keeps its host.
//   - the rest of the host (remaining labels and any :port) is preserved; an
//     IPv6 literal host is left intact and re-bracketed.
//
// Examples (illustrative hosts):
//
//	https://api.korbit.co.kr      → wss://ws-api.korbit.co.kr
//	https://api-test.korbit.co.kr → wss://ws-api-test.korbit.co.kr
//	https://apiz.korbit.com       → wss://ws-api.korbit.com
//	https://api7-test.korbit.com  → wss://ws-api-test.korbit.com
//	http://127.0.0.1:9999         → ws://127.0.0.1:9999
//
// The result is a base URL with no path; callers append /v2/public and
// /v2/private. It returns "" only when raw cannot be parsed into a host — the
// caller then falls back to its own default. The heuristic is best-effort: a
// deployment whose WebSocket host does not follow this convention can pin an
// exact endpoint with the explicit WS base URL override.
func DeriveWSBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}

	host := u.Hostname() // strips any brackets from an IPv6 literal
	// An IPv6 literal has no DNS labels and can't begin with "api": leave it
	// intact and re-bracket it below so the result stays a valid URL host.
	isIPv6 := strings.Contains(host, ":")
	newHost := host
	if !isIPv6 {
		label, rest := host, ""
		if dot := strings.IndexByte(host, '.'); dot >= 0 {
			label, rest = host[:dot], host[dot:] // rest keeps the leading dot
		}
		if strings.HasPrefix(strings.ToLower(label), "api") {
			suffix := ""
			if dash := strings.IndexByte(label, '-'); dash >= 0 {
				suffix = label[dash:] // keep "-test", …
			}
			label = "ws-api" + suffix
		}
		newHost = label + rest
	} else {
		newHost = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		newHost += ":" + port
	}
	return scheme + "://" + newHost
}

// ValidateWSBaseURL rejects a malformed WebSocket base URL, and — when the
// connection will be signed (a private channel upgrade) — refuses to sign over a
// plaintext ws:// to a non-loopback host, which would expose a replayable signed
// upgrade. ws:// to localhost is allowed for the sandbox. label names the source
// of the value for the error message (e.g. "--ws-base-url").
func ValidateWSBaseURL(raw, label string, signed bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return output.Usagef("%s must be a ws(s) URL (got %q)", label, raw)
	}
	if signed && u.Scheme == "ws" && !IsLoopbackHost(u.Hostname()) {
		return output.Usagef(
			"refusing to sign a private WebSocket upgrade over plaintext ws to non-local host %q — use wss (ws is allowed only for a local sandbox)",
			u.Host)
	}
	return nil
}

// IsLoopbackHost reports whether host is localhost or a loopback IP.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- public-IP probe ----

// Report is the result of probing Korbit over both families. Each address is
// null when that family is unavailable. Allowlist is the ready-to-paste set for
// the portal's API-key IP allowlist: the full IPv4 address (a /32) and the IPv6
// /64 prefix.
type Report struct {
	IPv4         *string  `json:"ipv4"`
	IPv6         *string  `json:"ipv6"`
	IPv6Prefix64 *string  `json:"ipv6Prefix64"`
	Allowlist    []string `json:"allowlistEntries"`
}

// Any reports whether either family's address was determined.
func (r Report) Any() bool { return r.IPv4 != nil || r.IPv6 != nil }

// EntryFor returns the ready-to-paste allowlist entry for a family label
// ("IPv4"/"IPv6"): the full IPv4 address (a /32), or the IPv6 /64 prefix (the
// full address when the prefix couldn't be derived). It returns "" when that
// family's address wasn't determined.
func (r Report) EntryFor(label string) string {
	switch label {
	case "IPv4":
		if r.IPv4 != nil {
			return *r.IPv4
		}
	case "IPv6":
		if r.IPv6Prefix64 != nil {
			return *r.IPv6Prefix64
		}
		if r.IPv6 != nil {
			return *r.IPv6
		}
	}
	return ""
}

// IPs queries baseURL over the given TCP families ("tcp4"/"tcp6") concurrently
// (each best-effort) and assembles the allowlist suggestions. networks caps the
// probe to what --family permits — a family not listed is never contacted, so an
// --family ipv6 run makes no IPv4 request and suggests no IPv4 allowlist entry. A
// listed family that fails to connect is simply absent from the report.
func IPs(prober korbit.IPProber, baseURL string, timeoutMs int, networks []string) Report {
	ua := useragent.For(korbit.SurfaceCLI, "ip-probe")
	addrs := make([]string, len(networks))
	oks := make([]bool, len(networks))
	var wg sync.WaitGroup
	for i, nw := range networks {
		wg.Add(1)
		go func(i int, nw string) {
			defer wg.Done()
			addrs[i], oks[i] = probeFamily(prober, nw, baseURL, ua, timeoutMs)
		}(i, nw)
	}
	wg.Wait()

	var rep Report
	for i, nw := range networks {
		if !oks[i] {
			continue
		}
		addr := addrs[i]
		if nw == "tcp4" {
			rep.IPv4 = &addr
			rep.Allowlist = append(rep.Allowlist, addr)
		} else {
			rep.IPv6 = &addr
			if p, ok := prefix64(addr); ok {
				rep.IPv6Prefix64 = &p
				rep.Allowlist = append(rep.Allowlist, p)
			}
		}
	}
	return rep
}

// FamiliesLabel is a human label for a set of probe networks ("IPv4", "IPv6", or
// "IPv4 or IPv6"), for the "could not determine" diagnostics.
func FamiliesLabel(networks []string) string {
	has4, has6 := false, false
	for _, nw := range networks {
		switch nw {
		case "tcp4":
			has4 = true
		case "tcp6":
			has6 = true
		}
	}
	switch {
	case has4 && !has6:
		return "IPv4"
	case has6 && !has4:
		return "IPv6"
	default:
		return "IPv4 or IPv6"
	}
}

// probeFamily fetches and validates one family's address, returning ok=false
// (rather than an error) when the family is unavailable or the response isn't a
// well-formed address of the expected family — the caller reports only what it
// could confirm.
func probeFamily(prober korbit.IPProber, network, baseURL, userAgent string, timeoutMs int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	raw, err := prober(ctx, network, baseURL, userAgent, timeoutMs)
	if err != nil {
		return "", false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	wantV4 := network == "tcp4"
	if wantV4 != addr.Is4() {
		return "", false
	}
	return addr.String(), true
}

// prefix64 returns the canonical IPv6 /64 prefix (e.g. "2001:db8:1:2::/64") for
// a real IPv6 address. The allowlist takes /64 rather than the full /128 so it
// keeps matching as the host's address rotates within its delegated prefix.
func prefix64(ip string) (string, bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return "", false
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return "", false
	}
	return p.String(), true
}

// ---- REST/WebSocket reachability smoke test ----

// EndpointCheck is one endpoint's smoke-test result.
type EndpointCheck struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
	// Err is the underlying transport error when the host could not be reached
	// (Reachable false), nil otherwise. Not serialized — Detail carries the
	// human-readable form; Err lets a caller classify or re-render the failure
	// without parsing Detail.
	Err error `json:"-"`
}

// EndpointVerification is the smoke test `key set-base-url` runs after storing a
// key's endpoints: a best-effort reachability probe of the REST and WebSocket
// base URLs, so the user learns immediately if the (possibly derived) WebSocket
// host is wrong instead of only when a `monitor` later fails to connect.
type EndpointVerification struct {
	REST EndpointCheck `json:"rest"`
	WS   EndpointCheck `json:"ws"`
}

// Endpoints smoke-tests the REST base URL (an unauthenticated GET /v2/time) and
// the WebSocket base URL (a public-channel dial). It is best-effort: every
// outcome is reported in the result, never returned as an error, and a nil doer
// or dialer marks that half "not checked". Each probe gets its OWN timeout
// budget so a slow REST host can't starve the WS probe into a false failure.
func Endpoints(doer korbit.Doer, dial stream.Dialer, baseURL, wsBaseURL string, timeoutMs int) EndpointVerification {
	d := time.Duration(timeoutMs) * time.Millisecond
	restCtx, cancelREST := context.WithTimeout(context.Background(), d)
	defer cancelREST()
	wsCtx, cancelWS := context.WithTimeout(context.Background(), d)
	defer cancelWS()
	return EndpointVerification{
		REST: REST(restCtx, doer, baseURL),
		WS:   WS(wsCtx, dial, wsBaseURL),
	}
}

// REST probes one REST base URL with an unauthenticated GET /v2/time.
func REST(ctx context.Context, doer korbit.Doer, baseURL string) EndpointCheck {
	c := EndpointCheck{URL: baseURL}
	if doer == nil {
		c.Reachable, c.Detail = true, "not checked"
		return c
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v2/time", nil)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", useragent.For(korbit.SurfaceCLI, "probe"))
	resp, err := doer.Do(req)
	if err != nil {
		c.Detail, c.Err = "unreachable: "+err.Error(), err
		return c
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	c.Reachable = true
	if resp.StatusCode/100 == 2 {
		c.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	} else {
		c.Detail = fmt.Sprintf("reachable, but HTTP %d", resp.StatusCode)
	}
	return c
}

// WS probes one WebSocket base URL with a public-channel dial.
func WS(ctx context.Context, dial stream.Dialer, wsBaseURL string) EndpointCheck {
	c := EndpointCheck{URL: wsBaseURL}
	if dial == nil {
		c.Reachable, c.Detail = true, "not checked"
		return c
	}
	pub, _ := WSURLsFor(wsBaseURL)
	hdr := http.Header{}
	hdr.Set("User-Agent", useragent.For(korbit.SurfaceStreamWS, "probe"))
	conn, err := dial(ctx, pub, hdr)
	if err != nil {
		// A refused upgrade still means the host answered: the URL is reachable,
		// just rejecting this (public) handshake — distinct from a wrong host,
		// which fails to connect at all.
		var ue *stream.UpgradeError
		if errors.As(err, &ue) {
			c.Reachable, c.Detail = true, fmt.Sprintf("reachable, but upgrade rejected: HTTP %d", ue.Status)
			return c
		}
		c.Detail, c.Err = "unreachable: "+err.Error(), err
		return c
	}
	_ = conn.Close()
	c.Reachable, c.Detail = true, "connected"
	return c
}

// ---- IP-allowlist error classification ----

// IsIPAllowlistCode reports whether a symbolic API error code denotes an
// IP-allowlist rejection. It matches on word-ish boundaries (`IP_`, `_IP`,
// WHITELIST/ALLOWLIST) rather than a bare "IP" substring, so unrelated codes
// that merely contain the letters "ip" don't get the allowlist fix hint. The
// public spec doesn't pin the exact symbol, so this stays a heuristic — it only
// selects the *fix text*, never the exit code.
func IsIPAllowlistCode(code string) bool {
	up := strings.ToUpper(code)
	return strings.HasPrefix(up, "IP_") || strings.Contains(up, "_IP_") ||
		strings.HasSuffix(up, "_IP") || strings.Contains(up, "WHITELIST") ||
		strings.Contains(up, "ALLOWLIST")
}
