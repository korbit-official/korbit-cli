// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/keystore"
	"github.com/digitalx-official/digitalx-cli/internal/keystore/keystoretest"
	"github.com/digitalx-official/digitalx-cli/internal/output"
)

// testEnv wires a Manager over in-memory mock backends ("file" and
// "keychain"), with per-backend availability injectable via probeErr.
type testEnv struct {
	m        *Manager
	stores   map[string]*keystoretest.MockKeystore
	probeErr map[string]error
}

func newEnv(t *testing.T, defaultBackend string) *testEnv {
	t.Helper()
	env := &testEnv{
		stores: map[string]*keystoretest.MockKeystore{
			"file":     keystoretest.New("file"),
			"keychain": keystoretest.New("keychain"),
		},
		probeErr: map[string]error{},
	}
	open := func(b string) (keystore.Keystore, error) {
		if s, ok := env.stores[b]; ok {
			return s, nil
		}
		return nil, output.Configf("unknown keystore backend %q", b)
	}
	probe := func(b string) error {
		if err := env.probeErr[b]; err != nil {
			return err
		}
		if _, ok := env.stores[b]; !ok {
			return output.Configf("unknown keystore backend %q", b)
		}
		return nil
	}
	env.m = NewManagerWithBackends(t.TempDir(), defaultBackend, open, probe, func() int64 { return 1700000000000 })
	return env
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	return newEnv(t, "file").m
}

func TestKeysFileCorruption(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keys.json"), []byte("not json"), 0o600)
	m := NewManager(dir, "file", nil, nil)
	if _, err := m.List(); err == nil {
		t.Fatal("corrupt keys.json should surface a config error")
	}
}

func TestKeysFileUnsupportedVersionRejected(t *testing.T) {
	dir := t.TempDir()
	bad := `{"version":99,"defaultKey":null,"keys":{}}`
	os.WriteFile(filepath.Join(dir, "keys.json"), []byte(bad), 0o600)
	if _, err := NewManager(dir, "file", nil, nil).List(); err == nil {
		t.Fatal("unsupported version must be rejected")
	}
}

func TestKeysFileMissingTypeOrKeystoreRejected(t *testing.T) {
	cases := map[string]string{
		"missing type":     `{"version":1,"defaultKey":null,"keys":{"a":{"keystore":"file","apiKeyId":null,"publicKey":"p","createdAt":1}}}`,
		"missing keystore": `{"version":1,"defaultKey":null,"keys":{"a":{"type":"ed25519","apiKeyId":null,"publicKey":"p","createdAt":1}}}`,
	}
	for label, raw := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "keys.json"), []byte(raw), 0o600)
		m := NewManager(dir, "file", nil, nil)
		if _, err := m.List(); err == nil || !strings.Contains(err.Error(), `"a"`) {
			t.Fatalf("%s: expected per-key load error naming the key, got %v", label, err)
		}
	}
}

func TestKeysFileInvalidDefaultAccountSeqRejected(t *testing.T) {
	// Unlike type/keystore (tolerant on load), defaultAccountSeq is a hard wire
	// constraint (>= 1) — an out-of-range value is rejected at load, not deferred.
	cases := map[string]string{
		"zero":     `{"version":1,"defaultKey":null,"keys":{"a":{"type":"ed25519","keystore":"file","apiKeyId":null,"publicKey":"p","createdAt":1,"defaultAccountSeq":0}}}`,
		"negative": `{"version":1,"defaultKey":null,"keys":{"a":{"type":"ed25519","keystore":"file","apiKeyId":null,"publicKey":"p","createdAt":1,"defaultAccountSeq":-2}}}`,
	}
	for label, raw := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "keys.json"), []byte(raw), 0o600)
		m := NewManager(dir, "file", nil, nil)
		if _, err := m.List(); err == nil || !strings.Contains(err.Error(), "defaultAccountSeq") || !strings.Contains(err.Error(), `"a"`) {
			t.Fatalf("%s: expected a load error naming the key and field, got %v", label, err)
		}
	}
}

