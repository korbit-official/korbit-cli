// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

var kebabRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// globalFlags mirrors the spec's global-flag names that a command flag must not
// shadow. Kept as a literal here so the catalog guard is independent of spec
// internals.
var globalFlags = map[string]bool{
	"help": true, "key": true, "base-url": true, "ws-base-url": true,
	"timeout": true, "time-sync": true, "retry-timeout": true,
	"no-reconcile": true, "dry-run": true, "json": true, "compact": true,
	"debug": true, "version": true,
}

// expectedCatalogKeys is the closed set of endpoint operations the catalog must
// cover — the structural replacement for string-keyed routing. It is the source
// of truth for the count; an endpoint with no operation simply is not here.
var expectedCatalogKeys = []string{
	// market (public)
	"ticker", "orderbook", "trades", "candles", "pairs", "ticksize", "currencies", "time",
	// orders (signed)
	"order place", "order get", "order cancel", "order open", "order history", "fills",
	// account (signed)
	"balance", "fees", "whoami",
	// crypto deposits (signed)
	"deposit addresses", "deposit address", "deposit generate", "deposit history", "deposit status",
	// crypto withdrawals (signed)
	"withdraw addresses", "withdraw amount", "withdraw request", "withdraw cancel", "withdraw history", "withdraw status",
	// KRW (signed)
	"krw deposit request", "krw withdraw request", "krw deposit history", "krw withdraw history",
}

func TestCatalogCoversExactlyTheExpectedEndpoints(t *testing.T) {
	cat := Catalog()
	if len(cat) != len(expectedCatalogKeys) {
		t.Fatalf("expected %d operations, got %d", len(expectedCatalogKeys), len(cat))
	}
	want := map[string]bool{}
	for _, k := range expectedCatalogKeys {
		want[k] = true
	}
	if len(want) != len(expectedCatalogKeys) {
		t.Fatalf("expectedCatalogKeys has %d unique entries but %d total — duplicate key", len(want), len(expectedCatalogKeys))
	}
	got := map[string]bool{}
	for _, op := range cat {
		got[op.Meta().Key()] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("catalog is missing expected operation %q", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("catalog has unexpected operation %q", k)
		}
	}
}

func TestCatalogIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range Catalog() {
		k := op.Meta().Key()
		if k == "" {
			t.Fatalf("operation with empty key: %v", op.Meta().ID)
		}
		if seen[k] {
			t.Fatalf("duplicate operation key %q", k)
		}
		seen[k] = true
	}
}

