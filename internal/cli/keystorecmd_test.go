// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/keystore"
)

// keyBackend reads the backend a key's record points at (via keys.json).
func keyBackend(t *testing.T, home, name string) string {
	t.Helper()
	m := keys.NewManager(home, "file", nil, nil)
	s, err := m.Show(name)
	if err != nil {
		t.Fatal(err)
	}
	return s.Keystore
}

func TestKeystoreStatusDefault(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home)
	out, errb, code := runCLI([]string{"keystore", "status", "--json"}, map[string]string{"KORBIT_CLI_HOME": home}, nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	var st struct {
		Default  string `json:"default"`
		KeyCount int    `json:"keyCount"`
		Backends []struct {
			Name      string `json:"name"`
			Default   bool   `json:"default"`
			Available bool   `json:"available"`
			KeyCount  int    `json:"keyCount"`
		} `json:"backends"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if st.Default != "file" || st.KeyCount != 1 {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(st.Backends))
	}
	for _, b := range st.Backends {
		if !b.Available {
			t.Fatalf("backend %q reported unavailable (keyring is mocked)", b.Name)
		}
		if (b.Name == "file") != b.Default {
			t.Fatalf("default flag wrong for %q: %+v", b.Name, b)
		}
		// The seeded key lives in the file backend; per-backend counts say so.
		if want := map[string]int{"file": 1, "keychain": 0}[b.Name]; b.KeyCount != want {
			t.Fatalf("keyCount wrong for %q: %+v", b.Name, b)
		}
	}
}

func TestKeystoreMigrateFileToKeychainAndBack(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home) // "bot" in the file backend
	env := map[string]string{"KORBIT_CLI_HOME": home}

	// file -> keychain (per key; the new-key default in config stays put)
	_, errb, code := runCLI([]string{"keystore", "migrate", "keychain", "bot"}, env, nil)
	if code != 0 {
		t.Fatalf("migrate to keychain: code=%d err=%s", code, errb)
	}
	if got := keyBackend(t, home, "bot"); got != "keychain" {
		t.Fatalf("record not re-pointed: %q", got)
	}
	if cfg, _ := config.Load(home, nil); cfg.Keystore != "file" {
		t.Fatalf("migrate must not change the new-key default: %+v", cfg)
	}
	if v, _ := keystore.NewKeyring().Get("bot"); v == "" {
		t.Fatal("key not present in keychain after migrate")
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v != "" {
		t.Fatal("key not removed from file backend after migrate")
	}

	// keychain -> file via --all (round-trip restores the original layout)
	_, errb, code = runCLI([]string{"keystore", "migrate", "file", "--all"}, env, nil)
	if code != 0 {
		t.Fatalf("migrate back to file: code=%d err=%s", code, errb)
	}
	if got := keyBackend(t, home, "bot"); got != "file" {
		t.Fatalf("record not re-pointed back: %q", got)
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v == "" {
		t.Fatal("key not present in file backend after migrating back")
	}
	if v, _ := keystore.NewKeyring().Get("bot"); v != "" {
		t.Fatal("key not removed from keychain after migrating back")
	}
}

func TestKeystoreMigrateNamesOrAllRequired(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}
	// No names and no --all: usage error, nothing moved.
	if _, _, code := runCLI([]string{"keystore", "migrate", "keychain"}, env, nil); code != 2 {
		t.Fatalf("expected exit 2 without names/--all, got %d", code)
	}
	// Both names and --all: also a usage error.
	if _, _, code := runCLI([]string{"keystore", "migrate", "keychain", "bot", "--all"}, env, nil); code != 2 {
		t.Fatalf("expected exit 2 for names+--all, got %d", code)
	}
	// Unknown key name: usage error.
	if _, _, code := runCLI([]string{"keystore", "migrate", "keychain", "nope"}, env, nil); code != 2 {
		t.Fatalf("expected exit 2 for unknown key, got %d", code)
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v == "" {
		t.Fatal("key must be untouched after usage errors")
	}
}

func TestKeystoreMigrateOnlyNamedKey(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home) // "bot"
	env := map[string]string{"KORBIT_CLI_HOME": home}
	if _, errb, code := runCLI([]string{"key", "add", "reader"}, env, nil); code != 0 {
		t.Fatalf("key add: %s", errb)
	}

	_, errb, code := runCLI([]string{"keystore", "migrate", "keychain", "bot"}, env, nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if got := keyBackend(t, home, "bot"); got != "keychain" {
		t.Fatalf("bot should be migrated: %q", got)
	}
	if got := keyBackend(t, home, "reader"); got != "file" {
		t.Fatalf("reader must be untouched: %q", got)
	}
	if v, _ := keystore.NewFile(home).Get("reader"); v == "" {
		t.Fatal("reader's material must stay in the file backend")
	}
}

func TestKeystoreMigrateKeepSource(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	_, errb, code := runCLI([]string{"keystore", "migrate", "keychain", "--all", "--keep-source"}, env, nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v == "" {
		t.Fatal("--keep-source should have left the key in the file backend")
	}
	if v, _ := keystore.NewKeyring().Get("bot"); v == "" {
		t.Fatal("key should also be in keychain")
	}
	if got := keyBackend(t, home, "bot"); got != "keychain" {
		t.Fatalf("record should point at the target: %q", got)
	}
}

func TestKeystoreMigrateSameBackendIsNoop(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home)
	out, errb, code := runCLI([]string{"keystore", "migrate", "file", "bot", "--compact"}, map[string]string{"KORBIT_CLI_HOME": home}, nil)
	if code != 0 { // idempotent: re-running a migration is a clean no-op
		t.Fatalf("same-backend migrate should succeed as a no-op: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, `"alreadyInTarget":["bot"]`) {
		t.Fatalf("expected alreadyInTarget bucket: %s", out)
	}
}

func TestKeystoreMigrateUnknownBackend(t *testing.T) {
	home := t.TempDir()
	_, _, code := runCLI([]string{"keystore", "migrate", "redis", "--all"}, map[string]string{"KORBIT_CLI_HOME": home}, nil)
	if code != 2 { // UsageError -> exit 2
		t.Fatalf("expected exit 2 for unknown backend, got %d", code)
	}
}

func TestKeystoreMigrateAbortsWhenTargetUnavailable(t *testing.T) {
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	home := t.TempDir()
	seedBoundKey(t, home)
	env := map[string]string{"KORBIT_CLI_HOME": home}

	_, errb, code := runCLI([]string{"keystore", "migrate", "keychain", "--all"}, env, nil)
	if code != 4 {
		t.Fatalf("expected exit 4 when keychain unavailable, got %d (%s)", code, errb)
	}
	// Nothing must have changed: record still file, key still in file.
	if got := keyBackend(t, home, "bot"); got != "file" {
		t.Fatalf("record must stay file when target is unavailable: %q", got)
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v == "" {
		t.Fatal("key must remain in the file backend when migration aborts")
	}
	// Restore a working mock so later tests aren't affected by the error provider.
	keystore.MockKeychain()
}

// TestKeystoreMigrateRecoversWhenSourceUnavailable reproduces the scenario
// doctor advertises: a key's record points at the keychain backend, but the OS
// keyring is unavailable here and the material actually sits in the file
// backend (e.g. keys.json was edited or copied between machines). `keystore
// migrate file <key>` must recover it — not abort trying to read the
// unreachable keychain.
func TestKeystoreMigrateRecoversWhenSourceUnavailable(t *testing.T) {
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	home := t.TempDir()
	seedBoundKey(t, home) // key "bot": material in the file backend
	// Simulate the orphan: the record claims the (unavailable) keychain.
	if err := keys.NewManager(home, "file", nil, nil).SetKeystoreBackend("bot", "keychain"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KORBIT_CLI_HOME": home}

	out, errb, code := runCLI([]string{"keystore", "migrate", "file", "bot", "--compact"}, env, nil)
	if code != 0 {
		t.Fatalf("recovery migrate should succeed, got code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, `"recovered":["bot"]`) {
		t.Fatalf("expected recovered bucket: %s", out)
	}
	if !strings.Contains(out, `"warnings":[`) || !strings.Contains(out, "unavailable here") {
		t.Fatalf("recovery warning must be carried in stdout: %s", out)
	}
	if strings.TrimSpace(errb) != "" {
		t.Fatalf("recovery warning must not be duplicated on stderr: %s", errb)
	}
	if got := keyBackend(t, home, "bot"); got != "file" {
		t.Fatalf("record not re-pointed to file: %q", got)
	}
	if v, _ := keystore.NewFile(home).Get("bot"); v == "" {
		t.Fatal("key should be recovered/retained in the file backend")
	}
	keystore.MockKeychain()
}

// TestKeystoreMigrateSkipsUnknownBackendKey: a key whose record names a backend
// this build can't construct must not brick the batch. `--all` migrates the
// others and skips it (untouched, record preserved); naming it explicitly errors.
func TestKeystoreMigrateSkipsUnknownBackendKey(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	seedBoundKey(t, home) // "bot" in the file backend
	env := map[string]string{"KORBIT_CLI_HOME": home}
	if _, errb, code := runCLI([]string{"key", "add", "future"}, env, nil); code != 0 {
		t.Fatalf("key add: %s", errb)
	}
	// Re-point "future" at a backend this build doesn't support (as a newer CLI would).
	if err := keys.NewManager(home, "file", nil, nil).SetKeystoreBackend("future", "hardware"); err != nil {
		t.Fatal(err)
	}

	// --all: migrates "bot", skips "future" (record preserved), exit 0.
	out, errb, code := runCLI([]string{"keystore", "migrate", "keychain", "--all", "--compact"}, env, nil)
	if code != 0 {
		t.Fatalf("--all must not abort on an unknown-backend key: code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, `"moved":["bot"]`) {
		t.Fatalf("the migratable key should still move: %s", out)
	}
	if !strings.Contains(out, `"skippedUnsupported":["future"]`) || !strings.Contains(out, `"warnings":[`) {
		t.Fatalf("skipped unknown-backend key must be reported in stdout: %s", out)
	}
	if strings.TrimSpace(errb) != "" {
		t.Fatalf("skipped unknown-backend warning must not be duplicated on stderr: %s", errb)
	}
	if got := keyBackend(t, home, "future"); got != "hardware" {
		t.Fatalf("unknown-backend key must be left untouched, got %q", got)
	}

	// Naming it explicitly is a clear error.
	if _, _, code := runCLI([]string{"keystore", "migrate", "keychain", "future"}, env, nil); code != 4 {
		t.Fatalf("naming an unknown-backend key should fail (exit 4), got %d", code)
	}
}

func TestKeystoreDefaultCommand(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}

	out, errb, code := runCLI([]string{"keystore", "default", "keychain", "--json"}, env, nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, `"default": "keychain"`) && !strings.Contains(out, `"default":"keychain"`) {
		t.Fatalf("output should echo the default: %s", out)
	}
	if cfg, _ := config.Load(home, nil); cfg.Keystore != "keychain" {
		t.Fatalf("config not updated: %+v", cfg)
	}
	// New keys now land in the keychain; existing records are untouched by design.
	if _, errb, code := runCLI([]string{"key", "add", "hot"}, env, nil); code != 0 {
		t.Fatalf("key add: %s", errb)
	}
	if got := keyBackend(t, home, "hot"); got != "keychain" {
		t.Fatalf("new key should use the new default: %q", got)
	}

	// Unknown backend -> usage error; unavailable backend -> config error.
	if _, _, code := runCLI([]string{"keystore", "default", "redis"}, env, nil); code != 2 {
		t.Fatalf("expected exit 2 for unknown backend, got %d", code)
	}
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	if _, _, code := runCLI([]string{"keystore", "default", "keychain"}, env, nil); code != 4 {
		t.Fatalf("expected exit 4 for unavailable backend, got %d", code)
	}
	keystore.MockKeychain()
}

func TestKeyAddKeystoreFlag(t *testing.T) {
	keystore.MockKeychain()
	home := t.TempDir()
	env := map[string]string{"KORBIT_CLI_HOME": home}

	out, errb, code := runCLI([]string{"key", "add", "hot", "--keystore", "keychain", "--json"}, env, nil)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out, `"keystore": "keychain"`) {
		t.Fatalf("key add output should carry the keystore: %s", out)
	}
	if got := keyBackend(t, home, "hot"); got != "keychain" {
		t.Fatalf("record backend = %q", got)
	}
	if v, _ := keystore.NewKeyring().Get("hot"); v == "" {
		t.Fatal("material should be in the keychain")
	}
	if v, _ := keystore.NewFile(home).Get("hot"); v != "" {
		t.Fatal("material must not be in the file backend")
	}

	// Bad value -> usage error before anything is created.
	if _, _, code := runCLI([]string{"key", "add", "x", "--keystore", "redis"}, env, nil); code != 2 {
		t.Fatalf("expected exit 2 for bad --keystore, got %d", code)
	}
	// Unavailable backend -> config error, nothing created.
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	if _, _, code := runCLI([]string{"key", "add", "x", "--keystore", "keychain"}, env, nil); code != 4 {
		t.Fatalf("expected exit 4 for unavailable backend, got %d", code)
	}
	keystore.MockKeychain()
	if _, _, code := runCLI([]string{"key", "show", "x"}, env, nil); code == 0 {
		t.Fatal("failed add must not leave a record behind")
	}
}

func TestKeystoreStatusReportsUnavailableKeychain(t *testing.T) {
	keystore.MockKeychainUnavailable(errors.New("no secret service"))
	home := t.TempDir()
	out, _, code := runCLI([]string{"keystore", "status"}, map[string]string{"KORBIT_CLI_HOME": home}, nil)
	if code != 0 {
		t.Fatalf("status should not fail: code=%d", code)
	}
	if !strings.Contains(out, "keychain") || !strings.Contains(out, "unavailable") {
		t.Fatalf("status should flag keychain unavailable:\n%s", out)
	}
	keystore.MockKeychain()
}