func TestMetaForSelection(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.m.SetBaseURL("a", "https://api-test.digitalx.miraeasset.com", "wss://ws-api-test.digitalx.miraeasset.com"); err != nil {
		t.Fatal(err)
	}
	if err := env.m.SetDefaultAccountSeq("a", 3); err != nil {
		t.Fatal(err)
	}

	// A stored selection picks up every per-key field (name "" => the default key).
	got := env.m.MetaForSelection(Selection{Name: ""})
	if got.BaseURL != "https://api-test.digitalx.miraeasset.com" ||
		got.WSBaseURL != "wss://ws-api-test.digitalx.miraeasset.com" ||
		got.DefaultAccountSeq != "3" {
		t.Fatalf("stored selection meta = %+v", got)
	}

	// An inline credential has no stored metadata: the per-key tiers are skipped.
	if inline := env.m.MetaForSelection(Selection{Inline: true}); inline != (KeyMeta{}) {
		t.Fatalf("inline selection must yield the zero KeyMeta, got %+v", inline)
	}
}

func TestMetadataCacheInvalidatedOnMutation(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}

	// First peek fills the cache.
	if got := env.m.MetaDefaultAccountSeq("a"); got != "" {
		t.Fatalf("fresh key should have no default accountSeq, got %q", got)
	}
	// A direct on-disk edit (bypassing the Manager) is NOT observed — the cache
	// holds the earlier parse.
	if raw, err := os.ReadFile(env.m.path); err == nil {
		os.WriteFile(env.m.path, []byte(strings.Replace(string(raw),
			`"createdAt"`, `"defaultAccountSeq":9,"createdAt"`, 1)), 0o600)
	}
	if got := env.m.MetaDefaultAccountSeq("a"); got != "" {
		t.Fatalf("cache should mask an out-of-band on-disk change, got %q", got)
	}
	// A mutation through the Manager invalidates the cache; the next peek re-reads.
	if err := env.m.SetDefaultAccountSeq("a", 2); err != nil {
		t.Fatal(err)
	}
	if got := env.m.MetaDefaultAccountSeq("a"); got != "2" {
		t.Fatalf("after a Manager mutation the peek must reflect it, got %q", got)
	}
	// An explicit Invalidate forces a fresh read of whatever is on disk now.
	env.m.Invalidate()
	if got := env.m.MetaDefaultAccountSeq("a"); got != "2" {
		t.Fatalf("after Invalidate the peek must re-read disk, got %q", got)
	}
}

