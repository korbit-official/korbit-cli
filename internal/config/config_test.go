// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setUserHome points os.UserHomeDir at a fresh temp directory and returns it,
// so the Home branches that probe ~ never see the developer's real home.
func setUserHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)        // unix
	t.Setenv("USERPROFILE", dir) // windows
	return dir
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestHome(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		setUserHome(t)
		got := Home(env(map[string]string{"DIGITALX_CLI_HOME": "/tmp/custom"}))
		if got != "/tmp/custom" {
			t.Fatalf("Home = %q", got)
		}
	})

	t.Run("legacy env accepted", func(t *testing.T) {
		setUserHome(t)
		got := Home(env(map[string]string{"KORBIT_CLI_HOME": "/tmp/legacy"}))
		if got != "/tmp/legacy" {
			t.Fatalf("Home = %q", got)
		}
	})

	t.Run("current env wins over legacy", func(t *testing.T) {
		setUserHome(t)
		got := Home(env(map[string]string{
			"DIGITALX_CLI_HOME": "/tmp/custom",
			"KORBIT_CLI_HOME":   "/tmp/legacy",
		}))
		if got != "/tmp/custom" {
			t.Fatalf("Home = %q", got)
		}
	})

	t.Run("existing current directory", func(t *testing.T) {
		home := setUserHome(t)
		mkdir(t, filepath.Join(home, DirName))
		mkdir(t, filepath.Join(home, LegacyDirName))
		if got, want := Home(env(nil)), filepath.Join(home, DirName); got != want {
			t.Fatalf("Home = %q, want %q", got, want)
		}
	})

	t.Run("existing legacy directory", func(t *testing.T) {
		home := setUserHome(t)
		mkdir(t, filepath.Join(home, LegacyDirName))
		if got, want := Home(env(nil)), filepath.Join(home, LegacyDirName); got != want {
			t.Fatalf("Home = %q, want %q", got, want)
		}
	})

	t.Run("neither directory exists", func(t *testing.T) {
		home := setUserHome(t)
		if got, want := Home(env(nil)), filepath.Join(home, DirName); got != want {
			t.Fatalf("Home = %q, want %q", got, want)
		}
	})

	t.Run("legacy name is a plain file", func(t *testing.T) {
		home := setUserHome(t)
		if err := os.WriteFile(filepath.Join(home, LegacyDirName), []byte("not a home"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, want := Home(env(nil)), filepath.Join(home, DirName); got != want {
			t.Fatalf("Home = %q, want %q", got, want)
		}
	})
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestHomeNormalizesAPinnedRelativePath: a pinned home reaches the CLI as
// whatever the user typed, and the basename of that string is what picks the file
// names inside it (LegacyLayout). Pinned as `.` from inside ~/.korbit-cli, the
// raw spelling has basename `.` — which reads as a current-layout home and starts
// a second set of databases in a directory that already holds one. So the path is
// made absolute first.
func TestHomeNormalizesAPinnedRelativePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), LegacyDirName)
	mkdir(t, dir)
	t.Chdir(dir)

	got := Home(env(map[string]string{"KORBIT_CLI_HOME": "."}))
	if !filepath.IsAbs(got) {
		t.Errorf("Home = %q, want an absolute path", got)
	}
	if !LegacyLayout(got) {
		t.Errorf("LegacyLayout(%q) = false; a home pinned as `.` from inside %s is that directory", got, LegacyDirName)
	}
	// And the raw spelling answers the same way, since LegacyLayout normalizes too.
	if !LegacyLayout(".") {
		t.Error(`LegacyLayout(".") = false from inside a legacy-layout home`)
	}
}

// TestLegacyLayoutFollowsTheFilesystemsCaseRules: on a case-insensitive
// filesystem `.KORBIT-CLI` and `.korbit-cli` are the SAME directory, so both
// spellings must pick the same file names. Where names are case-sensitive they
// are different directories and the exact spelling is what counts.
func TestLegacyLayoutFollowsTheFilesystemsCaseRules(t *testing.T) {
	upper := filepath.Join(t.TempDir(), strings.ToUpper(LegacyDirName))
	mkdir(t, upper)
	sameDir := sameFile(t, upper, filepath.Join(filepath.Dir(upper), LegacyDirName))
	if !sameDir {
		t.Skipf("%s is on a case-sensitive filesystem, where the two spellings are different directories", upper)
	}
	if !LegacyLayout(upper) {
		t.Errorf("LegacyLayout(%q) = false, but it is the same directory as %s on this filesystem", upper, LegacyDirName)
	}
}

