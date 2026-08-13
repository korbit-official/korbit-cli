// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli"
)

// fakeProbe returns canned IPs per network family; an empty string means that
// family is unavailable (returns an error, as the real prober would on a failed
// dial).
func fakeProbe(v4, v6 string) func(context.Context, string, string, string, int) (string, error) {
	return func(_ context.Context, network, _, _ string, _ int) (string, error) {
		var ip string
		switch network {
		case "tcp4":
			ip = v4
		case "tcp6":
			ip = v6
		}
		if ip == "" {
			return "", errors.New("no route to host (family unavailable)")
		}
		return ip, nil
	}
}

func runIP(args []string, env map[string]string, probe func(context.Context, string, string, string, int) (string, error)) (string, string, int) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv:  func(k string) string { return env[k] },
		Stdout:  &out,
		Stderr:  &errb,
		Now:     func() int64 { return 1700000000000 },
		IPProbe: probe,
	})
	return out.String(), errb.String(), code
}

func TestIPReportsBothFamilies(t *testing.T) {
	out, stderr, code := runIP([]string{"ip", "--compact"}, nil, fakeProbe("203.0.113.7", "2001:db8:1:2:3:4:5:6"))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	var rep struct {
		IPv4         *string  `json:"ipv4"`
		IPv6         *string  `json:"ipv6"`
		IPv6Prefix64 *string  `json:"ipv6Prefix64"`
		Allowlist    []string `json:"allowlistEntries"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v (%s)", err, out)
	}
	if rep.IPv4 == nil || *rep.IPv4 != "203.0.113.7" {
		t.Fatalf("ipv4 = %v", rep.IPv4)
	}
	if rep.IPv6 == nil || *rep.IPv6 != "2001:db8:1:2:3:4:5:6" {
		t.Fatalf("ipv6 = %v", rep.IPv6)
	}
	if rep.IPv6Prefix64 == nil || *rep.IPv6Prefix64 != "2001:db8:1:2::/64" {
		t.Fatalf("ipv6Prefix64 = %v, want 2001:db8:1:2::/64", rep.IPv6Prefix64)
	}
	want := []string{"203.0.113.7", "2001:db8:1:2::/64"}
	if strings.Join(rep.Allowlist, ",") != strings.Join(want, ",") {
		t.Fatalf("allowlistEntries = %v, want %v", rep.Allowlist, want)
	}
	// `ip` writes nothing to stderr in either mode — the allowlist guidance is part
	// of the result (the structured ipv6Prefix64/allowlistEntries here in --json).
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("ip must produce no stderr: %s", stderr)
	}
	// In human mode that guidance is on stdout and names the /64 prefix.
	textOut, textErr, textCode := runIP([]string{"ip"}, nil, fakeProbe("203.0.113.7", "2001:db8:1:2:3:4:5:6"))
	if textCode != 0 || strings.TrimSpace(textErr) != "" {
		t.Fatalf("text-mode ip: exit=%d stderr=%s", textCode, textErr)
	}
	if !strings.Contains(textOut, "IP allowlist") || !strings.Contains(textOut, "2001:db8:1:2::/64") {
		t.Fatalf("stdout guidance must name the /64 prefix: %s", textOut)
	}
}

// TestIPAlwaysProbesProduction pins that /v2/ip ignores --base-url:
// the IP that matters for the allowlist is the one production sees.
func TestIPAlwaysProbesProduction(t *testing.T) {
	// The two family probes run concurrently; guard the capture.
	var mu sync.Mutex
	var gotBaseURL string
	probe := func(_ context.Context, network, baseURL, _ string, _ int) (string, error) {
		mu.Lock()
		gotBaseURL = baseURL
		mu.Unlock()
		if network == "tcp4" {
			return "203.0.113.7", nil
		}
		return "", errors.New("no v6")
	}
	_, _, code := runIP([]string{"ip", "--base-url", "http://127.0.0.1:1", "--compact"}, nil, probe)
	// (`ip` never resolves --base-url — it always uses production.)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if gotBaseURL != "https://api.korbit.co.kr" {
		t.Fatalf("ip must probe production, got baseURL %q", gotBaseURL)
	}
}

func TestIPv4OnlyOmitsIPv6(t *testing.T) {
	out, _, code := runIP([]string{"ip", "--compact"}, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, `"ipv4":"203.0.113.7"`) || !strings.Contains(out, `"ipv6":null`) {
		t.Fatalf("expected ipv4 set and ipv6 null: %s", out)
	}
	if !strings.Contains(out, `"allowlistEntries":["203.0.113.7"]`) {
		t.Fatalf("allowlist should be just the v4 address: %s", out)
	}
}

// TestIPFamilyCapsProbe pins that --family restricts the IP probe to that family
// only: no request is made over the excluded family, and only its address is
// reported/suggested for the allowlist.
func TestIPFamilyCapsProbe(t *testing.T) {
	probed := func() (map[string]bool, func(context.Context, string, string, string, int) (string, error)) {
		var mu sync.Mutex
		seen := map[string]bool{}
		return seen, func(_ context.Context, network, _, _ string, _ int) (string, error) {
			mu.Lock()
			seen[network] = true
			mu.Unlock()
			if network == "tcp4" {
				return "203.0.113.7", nil
			}
			return "2001:db8:1:2:3:4:5:6", nil
		}
	}

	seen6, p6 := probed()
	out, _, code := runIP([]string{"ip", "--family", "ipv6", "--compact"}, nil, p6)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if seen6["tcp4"] {
		t.Fatal("--family ipv6 must not probe IPv4")
	}
	if !seen6["tcp6"] {
		t.Fatal("--family ipv6 should probe IPv6")
	}
	if !strings.Contains(out, `"ipv4":null`) || strings.Contains(out, "203.0.113.7") {
		t.Fatalf("--family ipv6 report must omit IPv4: %s", out)
	}

	seen4, p4 := probed()
	out, _, code = runIP([]string{"ip", "--family", "ipv4", "--compact"}, nil, p4)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if seen4["tcp6"] {
		t.Fatal("--family ipv4 must not probe IPv6")
	}
	if !strings.Contains(out, `"ipv6":null`) || strings.Contains(out, "2001:db8") {
		t.Fatalf("--family ipv4 report must omit IPv6: %s", out)
	}
}

func TestIPNoConnectivityIsExit1(t *testing.T) {
	out, _, code := runIP([]string{"ip", "--compact"}, nil, fakeProbe("", ""))
	if code != 1 {
		t.Fatalf("no connectivity should be exit 1, got %d", code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout must stay clean on failure: %s", out)
	}
}

func TestSetupSurfacesIPAllowlist(t *testing.T) {
	home := t.TempDir()
	out, stderr, code := runIP([]string{"setup", "--name", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, fakeProbe("203.0.113.7", "2001:db8:1:2:3:4:5:6"))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	// The setup JSON carries the probed allowlist for an agent to act on.
	if !strings.Contains(out, `"ipAllowlist"`) || !strings.Contains(out, `"2001:db8:1:2::/64"`) {
		t.Fatalf("setup output missing ipAllowlist: %s", out)
	}
	// The registration deep link prefills permissions and the probed allowlist.
	if !strings.Contains(out, "developers.korbit.co.kr/manage/create") || !strings.Contains(out, "permissions=47") {
		t.Fatalf("setup output missing registration link: %s", out)
	}
	if !strings.Contains(out, "whitelist=203.0.113.7") {
		t.Fatalf("registration link missing prefilled allowlist: %s", out)
	}
	// The link/guidance is part of the result on stdout (the JSON above); setup
	// writes no narration to stderr in --json mode.
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("setup --compact must write nothing to stderr: %s", stderr)
	}
}

// TestSetupSurvivesIPProbeFailure covers an endpoint that doesn't serve /v2/ip
// (e.g. the sandbox, which 404s — the prober surfaces that as an error, mirrored
// here by fakeProbe("","")). Setup must still succeed and omit the ipAllowlist
// field entirely; the registration link is still produced, just without a
// prefilled whitelist (the portal then autofills the IP).
func TestSetupSurvivesIPProbeFailure(t *testing.T) {
	home := t.TempDir()
	out, stderr, code := runIP([]string{"setup", "--name", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, fakeProbe("", ""))
	if code != 0 {
		t.Fatalf("setup must succeed even when IP probing fails: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, `"publicKey"`) {
		t.Fatalf("expected a generated key: %s", out)
	}
	if strings.Contains(out, "ipAllowlist") {
		t.Fatalf("ipAllowlist must be omitted when no IP could be determined: %s", out)
	}
	if !strings.Contains(out, "developers.korbit.co.kr/manage/create") || !strings.Contains(out, "permissions=47") {
		t.Fatalf("expected a registration link even without an allowlist: %s", out)
	}
	if strings.Contains(out, "whitelist=") {
		t.Fatalf("registration link must omit whitelist when no IP was determined: %s", out)
	}
	_ = stderr
}
