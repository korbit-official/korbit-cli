// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateAndParseRoundTrip(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(kp.PrivatePEM, "PRIVATE KEY") || !strings.Contains(kp.PublicPEM, "PUBLIC KEY") {
		t.Fatalf("unexpected PEM headers")
	}
	if _, err := ParsePrivatePEM(kp.PrivatePEM); err != nil {
		t.Fatalf("parse generated private: %v", err)
	}
	pub, err := PublicPEMFromPrivate(kp.PrivatePEM)
	if err != nil {
		t.Fatalf("derive public: %v", err)
	}
	if pub != kp.PublicPEM {
		t.Fatalf("derived public PEM != generated public PEM")
	}
}

func TestPublicSPKIBase64URLRoundTrip(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b64, err := PublicSPKIBase64URL(kp.PublicPEM)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// base64url alphabet only — never the '+'/'/' that URL query parsing mangles.
	if strings.ContainsAny(b64, "+/=") {
		t.Fatalf("expected base64url with no +,/,= : %q", b64)
	}
	der, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	block, _ := pem.Decode([]byte(kp.PublicPEM))
	if block == nil || string(der) != string(block.Bytes) {
		t.Fatalf("decoded SPKI DER does not match the public PEM body")
	}
	if _, err := PublicSPKIBase64URL("not a pem"); err == nil {
		t.Fatalf("expected error for invalid PEM")
	}

	// Transport-size guarantee for the developers-portal deep link: an ED25519
	// SPKI is 44 bytes of DER. base64url (unpadded) is 59 chars; the portal
	// re-pads to a multiple of 4 (60 chars) and wraps it, yielding the 112-char
	// PEM its create form validates. Pin both so a future encoding change can't
	// silently break the contract.
	if len(der) != 44 {
		t.Fatalf("ED25519 SPKI DER = %d bytes, want 44", len(der))
	}
	padded := b64
	for len(padded)%4 != 0 {
		padded += "="
	}
	if len(padded) != 60 {
		t.Fatalf("re-padded base64 SPKI = %d chars, want 60 (-> 112-char PEM)", len(padded))
	}
}

func TestParsePrivateRejectsNonEd25519AndGarbage(t *testing.T) {
	if err := AssertEd25519PrivatePEM("not a pem"); err == nil {
		t.Fatalf("expected error for garbage")
	}
	// An RSA-style block body is not ED25519 PKCS#8; must be rejected without
	// leaking key bytes.
	bad := pemEncode("PRIVATE KEY", []byte{0x01, 0x02, 0x03})
	err := AssertEd25519PrivatePEM(bad)
	if err == nil {
		t.Fatalf("expected error for non-ed25519")
	}
	if strings.ContainsAny(err.Error(), "\x01\x02\x03") {
		t.Fatalf("error must not contain key bytes: %q", err.Error())
	}
}

// TestSignWireBytes verifies the signature exactly the way the server does:
// strip the trailing `signature` segment and verify over the remaining encoded
// string in SENT order.
func TestSignWireBytes(t *testing.T) {
	kp, _ := GenerateKeypair()
	key, _ := ParsePrivatePEM(kp.PrivatePEM)

	p := &orderedParams{}
	p.add("symbol", "btc_krw")
	p.add("orderType", "limit")
	p.add("timestamp", "1700000000000")
	p.add("recvWindow", "5000")

	signedOver := p.encode()
	sig := SignParams(key, signedOver)
	p.add("signature", sig)

	sent := p.encode()
	if !strings.HasSuffix(sent, "&signature="+url.QueryEscape(sig)) {
		t.Fatalf("signature must be appended last; got %q", sent)
	}

	// Server-side: split off the signature segment, verify the rest in order.
	idx := strings.LastIndex(sent, "&signature=")
	rest := sent[:idx]
	if rest != signedOver {
		t.Fatalf("bytes verified != bytes signed:\n signed=%q\n  rest=%q", signedOver, rest)
	}
	sigBytes, _ := base64.StdEncoding.DecodeString(sig)

	pubKey := publicKeyFromPEM(t, kp.PublicPEM)
	if !ed25519.Verify(pubKey, []byte(rest), sigBytes) {
		t.Fatalf("signature did not verify over the sent bytes")
	}
}

// TestHMACSHA256SignerVector pins the HMAC-SHA256 signer to a well-known test
// vector (Wikipedia's HMAC example: key "key", the quick-brown-fox message),
// proving the wire form is lowercase hex of HMAC-SHA256 keyed by the secret's
// raw bytes — exactly what Digital X verifies (crypto.createHmac("sha256", secret)
// .update(query).digest("hex")).
func TestHMACSHA256SignerVector(t *testing.T) {
	s := NewHMACSHA256Signer([]byte("key"))
	got := s.Sign("The quick brown fox jumps over the lazy dog")
	const want = "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if got != want {
		t.Fatalf("HMAC-SHA256 signature = %q, want %q", got, want)
	}
}

