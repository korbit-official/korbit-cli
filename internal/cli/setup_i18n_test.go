// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli/setupui"
	"github.com/korbit-official/korbit-cli/internal/i18n"
)

// resetLang restores the process-global active display language to English after
// a test that activates another one — i18n.Active is a program-wide atomic, so a
// --lang ko run would otherwise leak Korean into a later test that renders text.
func resetLang(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Activate(i18n.DefaultLang) })
}

// pasteKeyCapture is pasteKey plus a capture of the prompt label the UI is handed,
// so a test can assert the interactive chrome localized too (not just the result).
func pasteKeyCapture(token string, gotLines *[]string, gotPrompt *string) func(setupui.Config) error {
	return func(cfg setupui.Config) error {
		*gotPrompt = cfg.Prompt
		lines, _, err := cfg.Submit(token)
		if err != nil {
			return err
		}
		*gotLines = lines
		return nil
	}
}

// TestSetupInteractiveLocalizesKorean: `setup --lang ko` on a TTY renders the whole
// run in Korean — the prompt label, the in-UI health-check status, and the result
// document on stdout — since interactive setup is the one mode that activates the
// detected language.
func TestSetupInteractiveLocalizesKorean(t *testing.T) {
	resetLang(t)
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	var lines []string
	var prompt string
	out, stderr, code := runWithSetupUI([]string{"setup", "--lang", "ko"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""),
		pasteKeyCapture("KEYID-KO", &lines, &prompt))
	if code != 0 {
		t.Fatalf("interactive ko setup must succeed: exit = %d — %s", code, stderr)
	}
	// The prompt label (interactive chrome, on stderr) is Korean.
	if !strings.Contains(prompt, "붙여넣으세요") {
		t.Fatalf("prompt label not localized: %q", prompt)
	}
	// The in-UI health-check status line is Korean.
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "상태 점검") {
		t.Fatalf("in-UI health-check status not localized: %v", lines)
	}
	// The result document on stdout is Korean (configured summary + the complementary
	// health-check framing setup owns), and the pasted id still rides through verbatim.
	if !strings.Contains(out, "설정되었습니다") || !strings.Contains(out, "KEYID-KO") {
		t.Fatalf("stdout result not localized / id missing: %s", out)
	}
	if !strings.Contains(out, "보조 상태 점검") {
		t.Fatalf("complementary health-check framing not localized: %s", out)
	}
}

// TestSetupKoreanStaysEnglishNonInteractive: --lang ko does NOT localize a
// non-interactive run. --no-interactive prints the link and exits without wiring
// the prompt, so the language is never activated and the result stays English —
// the machine-facing contract holds regardless of the host locale or --lang.
func TestSetupKoreanStaysEnglishNonInteractive(t *testing.T) {
	resetLang(t)
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	called := false
	stub := func(setupui.Config) error { called = true; return nil }
	out, stderr, code := runWithSetupUI([]string{"setup", "--lang", "ko", "--no-interactive"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("setup --lang ko --no-interactive must succeed: exit = %d — %s", code, stderr)
	}
	if called {
		t.Fatalf("--no-interactive must not invoke the prompt")
	}
	if !strings.Contains(out, "isn't registered yet") {
		t.Fatalf("non-interactive result must stay English: %s", out)
	}
	if strings.Contains(out, "등록되지 않았습니다") || strings.Contains(out, "다음 단계") {
		t.Fatalf("non-interactive result must not be localized: %s", out)
	}
}

// TestDoctorStaysEnglishWithLangKo: standalone `doctor` never activates a display
// language, so its report — including the localizable problem detail/fix strings —
// stays English even under --lang ko. This guards the active-gating: the same
// i18n.T-wrapped strings render Korean only when a surface (interactive setup)
// activates, and English everywhere else.
func TestDoctorStaysEnglishWithLangKo(t *testing.T) {
	resetLang(t)
	home := t.TempDir() // no keys → a failing "keys" check with a localizable detail
	out, _, _ := runWithDeps([]string{"doctor", "--lang", "ko"},
		map[string]string{"KORBIT_CLI_HOME": home}, healthyWhoami, fakeProbe("203.0.113.7", ""))
	if !strings.Contains(out, "no keys configured") {
		t.Fatalf("standalone doctor must render its problem detail in English: %s", out)
	}
	if strings.Contains(out, "구성된 키가 없습니다") {
		t.Fatalf("standalone doctor must not localize even with --lang ko: %s", out)
	}
}

// TestSetupKoreanStaysEnglishJSON: --json is the machine contract, so --lang ko
// leaves it English (the prompt is suppressed, the language never activated).
func TestSetupKoreanStaysEnglishJSON(t *testing.T) {
	resetLang(t)
	home := t.TempDir()
	seedUnboundKey(t, home, "default")
	called := false
	stub := func(setupui.Config) error { called = true; return nil }
	out, _, code := runWithSetupUI([]string{"setup", "--lang", "ko", "--json"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""), stub)
	if code != 0 {
		t.Fatalf("setup --lang ko --json must succeed: exit = %d", code)
	}
	if called {
		t.Fatalf("--json must not invoke the prompt")
	}
	if !strings.Contains(out, `"status": "awaitingRegistration"`) {
		t.Fatalf("expected the English JSON awaitingRegistration document: %s", out)
	}
}