func TestCatalogMetaWellFormed(t *testing.T) {
	for _, op := range Catalog() {
		m := op.Meta()
		key := m.Key()
		if m.Summary == "" {
			t.Errorf("%s: empty Summary", key)
		}
		if m.Safety == "" {
			t.Errorf("%s: Safety not set", key)
		}
		switch m.Safety {
		case cmdmeta.SafetyReadOnly, cmdmeta.SafetyIdempotent, cmdmeta.SafetyNonIdempotent:
		default:
			t.Errorf("%s: unknown Safety %q", key, m.Safety)
		}
		switch m.Method {
		case "GET", "POST", "DELETE":
		default:
			t.Errorf("%s: invalid Method %q", key, m.Method)
		}
		// A GET is read-only; a non-GET is a write. This keeps the derived
		// retry policy and the published method aligned.
		if (m.Method == "GET") != (m.Safety == cmdmeta.SafetyReadOnly) {
			t.Errorf("%s: Method %q and Safety %q disagree on read-vs-write", key, m.Method, m.Safety)
		}
		for _, p := range m.Params {
			if !kebabRE.MatchString(p.Flag) {
				t.Errorf("%s: flag --%s is not kebab-case", key, p.Flag)
			}
			if globalFlags[p.Flag] {
				t.Errorf("%s: flag --%s shadows a global flag", key, p.Flag)
			}
			if p.Kind == cmdmeta.KindEnum && len(p.EnumValues) == 0 {
				t.Errorf("%s: --%s is enum but declares no values", key, p.Flag)
			}
			if p.Default != "" && p.Kind == cmdmeta.KindEnum {
				ok := false
				for _, e := range p.EnumValues {
					if e == p.Default {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s: --%s default %q not in enum", key, p.Flag, p.Default)
				}
			}
			// Defaults self-validate against the param's own rules.
			if p.Default != "" {
				if _, err := cmdmeta.NormalizeValue(p, p.Default, "--"+p.Flag); err != nil {
					t.Errorf("%s: --%s default %q does not self-validate: %v", key, p.Flag, p.Default, err)
				}
			}
		}
	}
}

// TestCatalogSafetyClassification pins the safety class of the money-movers and
// the idempotent writes — the basis for the single-shot vs auto-retry policy.
func TestCatalogSafetyClassification(t *testing.T) {
	want := map[string]cmdmeta.Safety{
		"order place":          cmdmeta.SafetyNonIdempotent,
		"withdraw request":     cmdmeta.SafetyNonIdempotent,
		"krw deposit request":  cmdmeta.SafetyNonIdempotent,
		"krw withdraw request": cmdmeta.SafetyNonIdempotent,
		"order cancel":         cmdmeta.SafetyIdempotent,
		"withdraw cancel":      cmdmeta.SafetyIdempotent,
		"deposit generate":     cmdmeta.SafetyIdempotent,
		"ticker":               cmdmeta.SafetyReadOnly,
		"balance":              cmdmeta.SafetyReadOnly,
	}
	for key, ws := range want {
		op := Find(strings.Fields(key)...)
		if op == nil {
			t.Fatalf("operation %q not in catalog", key)
		}
		if got := op.Meta().Safety; got != ws {
			t.Errorf("%s: Safety = %q, want %q", key, got, ws)
		}
	}
}

// TestCatalogDestructiveHints pins the MCP destructive set: the four
// money-movers plus the two cancels (never a read).
func TestCatalogDestructiveHints(t *testing.T) {
	destructive := map[string]bool{
		"order place": true, "order cancel": true,
		"withdraw request": true, "withdraw cancel": true,
		"krw deposit request": true, "krw withdraw request": true,
	}
	for _, op := range Catalog() {
		m := op.Meta()
		if got := m.Destructive; got != destructive[m.Key()] {
			t.Errorf("%s: Destructive = %v, want %v", m.Key(), got, destructive[m.Key()])
		}
	}
}

// TestCatalogAuthMatchesSafety: every signed op has Auth set; every public op
// (the read-only market data) has nil Auth.
func TestCatalogAuthShape(t *testing.T) {
	public := map[string]bool{
		"ticker": true, "orderbook": true, "trades": true, "candles": true,
		"pairs": true, "ticksize": true, "currencies": true, "time": true,
	}
	for _, op := range Catalog() {
		m := op.Meta()
		if public[m.Key()] {
			if m.Auth != nil {
				t.Errorf("%s: public op must have nil Auth", m.Key())
			}
		} else if m.Auth == nil {
			t.Errorf("%s: signed op must have non-nil Auth", m.Key())
		}
	}
}

// loadPublicSpec returns the public apidocs.yaml text, or "" when absent (a
// standalone/published checkout) so the caller can skip — mirroring the spec
// registry test's guard.
func loadPublicSpec() string {
	if b, err := os.ReadFile("../../../docs/en/rest_api/apidocs.yaml"); err == nil {
		return string(b)
	}
	return ""
}

func TestCatalogPathsMatchPublicSpec(t *testing.T) {
	yaml := loadPublicSpec()
	if yaml == "" {
		t.Skip("public apidocs.yaml not found (standalone checkout) — skipping drift cross-check")
	}
	for _, op := range Catalog() {
		m := op.Meta()
		if m.Path == "" {
			t.Errorf("%s: operation declares no Method/Path", m.Key())
			continue
		}
		if !strings.Contains(yaml, m.Path) {
			t.Errorf("%s: path %s not found in public apidocs.yaml (spec drift?)", m.Key(), m.Path)
		}
	}
}

// responseFieldNamesRE matches a `name: <field>` line (a YAML mapping key
// "name", optionally as the first key of a list item "- name:"), capturing the
// field name. It is deliberately lenient about quoting.
var responseFieldNamesRE = regexp.MustCompile(`^\s*(?:- )?name:\s*"?([A-Za-z0-9_]+)"?\s*$`)

// publicSpecResponseFieldNames returns the set of every field name declared
// inside a `response:` block anywhere in the public apidocs.yaml. It is a
// name-existence check, not a per-endpoint structural one: the public spec
// defines response fields once and reuses them across endpoints via YAML
// anchors, so resolving an alias to a specific endpoint would need a real YAML
// parser (a dependency this single-binary CLI avoids). The guard still catches
// the real drift risk — a hint naming a field the spec no longer has.
func publicSpecResponseFieldNames(yaml string) map[string]bool {
	names := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(yaml))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	inResponse := false
	responseIndent := 0
	indentOf := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := indentOf(line)
		if inResponse {
			if ind <= responseIndent && !strings.HasPrefix(trimmed, "- ") && strings.HasSuffix(strings.SplitN(trimmed, " ", 2)[0], ":") {
				inResponse = false
			}
		}
		if !inResponse && (trimmed == "response:" || strings.HasPrefix(trimmed, "response:")) {
			inResponse = true
			responseIndent = ind
			continue
		}
		if inResponse {
			if m := responseFieldNamesRE.FindStringSubmatch(line); m != nil {
				names[m[1]] = true
			}
		}
	}
	return names
}

// TestCatalogResponseHintsMatchPublicSpec is the drift guard for the per-command
// response-shape hints: every field name an operation's OpMeta.Response declares
// must appear in a response block of the public apidocs.yaml, so the
// machine-readable hints cannot drift from the spec the API returns.
func TestCatalogResponseHintsMatchPublicSpec(t *testing.T) {
	yaml := loadPublicSpec()
	if yaml == "" {
		t.Skip("public apidocs.yaml not found (standalone checkout) — skipping response-hint drift cross-check")
	}
	names := publicSpecResponseFieldNames(yaml)
	if len(names) == 0 {
		t.Fatal("parsed no response field names from public apidocs.yaml — parser or spec format changed")
	}
	for _, op := range Catalog() {
		m := op.Meta()
		for _, rf := range m.Response {
			if !names[rf.Name] {
				t.Errorf("%s: response hint field %q not found in any response block of public apidocs.yaml (hint drift?)", m.Key(), rf.Name)
			}
		}
	}
}