// TestSignersRedactUnderFmt guards the leak-hardening: a Signer rides inside
// apiclient.Credentials, so every fmt verb must render the redaction placeholder,
// never key material.
func TestSignersRedactUnderFmt(t *testing.T) {
	kp, _ := GenerateKeypair()
	priv, _ := ParsePrivatePEM(kp.PrivatePEM)
	secret := []byte("super-secret-shared-key")
	for _, s := range []Signer{NewEd25519Signer(priv), NewHMACSHA256Signer(secret)} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(verb, s)
			if !strings.Contains(out, "REDACTED") {
				t.Fatalf("%s of signer should be redacted, got %q", verb, out)
			}
			if strings.Contains(out, string(secret)) {
				t.Fatalf("%s of signer leaked the secret: %q", verb, out)
			}
		}
	}
}

// TestLooksLikeEd25519PublicKey covers the guard that stops a public key from
// being pasted where the portal-issued api-key id belongs. The positive cases
// are the encodings a user could plausibly copy; the negatives are the shapes a
// real api-key id takes.
func TestLooksLikeEd25519PublicKey(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(kp.PublicPEM))
	der := block.Bytes // 44-byte SPKI

	positive := map[string]string{
		"full PEM":            kp.PublicPEM,
		"PEM body std base64": base64.StdEncoding.EncodeToString(der),
		// A wrapped PEM body: der[:21] is a whole number of base64 groups (21 is a
		// multiple of 3), so the two encoded halves concatenate back to the full
		// SPKI once whitespace is stripped.
		"PEM body with newlines":    base64.StdEncoding.EncodeToString(der[:21]) + "\n" + base64.StdEncoding.EncodeToString(der[21:]),
		"base64url no padding":      base64.RawURLEncoding.EncodeToString(der),
		"base64url with padding":    base64.URLEncoding.EncodeToString(der),
		"raw std base64 no padding": base64.RawStdEncoding.EncodeToString(der),
	}
	for name, in := range positive {
		if !LooksLikeEd25519PublicKey(in) {
			t.Errorf("%s: expected a public key to be detected", name)
		}
	}

	negative := map[string]string{
		"empty":                "",
		"portal-style id":      "dgx-ak-9f3c2b7e",
		"sandbox id":           "SANDBOX_ED25519_KEY_00000001_0000002",
		"uuid":                 "018f1a2b-3c4d-7e5f-8a9b-0c1d2e3f4a5b",
		"short base64":         base64.StdEncoding.EncodeToString([]byte("hello")),
		"32-byte raw key only": base64.StdEncoding.EncodeToString(der[len(der)-32:]),
	}
	for name, in := range negative {
		if LooksLikeEd25519PublicKey(in) {
			t.Errorf("%s: %q must not be flagged as a public key", name, in)
		}
	}
	// The public and private detectors must not cross-fire: a public key is not a
	// private key, and vice versa (different DER prefixes and lengths).
	if LooksLikeEd25519PrivateKey(kp.PublicPEM) {
		t.Error("a public key PEM must not be flagged as a private key")
	}
	if LooksLikeEd25519PublicKey(kp.PrivatePEM) {
		t.Error("a private key PEM must not be flagged as a public key")
	}
}

// TestLooksLikeEd25519PrivateKey covers the guard against pasting private key
// material where an api-key id belongs.
func TestLooksLikeEd25519PrivateKey(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(kp.PrivatePEM))
	der := block.Bytes // 48-byte PKCS#8

	positive := map[string]string{
		"full PEM":               kp.PrivatePEM,
		"PKCS#8 body std base64": base64.StdEncoding.EncodeToString(der),
		"base64url no padding":   base64.RawURLEncoding.EncodeToString(der),
	}
	for name, in := range positive {
		if !LooksLikeEd25519PrivateKey(in) {
			t.Errorf("%s: expected a private key to be detected", name)
		}
	}
	for _, in := range []string{"", "dgx-ak-9f3c2b7e", "SANDBOX_ED25519_KEY_00000001_0000002"} {
		if LooksLikeEd25519PrivateKey(in) {
			t.Errorf("%q must not be flagged as a private key", in)
		}
	}
}

func publicKeyFromPEM(t *testing.T, pemStr string) ed25519.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public: %v", err)
	}
	return parsed.(ed25519.PublicKey)
}
