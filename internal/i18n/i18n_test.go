// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package i18n

import (
	"testing"
)

// restoreDefault re-activates the default language after a test that switched
// it — the active table is package state shared across tests.
func restoreDefault(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := Activate(DefaultLang); err != nil {
			t.Fatalf("restore default: %v", err)
		}
	})
}

func TestActivateAndFallback(t *testing.T) {
	restoreDefault(t)

	if got := Active(); got != DefaultLang {
		t.Fatalf("initial Active() = %q, want %q", got, DefaultLang)
	}
	// Unknown language: error, active unchanged.
	if err := Activate("fr"); err == nil {
		t.Fatal("Activate(fr) succeeded, want error")
	}
	if got := Active(); got != DefaultLang {
		t.Fatalf("Active() after failed Activate = %q, want %q", got, DefaultLang)
	}
	// Empty input is "not chosen" → default. No OS-locale consultation.
	if err := Activate(""); err != nil {
		t.Fatalf("Activate(\"\"): %v", err)
	}
	if got := Active(); got != DefaultLang {
		t.Fatalf("Active() after Activate(\"\") = %q, want %q", got, DefaultLang)
	}

	if err := Activate("ko"); err != nil {
		t.Fatalf("Activate(ko): %v", err)
	}
	// An unregistered key renders as itself (English fallback), with args.
	if got := T("no such key %s", "x"); got != "no such key x" {
		t.Fatalf("fallback T = %q", got)
	}
}

func TestDetect(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
		wantErr  bool
	}{
		{name: "explicit wins over locale", explicit: "en", env: map[string]string{"LANG": "ko_KR.UTF-8"}, want: "en"},
		{name: "explicit ko", explicit: "ko", want: "ko"},
		{name: "explicit unsupported errors", explicit: "fr", wantErr: true},
		{name: "LANG korean", env: map[string]string{"LANG": "ko_KR.UTF-8"}, want: "ko"},
		{name: "LANG english", env: map[string]string{"LANG": "en_US.UTF-8"}, want: "en"},
		{name: "LC_ALL takes precedence", env: map[string]string{"LC_ALL": "ko_KR", "LANG": "en_US"}, want: "ko"},
		{name: "unsupported locale falls back to default", env: map[string]string{"LANG": "ja_JP.UTF-8"}, want: DefaultLang},
		{name: "C locale is treated as absent", env: map[string]string{"LANG": "C"}, want: DefaultLang},
		{name: "no locale at all", want: DefaultLang},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(tc.explicit, env(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Detect(%q) succeeded, want error", tc.explicit)
				}
				return
			}
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Detect(%q, %v) = %q, want %q", tc.explicit, tc.env, got, tc.want)
			}
		})
	}
}

// localeToCode is the platform-agnostic core the Windows path feeds its
// preference-ordered UI languages into (GetUserPreferredUILanguages returns a
// list); the best-supported match across the list wins, most preferred first.
func TestLocaleToCode(t *testing.T) {
	cases := []struct {
		name    string
		locales []string
		want    string
	}{
		{name: "nil", locales: nil, want: DefaultLang},
		{name: "single korean", locales: []string{"ko-KR"}, want: "ko"},
		{name: "single english", locales: []string{"en-US"}, want: "en"},
		{name: "first unsupported, second supported", locales: []string{"ja-JP", "ko-KR"}, want: "ko"},
		{name: "preference order honored", locales: []string{"ko-KR", "en-US"}, want: "ko"},
		{name: "none supported", locales: []string{"ja-JP", "zh-CN"}, want: DefaultLang},
		{name: "unparseable skipped", locales: []string{"not-a-tag!!", "ko"}, want: "ko"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := localeToCode(tc.locales); got != tc.want {
				t.Fatalf("localeToCode(%v) = %q, want %q", tc.locales, got, tc.want)
			}
		})
	}
}

// Detect resolves; it must not change the active language. Only a caller's
// Activate does — the tui.
func TestDetectDoesNotActivate(t *testing.T) {
	restoreDefault(t)
	if _, err := Detect("ko", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if got := Active(); got != DefaultLang {
		t.Fatalf("Detect changed the active language to %q", got)
	}
}

// Resolve records the detected language and validates the explicit value, but
// must not activate — only a localizing surface activates via Activate(Detected()).
func TestResolveRecordsDetectedWithoutActivating(t *testing.T) {
	restoreDefault(t)
	noEnv := func(string) string { return "" }
	t.Cleanup(func() { _, _ = Resolve("", noEnv) }) // reset detected to default

	got, err := Resolve("ko", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ko" || Detected() != "ko" {
		t.Fatalf("Resolve/Detected = %q/%q, want ko/ko", got, Detected())
	}
	if Active() != DefaultLang {
		t.Fatalf("Resolve activated the language: Active() = %q", Active())
	}

	// An invalid explicit value errors and leaves the detected language unchanged.
	if _, err := Resolve("fr", noEnv); err == nil {
		t.Fatal("Resolve(fr) succeeded, want error")
	}
	if Detected() != "ko" {
		t.Fatalf("failed Resolve changed detected to %q", Detected())
	}
}