// sameFile reports whether two paths name the same directory, which is how the
// case-sensitivity of the filesystem under them is established rather than
// assumed. A lowercase path that does not exist means the names are distinct.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func TestLoadMissingFileDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keystore != "file" || cfg.BaseURL != "" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"keystore":"keychain","baseUrl":"https://api.digitalx.miraeasset.com/"}`)
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keystore != "keychain" {
		t.Fatalf("keystore = %q", cfg.Keystore)
	}
	if cfg.BaseURL != "https://api.digitalx.miraeasset.com" { // trailing slash stripped
		t.Fatalf("baseUrl = %q", cfg.BaseURL)
	}
}

func TestLoadWSBaseURL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"baseUrl":"https://api-test.digitalx.miraeasset.com","wsBaseUrl":"wss://ws-api-test.digitalx.miraeasset.com/"}`)
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WSBaseURL != "wss://ws-api-test.digitalx.miraeasset.com" { // trailing slash stripped
		t.Fatalf("wsBaseUrl = %q", cfg.WSBaseURL)
	}
}

func TestLoadRejections(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"keystore":"redis"}`,
		`{"baseUrl":"ftp://x"}`,
		`{"baseUrl":123}`,
		`{"wsBaseUrl":"https://not-ws.test"}`,
		`{"wsBaseUrl":123}`,
	} {
		dir := t.TempDir()
		write(t, dir, body)
		if _, err := Load(dir, nil); err == nil {
			t.Fatalf("expected error for %q", body)
		}
	}
}

func TestLoadTUIColorScheme(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"tui":{"colorScheme":"red-blue"}}`)
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUIColorScheme != "red-blue" {
		t.Fatalf("TUIColorScheme = %q", cfg.TUIColorScheme)
	}
}

func TestLoadTUIEmptyBlockOK(t *testing.T) {
	// An empty tui block (or one with only unrelated keys) is valid and leaves
	// both prefs unset.
	dir := t.TempDir()
	write(t, dir, `{"tui":{}}`)
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUIColorScheme != "" || cfg.TUIOrderLevels != nil {
		t.Fatalf("empty tui block set prefs: %+v", cfg)
	}
}

func TestLoadTUIRejectsInvalid(t *testing.T) {
	// The tui block fails early like every other field — a bad value is a
	// ConfigError, not silently defaulted.
	for _, body := range []string{
		`{"tui":"nope"}`,                         // block not an object
		`{"tui":{"colorScheme":"chartreuse"}}`,   // unknown scheme
		`{"tui":{"colorScheme":42}}`,             // scheme wrong type
		`{"tui":{"orderLevels":[10,25,50,101]}}`, // level out of range
		`{"tui":{"orderLevels":[0,25]}}`,         // level below 1
		`{"tui":{"orderLevels":[]}}`,             // empty list
		`{"tui":{"orderLevels":[10,25,50,"x"]}}`, // wrong element type
		`{"tui":{"orderLevels":42}}`,             // not an array
	} {
		dir := t.TempDir()
		write(t, dir, body)
		if _, err := Load(dir, nil); err == nil {
			t.Fatalf("expected error for %q", body)
		}
	}
}

func TestLoadTUIOrderLevels(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"tui":{"orderLevels":[5,10,20,50,100]}}`)
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{5, 10, 20, 50, 100}
	if len(cfg.TUIOrderLevels) != len(want) {
		t.Fatalf("TUIOrderLevels = %v", cfg.TUIOrderLevels)
	}
	for i, v := range want {
		if cfg.TUIOrderLevels[i] != v {
			t.Fatalf("TUIOrderLevels = %v", cfg.TUIOrderLevels)
		}
	}
}

