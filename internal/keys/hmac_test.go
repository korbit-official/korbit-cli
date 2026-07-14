// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"encoding/hex"
	"strings"
	"testing"
)

const hmacSecret = "ZwKS2evdxj9j3Neir2s0UHAmpNFfo4a0iHawEElGCBs"

// TestAddHMACBornBound checks that an HMAC key is created already bound, with no
// public key, type hmac-sha256, and becomes the default as the first real key.
func TestAddHMACBornBound(t *testing.T) {
	env := newEnv(t, "file")
	nk, err := env.m.AddHMAC("hmac-bot", hmacSecret, "KEYID-9", "")
	if err != nil {
		t.Fatalf("AddHMAC: %v", err)
	}
	if nk.Type != TypeHMACSHA256 || !nk.IsDefault || nk.PublicKey != "" {
		t.Fatalf("unexpected NewKey: %+v", nk)
	}
	s, err := env.m.Show("hmac-bot")
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != TypeHMACSHA256 || !s.Bound || s.PublicKey != "" {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if got, _ := env.stores["file"].Get("hmac-bot"); got != hmacSecret {
		t.Fatalf("secret not stored verbatim")
	}
}

func TestAddHMACRejectsEmpty(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddHMAC("hmac-bot", "  ", "KEYID-9", ""); err == nil {
		t.Fatal("empty secret should be rejected")
	}
	if _, err := env.m.AddHMAC("hmac-bot", hmacSecret, "", ""); err == nil {
		t.Fatal("empty api-key should be rejected")
	}
}

// TestResolveHMACBuildsHexSigner verifies an HMAC key resolves to a signer whose
// wire form is lowercase-hex HMAC-SHA256 keyed by the stored secret's bytes.
func TestResolveHMACBuildsHexSigner(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddHMAC("hmac-bot", "key", "KEYID-9", ""); err != nil {
		t.Fatal(err)
	}
	resolved, err := env.m.Resolve("hmac-bot")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Type != TypeHMACSHA256 || resolved.APIKeyID != "KEYID-9" {
		t.Fatalf("unexpected resolved: %+v", resolved)
	}
	signer, err := resolved.Signer()
	if err != nil {
		t.Fatal(err)
	}
	// Same vector as the korbit package's signer test, proving the seam wires the
	// secret bytes through unchanged.
	if got := signer.Sign("The quick brown fox jumps over the lazy dog"); got != "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8" {
		t.Fatalf("hmac signer wire form wrong: %q", got)
	}
}

func TestBindRefusesHMAC(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddHMAC("hmac-bot", hmacSecret, "KEYID-9", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"KEYID-9", "KEYID-10"} {
		err := env.m.Bind("hmac-bot", id)
		if err == nil {
			t.Fatalf("Bind(%q) should reject an hmac key", id)
		}
		if !strings.Contains(err.Error(), TypeEd25519) || !strings.Contains(err.Error(), TypeHMACSHA256) {
			t.Fatalf("Bind(%q) should explain the ed25519-only rule, got %v", id, err)
		}
	}
	s, err := env.m.Show("hmac-bot")
	if err != nil {
		t.Fatal(err)
	}
	if s.APIKeyID == nil || *s.APIKeyID != "KEYID-9" {
		t.Fatalf("failed bind must preserve original api-key id, got %+v", s.APIKeyID)
	}
}

// TestRenameAndRemoveHMAC: the type-agnostic lifecycle ops work on hmac keys.
func TestRenameAndRemoveHMAC(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.AddHMAC("hmac-bot", hmacSecret, "KEYID-9", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.Rename("hmac-bot", "hmac-bot2"); err != nil {
		t.Fatalf("Rename hmac: %v", err)
	}
	if got, _ := env.stores["file"].Get("hmac-bot2"); got != hmacSecret {
		t.Fatalf("secret did not move on rename")
	}
	if _, err := env.m.Remove("hmac-bot2", false); err != nil {
		t.Fatalf("Remove hmac: %v", err)
	}
	if env.stores["file"].Has("hmac-bot2") {
		t.Fatalf("secret not deleted on remove")
	}
}

// fakeEnv builds a getenv from a map for the selection tests.
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestSelectMutualExclusion(t *testing.T) {
	// Inline material + a stored-name selection (flag or env) is a usage error.
	cases := []struct {
		name    string
		flagKey string
		env     map[string]string
	}{
		{"flag + inline id", "mykey", map[string]string{EnvAPIKeyID: "X"}},
		{"flag + inline secret", "mykey", map[string]string{EnvAPIKeySecret: "S"}},
		{"env-name + inline type", "", map[string]string{EnvKeyName: "mykey", EnvAPIKeyType: "ed25519"}},
	}
	for _, c := range cases {
		if _, err := Select(c.flagKey, fakeEnv(c.env)); err == nil {
			t.Fatalf("%s: expected mutual-exclusion error", c.name)
		}
	}
}

func TestSelectNamedAndInline(t *testing.T) {
	// Flag beats env-name.
	sel, err := Select("flagkey", fakeEnv(map[string]string{EnvKeyName: "envkey"}))
	if err != nil || sel.Inline || sel.Name != "flagkey" {
		t.Fatalf("flag should win: %+v %v", sel, err)
	}
	// Env-name when no flag.
	sel, _ = Select("", fakeEnv(map[string]string{EnvKeyName: "envkey"}))
	if sel.Inline || sel.Name != "envkey" {
		t.Fatalf("env-name should select: %+v", sel)
	}
	// Nothing => default key (empty name, not inline).
	sel, _ = Select("", fakeEnv(nil))
	if sel.Inline || sel.Name != "" {
		t.Fatalf("empty selection expected: %+v", sel)
	}
	// Any inline var => inline.
	sel, _ = Select("", fakeEnv(map[string]string{EnvAPIKeyID: "X"}))
	if !sel.Inline {
		t.Fatalf("inline expected: %+v", sel)
	}
}

func TestResolveInlineHMAC(t *testing.T) {
	env := newEnv(t, "file")
	get := fakeEnv(map[string]string{
		EnvAPIKeyID:     "INLINE-KEY",
		EnvAPIKeySecret: "key",
		EnvAPIKeyType:   TypeHMACSHA256,
	})
	sel, err := Select("", get)
	if err != nil || !sel.Inline {
		t.Fatalf("inline selection: %+v %v", sel, err)
	}
	resolved, err := env.m.ResolveSelection(sel, get)
	if err != nil {
		t.Fatalf("ResolveSelection inline: %v", err)
	}
	if resolved.APIKeyID != "INLINE-KEY" || resolved.Type != TypeHMACSHA256 {
		t.Fatalf("unexpected inline resolved: %+v", resolved)
	}
	signer, _ := resolved.Signer()
	want, _ := hex.DecodeString("f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8")
	if signer.Sign("The quick brown fox jumps over the lazy dog") != hex.EncodeToString(want) {
		t.Fatal("inline hmac signer wire form wrong")
	}
}

func TestResolveInlineEd25519(t *testing.T) {
	env := newEnv(t, "file")
	// A valid ED25519 PEM via the keystore round-trip would be heavy; reuse the
	// keystore by adding a key, reading its PEM back, then feeding it inline.
	if _, err := env.m.Add("seed", "", ""); err != nil {
		t.Fatal(err)
	}
	pem, _ := env.stores["file"].Get("seed")
	get := fakeEnv(map[string]string{
		EnvAPIKeyID:     "INLINE-KEY",
		EnvAPIKeySecret: pem,
		EnvAPIKeyType:   TypeEd25519,
	})
	sel, _ := Select("", get)
	resolved, err := env.m.ResolveSelection(sel, get)
	if err != nil {
		t.Fatalf("ResolveSelection inline ed25519: %v", err)
	}
	if signer, serr := resolved.Signer(); serr != nil || signer == nil {
		t.Fatalf("inline ed25519 signer: %v", serr)
	}
}

func TestResolveInlineMissingOrBadType(t *testing.T) {
	env := newEnv(t, "file")
	// Missing type.
	get := fakeEnv(map[string]string{EnvAPIKeyID: "X", EnvAPIKeySecret: "s"})
	sel, _ := Select("", get)
	if _, err := env.m.ResolveSelection(sel, get); err == nil || !strings.Contains(err.Error(), EnvAPIKeyType) {
		t.Fatalf("missing type should error naming the var, got %v", err)
	}
	// Missing id.
	get = fakeEnv(map[string]string{EnvAPIKeySecret: "s", EnvAPIKeyType: TypeHMACSHA256})
	sel, _ = Select("", get)
	if _, err := env.m.ResolveSelection(sel, get); err == nil || !strings.Contains(err.Error(), EnvAPIKeyID) {
		t.Fatalf("missing id should error naming the var, got %v", err)
	}
	// Unknown type.
	get = fakeEnv(map[string]string{EnvAPIKeyID: "X", EnvAPIKeySecret: "s", EnvAPIKeyType: "rsa"})
	sel, _ = Select("", get)
	if _, err := env.m.ResolveSelection(sel, get); err == nil {
		t.Fatal("unknown inline type should error")
	}
}
