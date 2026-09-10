// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/keys"
)

// TestLegacyHomeEnvIsHonored: an installation whose shell profile or CI job
// still exports the legacy KORBIT_CLI_HOME keeps resolving to that home. The
// canonical variable is blanked here because the shared harness always sets it.
func TestLegacyHomeEnvIsHonored(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home) // bound default key "bot"

	out, stderr, code := runCLI([]string{"key", "list", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": "", "KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("key list exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"name":"bot"`) {
		t.Fatalf("legacy home env ignored: %s", out)
	}
}

// TestCurrentHomeEnvWinsOverLegacy: with both spellings exported, the canonical
// DIGITALX_CLI_HOME decides — a machine mid-migration must not silently read a
// different home than the one the user set today.
func TestCurrentHomeEnvWinsOverLegacy(t *testing.T) {
	current, legacy := t.TempDir(), t.TempDir()
	seedBoundKey(t, current)
	m := keys.NewManager(legacy, "file", func() int64 { return 1700000000000 }, nil)
	if _, err := m.Add("legacy-only", "", ""); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runCLI([]string{"key", "list", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": current, "KORBIT_CLI_HOME": legacy}, &stubDoer{})
	if code != 0 {
		t.Fatalf("key list exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"name":"bot"`) || strings.Contains(out, `"name":"legacy-only"`) {
		t.Fatalf("canonical home env must win: %s", out)
	}
}

// TestLegacyBaseURLEnvIsHonored: the fallback covers every variable, not just
// the home — here the base-URL override, read on the signing path.
func TestLegacyBaseURLEnvIsHonored(t *testing.T) {
	home := t.TempDir()
	const base = "https://api-legacy.example.test"
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"serverTime":1700000000000}}`, nil)}

	_, stderr, code := runCLI([]string{"time", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home, "KORBIT_CLI_BASE_URL": base}, doer)
	if code != 0 {
		t.Fatalf("time exit = %d — %s", code, stderr)
	}
	if doer.last == nil || doer.last.URL.Host != "api-legacy.example.test" {
		t.Fatalf("legacy base-url env ignored: %v", doer.last)
	}
}