func TestSetTUIColorScheme(t *testing.T) {
	dir := t.TempDir()
	if err := SetTUIColorScheme(dir, "red-blue", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil || cfg.TUIColorScheme != "red-blue" {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
	// Toggling back round-trips.
	if err := SetTUIColorScheme(dir, "green-red", nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(dir, nil)
	if cfg.TUIColorScheme != "green-red" {
		t.Fatalf("after toggle back: %q", cfg.TUIColorScheme)
	}
}

func TestSetTUIColorSchemeRejectsUnknown(t *testing.T) {
	if err := SetTUIColorScheme(t.TempDir(), "chartreuse", nil); err == nil {
		t.Fatal("expected error for unknown scheme")
	}
}

func TestSetTUIColorSchemePreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	// Top-level fields (incl. an unknown one) and a sibling key inside "tui" must
	// all survive persisting colorScheme.
	write(t, dir, `{"keystore":"keychain","baseUrl":"https://api.digitalx.miraeasset.com","futureField":42,"tui":{"layout":"wide"}}`)
	if err := SetTUIColorScheme(dir, "red-blue", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keystore != "keychain" || cfg.BaseURL != "https://api.digitalx.miraeasset.com" || cfg.TUIColorScheme != "red-blue" {
		t.Fatalf("fields not preserved: %+v", cfg)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	for _, want := range []string{`"futureField": 42`, `"layout": "wide"`, `"colorScheme": "red-blue"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("expected %s in %s", want, raw)
		}
	}
}

func TestSetTUIOrderLevels(t *testing.T) {
	dir := t.TempDir()
	if err := SetTUIOrderLevels(dir, []int{10, 25, 50, 100}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TUIOrderLevels) != 4 || cfg.TUIOrderLevels[3] != 100 {
		t.Fatalf("TUIOrderLevels = %v", cfg.TUIOrderLevels)
	}
}

func TestSetTUIOrderLevelsRejectsInvalid(t *testing.T) {
	for _, bad := range [][]int{nil, {}, {0}, {101}, {10, 25, 50, 60, 70, 80, 90, 95, 99, 100}} {
		if err := SetTUIOrderLevels(t.TempDir(), bad, nil); err == nil {
			t.Fatalf("expected error for %v", bad)
		}
	}
}

// TestSetTUIPrefsCoexist: color scheme and order levels share the "tui" object
// without clobbering each other.
func TestSetTUIPrefsCoexist(t *testing.T) {
	dir := t.TempDir()
	if err := SetTUIColorScheme(dir, "red-blue", nil); err != nil {
		t.Fatal(err)
	}
	if err := SetTUIOrderLevels(dir, []int{10, 25, 50, 100}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUIColorScheme != "red-blue" || len(cfg.TUIOrderLevels) != 4 {
		t.Fatalf("prefs did not coexist: %+v", cfg)
	}
}

func TestSetTUIColorSchemeReplacesMalformedBlock(t *testing.T) {
	dir := t.TempDir()
	// A "tui" that isn't an object can't be merged into — it is replaced so the
	// write still succeeds rather than wedging every future write.
	write(t, dir, `{"tui":"garbage"}`)
	if err := SetTUIColorScheme(dir, "red-blue", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil || cfg.TUIColorScheme != "red-blue" {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
}

func TestSetKeystorePreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	// A config with baseUrl plus a field this version doesn't model.
	write(t, dir, `{"keystore":"file","baseUrl":"https://api.digitalx.miraeasset.com","futureField":42}`)

	if err := SetKeystore(dir, "keychain", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keystore != "keychain" {
		t.Fatalf("keystore = %q", cfg.Keystore)
	}
	if cfg.BaseURL != "https://api.digitalx.miraeasset.com" {
		t.Fatalf("baseUrl not preserved: %q", cfg.BaseURL)
	}
	// The unknown field must survive the round-trip untouched.
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if !strings.Contains(string(raw), `"futureField": 42`) {
		t.Fatalf("unknown field dropped: %s", raw)
	}
}

func TestSetKeystoreCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := SetKeystore(dir, "keychain", nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir, nil)
	if err != nil || cfg.Keystore != "keychain" {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
}

func TestSetKeystoreRejectsUnknown(t *testing.T) {
	if err := SetKeystore(t.TempDir(), "redis", nil); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