// writeRecord plants a keys.json with one bound record of the given type and
// backend, for exercising the tolerant-load/strict-use policy.
func writeRecord(t *testing.T, m *Manager, typ, backend string) {
	t.Helper()
	raw := `{"version":1,"defaultKey":"a","keys":{"a":{"type":"` + typ + `","keystore":"` + backend + `","apiKeyId":"KEYID-1","publicKey":"p","createdAt":1}}}`
	if err := os.WriteFile(m.path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownTypeLoadsButFailsAtUse(t *testing.T) {
	env := newEnv(t, "file")
	// A type this build can't sign with (a forward-version scheme).
	writeRecord(t, env.m, "ecdsa-secp256k1", "file")
	// Listing still works — one future-typed key must not brick the registry.
	list, err := env.m.List()
	if err != nil || len(list) != 1 || list[0].Type != "ecdsa-secp256k1" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	// Signing with it fails, naming the type.
	if _, err := env.m.Resolve("a"); err == nil || !strings.Contains(err.Error(), `"ecdsa-secp256k1"`) {
		t.Fatalf("Resolve should reject unsupported type: %v", err)
	}
}

func TestUnknownBackendLoadsButFailsAtUse(t *testing.T) {
	env := newEnv(t, "file")
	writeRecord(t, env.m, TypeEd25519, "hardware")
	if _, err := env.m.List(); err != nil {
		t.Fatalf("unknown backend must still load: %v", err)
	}
	if _, err := env.m.Resolve("a"); err == nil || !strings.Contains(err.Error(), `"hardware"`) {
		t.Fatalf("Resolve should reject unknown backend per key: %v", err)
	}
}

func TestAddFirstKeyBecomesDefault(t *testing.T) {
	m := newManager(t)
	r, err := m.Add("a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsDefault {
		t.Fatalf("first key should be default")
	}
	if r.Type != TypeEd25519 || r.Keystore != "file" {
		t.Fatalf("new key should be stamped type/keystore, got %+v", r)
	}
	r2, err := m.Add("b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r2.IsDefault {
		t.Fatalf("second key should NOT auto-become default")
	}
}

func TestAddBackendSelection(t *testing.T) {
	env := newEnv(t, "file")
	// Default backend applies when none is passed.
	if r, err := env.m.Add("a", "", ""); err != nil || r.Keystore != "file" {
		t.Fatalf("Add default backend = %+v, %v", r, err)
	}
	if !env.stores["file"].Has("a") {
		t.Fatal("material should be in the file backend")
	}
	// An explicit backend wins over the default and is recorded per key.
	if r, err := env.m.Add("b", "", "keychain"); err != nil || r.Keystore != "keychain" {
		t.Fatalf("Add explicit backend = %+v, %v", r, err)
	}
	if !env.stores["keychain"].Has("b") || env.stores["file"].Has("b") {
		t.Fatal("material should be only in the keychain backend")
	}
	if s, _ := env.m.Show("b"); s.Keystore != "keychain" || s.Type != TypeEd25519 {
		t.Fatalf("Show = %+v", s)
	}
}

func TestAddFailsFastWhenBackendUnavailable(t *testing.T) {
	env := newEnv(t, "file")
	env.probeErr["keychain"] = errors.New("no keyring here")
	if _, err := env.m.Add("a", "", "keychain"); err == nil {
		t.Fatal("Add must fail when the chosen backend is unavailable")
	}
	// Nothing was created anywhere.
	if names, _ := env.m.Names(); len(names) != 0 {
		t.Fatalf("no record should exist, got %v", names)
	}
	if env.stores["keychain"].Has("a") || env.stores["file"].Has("a") {
		t.Fatal("no material should be stored")
	}
}

func TestAddDuplicateRejected(t *testing.T) {
	m := newManager(t)
	m.Add("a", "", "")
	if _, err := m.Add("a", "", ""); err == nil {
		t.Fatalf("duplicate add should fail")
	}
}

func TestResolveRequiresBindingAndKey(t *testing.T) {
	m := newManager(t)
	if _, err := m.Resolve(""); err == nil {
		t.Fatalf("resolve with no keys should fail")
	}
	m.Add("a", "", "")
	// Bound? not yet.
	if _, err := m.Resolve("a"); err == nil {
		t.Fatalf("resolve before bind should fail")
	}
	if err := m.Bind("a", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Resolve("")
	if err != nil {
		t.Fatalf("resolve default after bind: %v", err)
	}
	signer, err := got.Signer()
	if err != nil || signer == nil {
		t.Fatalf("resolved should carry a usable signer: %v", err)
	}
	if got.Name != "a" || got.APIKeyID != "KEYID-1" {
		t.Fatalf("resolved wrong: %+v", got)
	}
	if got.Type != TypeEd25519 || got.Keystore != "file" {
		t.Fatalf("resolved should carry type/keystore: %+v", got)
	}
}

// TestBindRejectsPublicKey reproduces the field bug where a user, at the
// registration step, pastes the key's own public key into --api-key instead of
// the id the portal issues. That value is not a valid api-key id and every
// signed call would fail with KEY_NOT_FOUND, so the bind must be refused up
// front, whichever encoding was pasted.
func TestBindRejectsPublicKey(t *testing.T) {
	m := newManager(t)
	if _, err := m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	show, err := m.Show("a")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(show.PublicKey))
	if block == nil {
		t.Fatal("no public key PEM for the new key")
	}
	pasted := map[string]string{
		"full PEM":  show.PublicKey,
		"PEM body":  base64.StdEncoding.EncodeToString(block.Bytes),
		"base64url": base64.RawURLEncoding.EncodeToString(block.Bytes),
	}
	for name, v := range pasted {
		err := m.Bind("a", v)
		if err == nil || !strings.Contains(err.Error(), "public key") {
			t.Errorf("%s: expected bind to reject a pasted public key, got %v", name, err)
		}
	}

	// The worse mistake: pasting a private key. Rejected with a secret-aware
	// message that never echoes the pasted material.
	kp, err := apiclient.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	privBlock, _ := pem.Decode([]byte(kp.PrivatePEM))
	for name, v := range map[string]string{
		"full PEM":    kp.PrivatePEM,
		"PKCS#8 body": base64.StdEncoding.EncodeToString(privBlock.Bytes),
	} {
		err := m.Bind("a", v)
		if err == nil || !strings.Contains(err.Error(), "PRIVATE key") {
			t.Errorf("%s: expected bind to reject a pasted private key, got %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), v) {
			t.Errorf("%s: error must not echo the pasted private key material", name)
		}
	}

	// A real id still binds.
	if err := m.Bind("a", "KEYID-1"); err != nil {
		t.Fatalf("a genuine api-key id must still bind: %v", err)
	}
}

func TestResolveMissingMaterialNamesBackend(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "keychain")
	env.m.Bind("a", "KEYID-1")
	// Simulate the orphan case: the record points at keychain but the material
	// is gone from it.
	env.stores["keychain"].Delete("a")
	_, err := env.m.Resolve("a")
	if err == nil || !strings.Contains(err.Error(), "keychain keystore") {
		t.Fatalf("error should name the key's own backend: %v", err)
	}
	if !strings.Contains(err.Error(), "keystore migrate") {
		t.Fatalf("error should point at the recovery command: %v", err)
	}
}

// TestResolveSignerRoundTrip is the end-to-end check of the signer seam: the
// key the keystore returned, parsed by Resolve and handed back via Signer(),
// must produce a signature the RECORD's public key verifies. This proves
// Signer() returns the real private key for that record, not some stale or
// mismatched material.
func TestResolveSignerRoundTrip(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("a", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	resolved, err := env.m.Resolve("a")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	signer, err := resolved.Signer()
	if err != nil {
		t.Fatalf("Signer: %v", err)
	}
	// Recover the record's public key (the only public side of truth) and verify
	// a signature made with the returned signer against it.
	show, err := env.m.Show("a")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(show.PublicKey))
	if block == nil {
		t.Fatal("stored public key is not a PEM")
	}
	parsedPub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse stored public key: %v", err)
	}
	pub, ok := parsedPub.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("stored public key is not ed25519: %T", parsedPub)
	}
	msg := "symbol=btc_krw&timestamp=1700000000000"
	sig, err := base64.StdEncoding.DecodeString(signer.Sign(msg))
	if err != nil {
		t.Fatalf("ed25519 signer should return base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte(msg), sig) {
		t.Fatal("signature from Signer() did not verify against the record's public key")
	}
}

// TestResolveCorruptPEMIsConfigError plants garbage where the private PEM should
// be. After the signer seam, parsing happens inside Resolve, so a corrupt vault
// entry must surface as a ConfigError (exit 4) naming the key and suggesting
// recovery — NOT as apiclient.ParsePrivatePEM's bare UsageError (exit 2), and never
// leaking the planted bytes.
func TestResolveCorruptPEMIsConfigError(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("a", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	// Overwrite the stored material with non-PEM garbage.
	if err := env.stores["file"].Set("a", "-----BEGIN GARBAGE-----\nnot a key\n-----END GARBAGE-----\n"); err != nil {
		t.Fatal(err)
	}
	_, err := env.m.Resolve("a")
	if err == nil {
		t.Fatal("corrupt PEM should fail Resolve")
	}
	var cfg *output.ConfigError
	if !errors.As(err, &cfg) {
		t.Fatalf("corrupt PEM must be a ConfigError (exit 4), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("error should name the key: %v", err)
	}
	if !strings.Contains(err.Error(), "re-add") {
		t.Fatalf("error should suggest recovery: %v", err)
	}
}

// TestResolvedFormatRedactsSigner is the leak-hardening guard. Go's fmt reaches
// unexported fields by reflection, so unexporting `signer` alone would NOT stop
// %+v/%#v from dumping the raw ed25519 bytes. We assert that none of %v/%+v/%#v
// of a real resolved credential contains any fragment of the key material —
// neither the raw bytes (in any of the shapes fmt prints byte slices) nor its
// base64/hex forms — only the redaction placeholder.
func TestResolvedFormatRedactsSigner(t *testing.T) {
	env := newEnv(t, "file")
	// Import a KNOWN keypair so the test holds the exact private bytes to scan for
	// (the signer behind Resolved is opaque — apiclient.Signer — and never hands
	// them back).
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := env.m.Add("a", pemStr, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Bind("a", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	resolved, err := env.m.Resolve("a")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(priv)
	// Every shape fmt could render the key as if a Format arm were missing, so
	// dropping ANY arm (the %#v hex-struct arm and the %s/%v raw-bytes arms are
	// the easy ones to forget) makes this test fail. Each entry is the exact
	// substring the unredacted verb would emit:
	leaks := []string{
		fmt.Sprintf("%v", raw),         // decimal byte slice: [12 34 ...]
		fmt.Sprintf("%d", raw),         // same, explicitly
		fmt.Sprintf("%x", raw),         // contiguous lowercase hex
		fmt.Sprintf("%q", string(raw)), // quoted/escaped
		string(raw),                    // the raw 64-byte key as a literal string (what %s/%v of the key emits)
		base64.StdEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, resolved)
		if !strings.Contains(out, redactedSigner) {
			t.Fatalf("%s output should contain the redaction marker: %s", verb, out)
		}
		for _, leak := range leaks {
			// The first byte alone is too short to be a reliable signal; require
			// the whole encoded form to be absent.
			if leak != "" && strings.Contains(out, leak) {
				t.Fatalf("%s output leaked key material: %s", verb, out)
			}
		}
		// Also make sure the non-secret fields still print (the redaction must
		// not blank the whole struct — debuggability matters).
		if !strings.Contains(out, "KEYID-1") {
			t.Fatalf("%s output should still show non-secret fields: %s", verb, out)
		}
	}
	// A pointer to Resolved must also be safe: fmt dereferences and still finds
	// the Formatter (the method has a value receiver, so *Resolved satisfies it).
	if out := fmt.Sprintf("%+v", &resolved); strings.Contains(out, base64.StdEncoding.EncodeToString(raw)) {
		t.Fatalf("*Resolved leaked key material: %s", out)
	}
}

func TestRemoveDefaultUnsetsNoFallback(t *testing.T) {
	m := newManager(t)
	m.Add("a", "", "") // default
	m.Add("b", "", "")
	res, err := m.Remove("a", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.DefaultKey != nil {
		t.Fatalf("removing default must unset it (no fallback), got %v", *res.DefaultKey)
	}
	if !res.SecretRemoved || res.Warning != "" {
		t.Fatalf("healthy removal must report SecretRemoved with no warning, got %+v", res)
	}
	// "b" still exists but is NOT silently promoted.
	if _, err := m.Resolve(""); err == nil {
		t.Fatalf("after default removal, resolve with no --key should fail")
	}
}

func TestRemoveDeletesFromOwnBackend(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "keychain")
	if _, err := env.m.Remove("a", false); err != nil {
		t.Fatal(err)
	}
	if env.stores["keychain"].Has("a") {
		t.Fatal("material must be deleted from the key's own backend")
	}
}

// TestRemoveStrandedBackendNeedsForce covers a record whose backend this build
// doesn't know (e.g. written by a newer CLI): without --force it can't be
// removed (and the error must point at --force), with --force the record goes
// but the secret is reported as left behind.
func TestRemoveStrandedBackendNeedsForce(t *testing.T) {
	env := newEnv(t, "file")
	writeRecord(t, env.m, TypeEd25519, "hardware") // backend unknown to this build

	// Without force: a hard error that names the escape hatch, and the record
	// must still be present afterwards.
	_, err := env.m.Remove("a", false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("no-force removal of a stranded key must error pointing at --force: %v", err)
	}
	if names, _ := env.m.Names(); len(names) != 1 {
		t.Fatalf("failed no-force removal must leave the record in place, got %v", names)
	}

	// With force: the record is removed, the secret is reported not-removed, and
	// the warning names the backend it was left in.
	res, err := env.m.Remove("a", true)
	if err != nil {
		t.Fatalf("forced removal of a stranded key must succeed: %v", err)
	}
	if res.SecretRemoved {
		t.Fatalf("a stranded backend can't delete the secret: %+v", res)
	}
	if !strings.Contains(res.Warning, "hardware") {
		t.Fatalf("warning must name the backend the secret was left in: %q", res.Warning)
	}
	if names, _ := env.m.Names(); len(names) != 0 {
		t.Fatalf("forced removal must drop the record, got %v", names)
	}
}

// TestRemoveDeleteErrorNeedsForce covers a known, reachable backend whose Delete
// errors (e.g. a transient keystore failure): same escape-hatch semantics.
func TestRemoveDeleteErrorNeedsForce(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", "file"); err != nil {
		t.Fatal(err)
	}
	env.stores["file"].DeleteErr = func(string) error { return errors.New("keystore busy") }

	if _, err := env.m.Remove("a", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("no-force removal must error pointing at --force when Delete fails: %v", err)
	}
	if names, _ := env.m.Names(); len(names) != 1 {
		t.Fatalf("failed no-force removal must leave the record, got %v", names)
	}

	res, err := env.m.Remove("a", true)
	if err != nil {
		t.Fatalf("forced removal past a Delete error must succeed: %v", err)
	}
	if res.SecretRemoved {
		t.Fatalf("Delete errored, so the secret was not removed: %+v", res)
	}
	if res.Warning == "" || !strings.Contains(res.Warning, "file") {
		t.Fatalf("warning must report the stranded secret and its backend: %q", res.Warning)
	}
	if names, _ := env.m.Names(); len(names) != 0 {
		t.Fatalf("forced removal must drop the record, got %v", names)
	}
}

// TestRemoveForceOnHealthyKeyIsCleanRemove asserts --force on a sound key behaves
// exactly like a normal remove: the secret is deleted, SecretRemoved is true, and
// no warning is set.
func TestRemoveForceOnHealthyKeyIsCleanRemove(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "keychain")
	res, err := env.m.Remove("a", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SecretRemoved || res.Warning != "" {
		t.Fatalf("force on a healthy key must be a clean full removal, got %+v", res)
	}
	if env.stores["keychain"].Has("a") {
		t.Fatal("force on a healthy key must still delete the secret from its backend")
	}
}

// TestRemoveForceUnsetsDefaultNoFallback asserts the no-fallback invariant holds
// under a forced removal too: removing the default (stranded) key unsets the
// default rather than re-pointing it at another key.
func TestRemoveForceUnsetsDefaultNoFallback(t *testing.T) {
	env := newEnv(t, "file")
	writeRecord(t, env.m, TypeEd25519, "hardware") // "a" is the default per writeRecord
	if _, err := env.m.Add("b", "", "file"); err != nil {
		t.Fatal(err)
	}
	res, err := env.m.Remove("a", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.DefaultKey != nil {
		t.Fatalf("forced removal of the default must unset it (no fallback), got %v", *res.DefaultKey)
	}
	// "b" exists but must not be silently promoted.
	if _, err := env.m.Resolve(""); err == nil {
		t.Fatal("after forced default removal, resolve with no --key must fail")
	}
}

func TestSetKeystoreBackendStamps(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "")
	if err := env.m.SetKeystoreBackend("a", "keychain"); err != nil {
		t.Fatal(err)
	}
	if s, _ := env.m.Show("a"); s.Keystore != "keychain" {
		t.Fatalf("record not re-pointed: %+v", s)
	}
	if err := env.m.SetKeystoreBackend("missing", "file"); err == nil {
		t.Fatal("unknown key should fail")
	}
}

func TestInvalidName(t *testing.T) {
	m := newManager(t)
	for _, bad := range []string{"", "-bad", "has space", "with/slash"} {
		if _, err := m.Add(bad, "", ""); err == nil {
			t.Errorf("expected invalid name rejected: %q", bad)
		}
	}
}

func TestBaseURLLifecycle(t *testing.T) {
	m := newManager(t)
	if _, err := m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	// Default: no override.
	if got, err := m.BaseURLOf("a"); err != nil || got != "" {
		t.Fatalf("BaseURLOf fresh key = %q, %v; want empty", got, err)
	}
	if got := m.MetaBaseURL("a"); got != "" {
		t.Fatalf("MetaBaseURL fresh key = %q; want empty", got)
	}

	// Set REST + WS, with trailing slashes trimmed.
	if err := m.SetBaseURL("a", "https://alt.example.test/", "wss://ws-alt.example.test/"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.BaseURLOf("a"); got != "https://alt.example.test" {
		t.Fatalf("BaseURLOf after set = %q; want trimmed", got)
	}
	// MetaBaseURL via default (a is the sole/default key) and via explicit name.
	if got := m.MetaBaseURL(""); got != "https://alt.example.test" {
		t.Fatalf("MetaBaseURL(default) = %q", got)
	}
	if got := m.MetaBaseURL("a"); got != "https://alt.example.test" {
		t.Fatalf("MetaBaseURL(explicit) = %q", got)
	}
	// The WS companion round-trips (trimmed) via MetaWSBaseURL and the summary.
	if got := m.MetaWSBaseURL("a"); got != "wss://ws-alt.example.test" {
		t.Fatalf("MetaWSBaseURL(explicit) = %q", got)
	}
	if s, _ := m.Show("a"); s.BaseURL != "https://alt.example.test" || s.WSBaseURL != "wss://ws-alt.example.test" {
		t.Fatalf("Show() = %q / %q", s.BaseURL, s.WSBaseURL)
	}

	// Clear reverts both to default.
	if err := m.ClearBaseURL("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.BaseURLOf("a"); got != "" {
		t.Fatalf("BaseURLOf after clear = %q; want empty", got)
	}
	if got := m.MetaWSBaseURL("a"); got != "" {
		t.Fatalf("MetaWSBaseURL after clear = %q; want empty", got)
	}
}

func TestSetBaseURLValidates(t *testing.T) {
	m := newManager(t)
	m.Add("a", "", "")
	for _, bad := range []string{"", "ftp://x", "notaurl", "://nohost"} {
		if err := m.SetBaseURL("a", bad, ""); err == nil {
			t.Errorf("SetBaseURL(%q) should fail", bad)
		}
	}
	// A valid REST URL but an invalid WS companion must also fail.
	for _, badWS := range []string{"https://not-ws.test", "ftp://x", "://nohost"} {
		if err := m.SetBaseURL("a", "https://x.test", badWS); err == nil {
			t.Errorf("SetBaseURL ws=%q should fail", badWS)
		}
	}
	// An empty WS companion is allowed (derived at use time).
	if err := m.SetBaseURL("a", "https://x.test", ""); err != nil {
		t.Fatalf("SetBaseURL with empty ws should succeed: %v", err)
	}
	if err := m.SetBaseURL("missing", "https://x.test", ""); err == nil {
		t.Fatalf("SetBaseURL on unknown key should fail")
	}
}

func TestMetaBaseURLBestEffort(t *testing.T) {
	m := newManager(t)
	// No keys at all: empty, no panic/error.
	if got := m.MetaBaseURL(""); got != "" {
		t.Fatalf("MetaBaseURL with no keys = %q", got)
	}
	m.Add("a", "", "")
	// Unknown explicit name: empty (canonical error comes later from Resolve).
	if got := m.MetaBaseURL("nope"); got != "" {
		t.Fatalf("MetaBaseURL(unknown) = %q", got)
	}
}

// TestRenamePreservesEverything is the happy path: the keypair, binding, default
// status, and per-key base URL all carry over, and the secret moves in the vault.
func TestRenamePreservesEverything(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil { // becomes the default
		t.Fatal(err)
	}
	if err := env.m.Bind("a", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	if err := env.m.SetBaseURL("a", "https://api-test.digitalx.miraeasset.com", "wss://ws-api-test.digitalx.miraeasset.com"); err != nil {
		t.Fatal(err)
	}
	before, _ := env.m.Show("a")

	res, err := env.m.Rename("a", "b")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !res.IsDefault || !res.SecretMoved || res.Warning != "" {
		t.Fatalf("rename result: %+v", res)
	}
	// Old name gone from registry AND vault; new name present in both.
	if names, _ := env.m.Names(); len(names) != 1 || names[0] != "b" {
		t.Fatalf("after rename, names = %v", names)
	}
	if env.stores["file"].Has("a") {
		t.Fatal("old secret must be deleted from the vault")
	}
	if !env.stores["file"].Has("b") {
		t.Fatal("secret must exist under the new name")
	}
	after, err := env.m.Show("b")
	if err != nil {
		t.Fatal(err)
	}
	if !after.IsDefault {
		t.Fatal("default must follow the rename")
	}
	if after.PublicKey != before.PublicKey {
		t.Fatal("keypair must be preserved")
	}
	if after.APIKeyID == nil || *after.APIKeyID != "KEYID-1" {
		t.Fatalf("binding must be preserved, got %v", after.APIKeyID)
	}
	if after.BaseURL != "https://api-test.digitalx.miraeasset.com" || after.WSBaseURL != "wss://ws-api-test.digitalx.miraeasset.com" {
		t.Fatalf("per-key base URLs must be preserved, got %q / %q", after.BaseURL, after.WSBaseURL)
	}
	// The renamed key still signs (the moved secret is intact).
	if _, err := env.m.Resolve("b"); err != nil {
		t.Fatalf("renamed key must resolve: %v", err)
	}
}

func TestRenameRejectsCollisionAndSameName(t *testing.T) {
	m := newManager(t)
	m.Add("a", "", "")
	m.Add("b", "", "")
	if _, err := m.Rename("a", "b"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("rename onto an existing name must error: %v", err)
	}
	if _, err := m.Rename("a", "a"); err == nil {
		t.Fatal("rename to the same name must error")
	}
	if _, err := m.Rename("missing", "x"); err == nil {
		t.Fatal("rename of an unknown key must error")
	}
	if _, err := m.Rename("a", "bad name!"); err == nil {
		t.Fatal("rename to an invalid name must error")
	}
	// Nothing changed.
	if names, _ := m.Names(); len(names) != 2 {
		t.Fatalf("failed renames must leave keys untouched, got %v", names)
	}
}

// TestRenameVerifyFailureRollsBack: a vault that silently corrupts the copy must
// abort before the registry commit and leave the key under its old name.
func TestRenameVerifyFailureRollsBack(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "")
	env.m.Bind("a", "KEYID-1")
	env.stores["file"].Tamper = map[string]string{"b": "corrupted"}

	if _, err := env.m.Rename("a", "b"); err == nil || !strings.Contains(err.Error(), "read back") {
		t.Fatalf("a verify mismatch must abort the rename: %v", err)
	}
	if names, _ := env.m.Names(); len(names) != 1 || names[0] != "a" {
		t.Fatalf("a failed rename must leave the key under its old name, got %v", names)
	}
	if env.stores["file"].Has("b") {
		t.Fatal("the in-flight copy must be rolled back")
	}
	if _, err := env.m.Resolve("a"); err != nil {
		t.Fatalf("the original key must still resolve: %v", err)
	}
}

// TestRenameUnavailableBackendFails: a key whose backend can't be reached can't be
// renamed (the secret would be orphaned) — the error points at remove --force.
func TestRenameUnavailableBackendFails(t *testing.T) {
	env := newEnv(t, "keychain")
	env.m.Add("a", "", "keychain")
	env.m.Bind("a", "KEYID-1")
	env.probeErr["keychain"] = errors.New("keyring locked")

	_, err := env.m.Rename("a", "b")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rename with an unavailable backend must error pointing at remove --force: %v", err)
	}
	if names, _ := env.m.Names(); len(names) != 1 || names[0] != "a" {
		t.Fatalf("the key must be left untouched, got %v", names)
	}
}

// TestRenamePostCommitDeleteWarns: when the source-delete after commit fails, the
// rename has still succeeded but warns about the orphaned old vault entry.
func TestRenamePostCommitDeleteWarns(t *testing.T) {
	env := newEnv(t, "file")
	env.m.Add("a", "", "")
	env.m.Bind("a", "KEYID-1")
	env.stores["file"].DeleteErr = func(name string) error {
		if name == "a" {
			return errors.New("delete boom")
		}
		return nil
	}
	res, err := env.m.Rename("a", "b")
	if err != nil {
		t.Fatalf("rename must succeed even if the post-commit delete fails: %v", err)
	}
	if res.Warning == "" || !strings.Contains(res.Warning, "a") {
		t.Fatalf("a failed source-delete must warn naming the old entry: %q", res.Warning)
	}
	// The rename committed: registry points at "b" and it resolves.
	if names, _ := env.m.Names(); len(names) != 1 || names[0] != "b" {
		t.Fatalf("rename must have committed, got %v", names)
	}
	if _, err := env.m.Resolve("b"); err != nil {
		t.Fatalf("renamed key must resolve: %v", err)
	}
}

func TestDefaultAccountSeq(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}

	// Initially not configured.
	if got := env.m.MetaDefaultAccountSeq("a"); got != "" {
		t.Fatalf("MetaDefaultAccountSeq fresh key = %q; want empty", got)
	}

	// Set to 3.
	if err := env.m.SetDefaultAccountSeq("a", 3); err != nil {
		t.Fatal(err)
	}
	if got := env.m.MetaDefaultAccountSeq("a"); got != "3" {
		t.Fatalf("MetaDefaultAccountSeq after set = %q; want 3", got)
	}

	// MetaDefaultAccountSeq via default key (a is the sole/default key).
	if got := env.m.MetaDefaultAccountSeq(""); got != "3" {
		t.Fatalf("MetaDefaultAccountSeq(default) = %q; want 3", got)
	}

	// Show reflects it.
	s, err := env.m.Show("a")
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultAccountSeq == nil || *s.DefaultAccountSeq != 3 {
		t.Fatalf("Show().DefaultAccountSeq = %v; want *3", s.DefaultAccountSeq)
	}

	// Clear (pass 0).
	if err := env.m.SetDefaultAccountSeq("a", 0); err != nil {
		t.Fatal(err)
	}
	if got := env.m.MetaDefaultAccountSeq("a"); got != "" {
		t.Fatalf("MetaDefaultAccountSeq after clear = %q; want empty", got)
	}
	s2, _ := env.m.Show("a")
	if s2.DefaultAccountSeq != nil {
		t.Fatalf("Show().DefaultAccountSeq after clear = %v; want nil", s2.DefaultAccountSeq)
	}

	// Unknown key returns "".
	if got := env.m.MetaDefaultAccountSeq("nonexistent"); got != "" {
		t.Fatalf("MetaDefaultAccountSeq unknown key = %q; want empty", got)
	}

	// SetDefaultAccountSeq on unknown key fails.
	if err := env.m.SetDefaultAccountSeq("nonexistent", 2); err == nil {
		t.Fatal("SetDefaultAccountSeq on unknown key should fail")
	}

	// Negative value fails.
	if err := env.m.SetDefaultAccountSeq("a", -1); err == nil {
		t.Fatal("SetDefaultAccountSeq with -1 should fail")
	}
}
