// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

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
// raw bytes — exactly what Korbit verifies (crypto.createHmac("sha256", secret)
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
// korbit.Credentials, so every fmt verb must render the redaction placeholder,
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

func publicKeyFromPEM(t *testing.T, pemStr string) ed25519.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public: %v", err)
	}
	return parsed.(ed25519.PublicKey)
}
