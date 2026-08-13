// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/config"
)

// TestRegistryMatchesConfigBackends is the drift guard that ties the two
// canonical lists together: keystore.registry (construction/probing) and
// config.Backends (CLI-layer name validation). Adding a backend to either
// side without the other is a latent runtime bug — a name that is constructible
// but rejected by every command, or config-listed but permanently "unknown" —
// so this test fails loudly, naming the missing side, the moment they diverge.
func TestRegistryMatchesConfigBackends(t *testing.T) {
	registryNames := make([]string, 0, len(registry))
	for name := range registry {
		registryNames = append(registryNames, name)
	}
	sort.Strings(registryNames)

	configNames := append([]string(nil), config.Backends...)
	sort.Strings(configNames)

	inConfig := make(map[string]bool, len(config.Backends))
	for _, n := range config.Backends {
		inConfig[n] = true
	}
	for name := range registry {
		if !inConfig[name] {
			t.Errorf("backend %q is in keystore.registry but missing from config.Backends — add it to config.Backends (config/config.go)", name)
		}
	}
	for _, name := range config.Backends {
		if _, ok := registry[name]; !ok {
			t.Errorf("backend %q is in config.Backends but missing from keystore.registry — add a registry entry (keystore/keystore.go)", name)
		}
	}
}

// TestRegistryBackendsConstructAndProbe checks, for every canonical backend
// name, that the three lookups agree end to end: ByName builds it and the built
// keystore reports exactly that backend name (catching a copy-pasted wrong name
// in a registry entry), Available does not reject it as "unknown" (it MAY report
// it unavailable — a CI box with no OS keyring legitimately does — which is a
// distinct, allowed outcome), and config.ValidBackend agrees the name is real.
func TestRegistryBackendsConstructAndProbe(t *testing.T) {
	// The keychain probe reads the OS keyring; MockInit keeps it off any real
	// keyring (mirrors TestKeyringRoundTrip).
	MockKeychain()

	for _, name := range config.Backends {
		ks, err := ByName(name, t.TempDir(), nil)
		if err != nil {
			t.Errorf("ByName(%q) failed: %v", name, err)
			continue
		}
		if ks.Backend() != name {
			t.Errorf("ByName(%q).Backend() = %q — registry entry names the wrong backend", name, ks.Backend())
		}

		if err := Available(name, nil); errors.Is(err, errUnknownBackend) {
			t.Errorf("Available(%q) reported the name as unknown; it must be recognized (an availability error would be fine): %v", name, err)
		}

		if !config.ValidBackend(name) {
			t.Errorf("config.ValidBackend(%q) = false — the CLI-layer validation rejects a registered backend", name)
		}
	}
}

// TestAvailableUnknownIsDistinguishable pins the seam the drift guard relies on:
// an unknown name wraps errUnknownBackend (so the guard can tell "unknown" from
// "unavailable"), while the user-facing message is unchanged.
func TestAvailableUnknownIsDistinguishable(t *testing.T) {
	err := Available("definitely-not-a-backend", nil)
	if !errors.Is(err, errUnknownBackend) {
		t.Fatalf("Available(unknown) = %v; want it to wrap errUnknownBackend", err)
	}
	if err.Error() != `unknown keystore backend "definitely-not-a-backend"` {
		t.Fatalf("unknown-backend message changed: %q", err.Error())
	}
	if _, err := ByName("definitely-not-a-backend", t.TempDir(), nil); !errors.Is(err, errUnknownBackend) {
		t.Fatalf("ByName(unknown) = %v; want it to wrap errUnknownBackend", err)
	}
}

func TestByNameSelectsBackend(t *testing.T) {
	for _, name := range []string{"file", "keychain"} {
		ks, err := ByName(name, t.TempDir(), nil)
		if err != nil || ks.Backend() != name {
			t.Fatalf("ByName(%q) = %v, %v", name, ks, err)
		}
	}
	if _, err := ByName("nope", t.TempDir(), nil); err == nil {
		t.Fatal("unknown backend must error")
	}
}

func TestFileCorruptionAndVersion(t *testing.T) {
	dir := t.TempDir()
	ks := NewFile(dir)
	path := filepath.Join(dir, "keystore.json")

	os.WriteFile(path, []byte("not json"), 0o600)
	if _, err := ks.Get("x"); err == nil {
		t.Fatal("corrupt JSON should error")
	}
	os.WriteFile(path, []byte(`{"version":2,"entries":{}}`), 0o600)
	if _, err := ks.Get("x"); err == nil {
		t.Fatal("unsupported version should error")
	}
}

func TestFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ks := NewFile(dir)
	if ks.Backend() != "file" {
		t.Fatalf("backend = %q", ks.Backend())
	}
	if got, err := ks.Get("missing"); err != nil || got != "" {
		t.Fatalf("absent should be empty/nil, got %q %v", got, err)
	}
	secret := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"
	if err := ks.Set("a", secret); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("round trip mismatch")
	}
	// File must be 0600 and must not contain the plaintext.
	info, _ := os.Stat(filepath.Join(dir, "keystore.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %o", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "keystore.json"))
	if string(raw) == "" || contains(raw, "BEGIN PRIVATE KEY") {
		t.Fatalf("plaintext leaked into keystore file")
	}
	if err := ks.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ks.Get("a"); got != "" {
		t.Fatalf("delete did not remove entry")
	}
}

func TestFileAADBindsName(t *testing.T) {
	dir := t.TempDir()
	ks := NewFile(dir)
	ks.Set("a", "secret-a")
	// Copy entry "a" to name "b" on disk, then attempt to read as "b": the AAD
	// mismatch must fail authentication (corruption error), not silently decrypt.
	raw, _ := os.ReadFile(filepath.Join(dir, "keystore.json"))
	swapped := replace(raw, `"a"`, `"b"`)
	os.WriteFile(filepath.Join(dir, "keystore.json"), swapped, 0o600)
	if _, err := ks.Get("b"); err == nil {
		t.Fatalf("expected AAD mismatch to fail decryption")
	}
}

func TestKeyringRoundTrip(t *testing.T) {
	MockKeychain()
	ks := NewKeyring()
	if ks.Backend() != "keychain" {
		t.Fatalf("backend = %q", ks.Backend())
	}
	if got, err := ks.Get("missing"); err != nil || got != "" {
		t.Fatalf("absent should be empty/nil, got %q %v", got, err)
	}
	if err := ks.Set("a", "secret"); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Get("a")
	if err != nil || got != "secret" {
		t.Fatalf("round trip: %q %v", got, err)
	}
	if err := ks.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ks.Get("a"); got != "" {
		t.Fatalf("delete did not remove")
	}
}

func contains(b []byte, s string) bool { return indexOf(b, s) >= 0 }

func replace(b []byte, old, new string) []byte {
	i := indexOf(b, old)
	if i < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	out = append(out, b[:i]...)
	out = append(out, []byte(new)...)
	out = append(out, b[i+len(old):]...)
	return out
}

func indexOf(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}
