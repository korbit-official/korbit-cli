// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// TestKeyringReadsLegacyService pins the dual-read path: a secret stored under
// the service an installation made before the product rename still resolves, and
// the current service wins when both carry an item for the same key.
func TestKeyringReadsLegacyService(t *testing.T) {
	MockKeychain()
	ks := NewKeyring()

	// Only the legacy service has the item: it must still be found.
	if err := keychain.set(legacyKeychainService, account("old"), "legacy-secret"); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Get("old")
	if err != nil || got != "legacy-secret" {
		t.Fatalf("Get(old) = %q, %v; want the legacy item", got, err)
	}

	// Both services carry an item: the current service wins.
	if err := ks.Set("old", "current-secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Get("old"); err != nil || got != "current-secret" {
		t.Fatalf("Get(old) = %q, %v; want the current-service item", got, err)
	}

	// A miss on both services is "" with no error, not a failure.
	if got, err := ks.Get("absent"); err != nil || got != "" {
		t.Fatalf("Get(absent) = %q, %v; want \"\", nil", got, err)
	}
}

// TestKeyringSetWritesCurrentServiceOnly pins that new items never land under
// the legacy service — the legacy name is read-and-clean-up only.
func TestKeyringSetWritesCurrentServiceOnly(t *testing.T) {
	MockKeychain()
	if err := NewKeyring().Set("fresh", "s"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := keychain.get(keychainService, account("fresh")); !found {
		t.Fatal("Set did not write under the current service")
	}
	if _, found, _ := keychain.get(legacyKeychainService, account("fresh")); found {
		t.Fatal("Set wrote under the legacy service")
	}
}

// TestKeyringDeleteRemovesLegacyItem pins that Delete clears BOTH services —
// leaving the legacy item behind would let Get's fallback resurrect a key the
// user removed.
func TestKeyringDeleteRemovesLegacyItem(t *testing.T) {
	MockKeychain()
	ks := NewKeyring()
	if err := keychain.set(legacyKeychainService, account("old"), "legacy-secret"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Set("old", "current-secret"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Delete("old"); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Get("old"); err != nil || got != "" {
		t.Fatalf("Get after Delete = %q, %v; want the key gone from both services", got, err)
	}
}

// TestProbeAccountIsNeverWritten pins the probe sentinel's contract: it names an
// account no write path ever uses, so ProbeKeyring stays read-only.
func TestProbeAccountIsNeverWritten(t *testing.T) {
	MockKeychain()
	if err := NewKeyring().Set("someone", "s"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := keychain.get(keychainService, probeAccount); found {
		t.Fatal("the probe sentinel account was written")
	}
	if err := ProbeKeyring(); err != nil {
		t.Fatalf("ProbeKeyring on a healthy keychain: %v", err)
	}
}

// recordingKeychain is a keychainProvider that records which services a
// delete was attempted against and can be made to fail for a chosen service.
type recordingKeychain struct {
	entries     map[string]string
	delFailFor  string
	delAttempts []string
}

func (k *recordingKeychain) set(service, account, secret string) error {
	k.entries[service+"\x00"+account] = secret
	return nil
}

func (k *recordingKeychain) get(service, account string) (string, bool, error) {
	s, ok := k.entries[service+"\x00"+account]
	return s, ok, nil
}

func (k *recordingKeychain) del(service, account string) error {
	k.delAttempts = append(k.delAttempts, service)
	if service == k.delFailFor {
		return errors.New("backend refused")
	}
	delete(k.entries, service+"\x00"+account)
	return nil
}

func (k *recordingKeychain) probe(string) error { return nil }

// TestKeyringDeleteAttemptsBothServicesOnFailure: a failure deleting the
// current-service item must not stop the legacy one from being removed. Giving
// up early would leave the legacy item behind for Get's fallback to resurrect,
// so a key the user deleted would come back on the next command.
func TestKeyringDeleteAttemptsBothServicesOnFailure(t *testing.T) {
	fake := &recordingKeychain{entries: map[string]string{}, delFailFor: keychainService}
	prev := keychain
	keychain = fake
	t.Cleanup(func() { keychain = prev })

	if err := fake.set(keychainService, account("old"), "current-secret"); err != nil {
		t.Fatal(err)
	}
	if err := fake.set(legacyKeychainService, account("old"), "legacy-secret"); err != nil {
		t.Fatal(err)
	}

	err := NewKeyring().Delete("old")
	if err == nil {
		t.Fatal("a backend failure must be reported")
	}
	if !strings.Contains(err.Error(), keychainService) {
		t.Fatalf("the error should name the failing service: %v", err)
	}
	want := []string{keychainService, legacyKeychainService}
	if len(fake.delAttempts) != len(want) {
		t.Fatalf("delete attempts = %v, want both services tried", fake.delAttempts)
	}
	for i := range want {
		if fake.delAttempts[i] != want[i] {
			t.Fatalf("delete attempts = %v, want %v", fake.delAttempts, want)
		}
	}
	if _, found, _ := fake.get(legacyKeychainService, account("old")); found {
		t.Fatal("the legacy item survived — Get's fallback would resurrect the key")
	}
}
