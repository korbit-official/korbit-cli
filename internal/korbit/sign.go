// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"

	"github.com/korbit-official/korbit-cli/internal/output"
)

// Keypair is a generated ED25519 keypair in PEM form. The public key is SPKI;
// the private key is PKCS#8.
type Keypair struct {
	PublicPEM  string
	PrivatePEM string
}

// GenerateKeypair creates a fresh ED25519 keypair.
func GenerateKeypair() (Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Keypair{}, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Keypair{}, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return Keypair{}, err
	}
	return Keypair{
		PublicPEM:  pemEncode("PUBLIC KEY", pubDER),
		PrivatePEM: pemEncode("PRIVATE KEY", privDER),
	}, nil
}

// ParsePrivatePEM decodes a PKCS#8 PEM and asserts it is an ED25519 key. The
// error messages never include key bytes.
func ParsePrivatePEM(pemStr string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, &output.UsageError{Message: "not a valid private key PEM"}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, &output.UsageError{Message: "not a valid private key PEM"}
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, &output.UsageError{
			Message: "expected an ED25519 private key — this CLI supports ED25519 keys only",
		}
	}
	return key, nil
}

// PublicPEMFromPrivate derives the SPKI public-key PEM from a private-key PEM.
func PublicPEMFromPrivate(privatePEM string) (string, error) {
	key, err := ParsePrivatePEM(privatePEM)
	if err != nil {
		return "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", err
	}
	return pemEncode("PUBLIC KEY", pubDER), nil
}

// AssertEd25519PrivatePEM validates a PEM without exposing the key.
func AssertEd25519PrivatePEM(privatePEM string) error {
	_, err := ParsePrivatePEM(privatePEM)
	return err
}

// PublicSPKIBase64URL returns the base64url (no padding) of the SPKI DER inside a
// PUBLIC KEY PEM — the transport form the developers portal's key-creation deep
// link expects. base64url is used deliberately: a standard-base64 '+' can be
// turned into a space by URL query parsing and corrupt the key, while base64url's
// alphabet ('-', '_', alphanumerics) is URL-safe and survives the round trip. The
// portal re-pads and re-wraps it into a PEM.
func PublicSPKIBase64URL(publicPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return "", &output.UsageError{Message: "not a valid public key PEM"}
	}
	return base64.RawURLEncoding.EncodeToString(block.Bytes), nil
}

// SignParams signs the exact encoded parameter string (with `signature`
// excluded) and returns the base64 signature to append last. Korbit verifies
// over the raw encoded bytes in sent order, so the caller must sign exactly
// what it sends and never re-encode afterwards.
func SignParams(key ed25519.PrivateKey, encodedParams string) string {
	sig := ed25519.Sign(key, []byte(encodedParams))
	return base64.StdEncoding.EncodeToString(sig)
}

// Signer produces the value of the `signature` parameter for an encoded
// parameter string, the way the server verifies it. It is the abstraction over
// the supported key types: ED25519 returns a base64 detached signature,
// HMAC-SHA256 a lowercase-hex MAC. Both sign EXACTLY the bytes the caller sends,
// so a Signer must never re-encode its input. A Signer carries secret material,
// so it redacts itself under every fmt verb — it rides inside Credentials, which
// diagnostics may render.
type Signer interface {
	Sign(encodedParams string) string
}

// NewEd25519Signer returns a Signer that produces a base64 ED25519 signature.
func NewEd25519Signer(key ed25519.PrivateKey) Signer { return ed25519Signer{key} }

// NewHMACSHA256Signer returns a Signer that produces a lowercase-hex
// HMAC-SHA256 MAC. The secret's raw bytes are the HMAC key (Korbit issues the
// secret as a text string; its UTF-8 bytes are used verbatim).
func NewHMACSHA256Signer(secret []byte) Signer { return hmacSHA256Signer{secret} }

// signerRedacted is what a Signer renders as under any fmt verb, so a stray
// %v/%+v/%#v of a Credentials can never dump key material.
const signerRedacted = "korbit.Signer(REDACTED)"

// SchemeOf reports the signing scheme of a Signer ("ed25519", "hmac-sha256", or
// "unknown") for diagnostics. It reveals only the algorithm name, never any key
// material — safe to log.
func SchemeOf(s Signer) string {
	switch s.(type) {
	case ed25519Signer:
		return "ed25519"
	case hmacSHA256Signer:
		return "hmac-sha256"
	default:
		return "unknown"
	}
}

type ed25519Signer struct{ key ed25519.PrivateKey }

func (s ed25519Signer) Sign(encodedParams string) string { return SignParams(s.key, encodedParams) }
func (ed25519Signer) String() string                     { return signerRedacted }
func (ed25519Signer) GoString() string                   { return signerRedacted }

type hmacSHA256Signer struct{ secret []byte }

func (s hmacSHA256Signer) Sign(encodedParams string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(encodedParams))
	return hex.EncodeToString(mac.Sum(nil))
}
func (hmacSHA256Signer) String() string   { return signerRedacted }
func (hmacSHA256Signer) GoString() string { return signerRedacted }

func pemEncode(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}
