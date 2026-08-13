// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strings"

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

// ed25519SPKIPrefix is the fixed 12-byte ASN.1 DER header of the X.509
// SubjectPublicKeyInfo that wraps an Ed25519 public key (RFC 8410): the
// AlgorithmIdentifier for id-Ed25519 (OID 1.3.101.112) then the 32-byte key
// BIT STRING. A complete Ed25519 SPKI is therefore exactly 44 bytes.
var ed25519SPKIPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// ed25519PKCS8Prefix is the fixed 16-byte ASN.1 DER header of the PKCS#8
// PrivateKeyInfo that wraps an Ed25519 private key (RFC 8410): version 0, the
// id-Ed25519 AlgorithmIdentifier, then the 32-byte seed as an OCTET STRING inside
// an OCTET STRING. A complete Ed25519 PKCS#8 key is therefore exactly 48 bytes.
var ed25519PKCS8Prefix = []byte{0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20}

func isEd25519SPKI(der []byte) bool {
	return len(der) == len(ed25519SPKIPrefix)+ed25519.PublicKeySize &&
		bytes.Equal(der[:len(ed25519SPKIPrefix)], ed25519SPKIPrefix)
}

func isEd25519PKCS8(der []byte) bool {
	return len(der) == len(ed25519PKCS8Prefix)+ed25519.SeedSize &&
		bytes.Equal(der[:len(ed25519PKCS8Prefix)], ed25519PKCS8Prefix)
}

// derFromKeyMaterial decodes s as a PEM block or as the bare base64 body of a DER
// blob — either alphabet (standard or URL-safe), padded or not, embedded
// whitespace/newlines tolerated — and returns the raw DER, or nil if s is not
// decodable as key material. The alphabets only disagree on the special
// characters (+/ vs -_), and a given special character is accepted by exactly one
// of them, so the first successful decode is the intended bytes.
func derFromKeyMaterial(s string) []byte {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return block.Bytes
	}
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if der, err := enc.DecodeString(compact); err == nil {
			return der
		}
	}
	return nil
}

// LooksLikeEd25519PublicKey reports whether s is, or encodes, an Ed25519 public
// key rather than an opaque identifier. It recognizes a PUBLIC KEY PEM block and
// the bare base64 body of an Ed25519 SPKI DER (see derFromKeyMaterial for the
// accepted encodings). It exists so a key-binding path can reject a public key
// pasted where the portal-issued api-key id belongs — the two are easy to
// confuse because the CLI prints the public key right beside the registration
// step.
func LooksLikeEd25519PublicKey(s string) bool { return isEd25519SPKI(derFromKeyMaterial(s)) }

// LooksLikeEd25519PrivateKey reports whether s is, or encodes, an Ed25519 private
// key (a PRIVATE KEY PEM block or the bare base64 body of its PKCS#8 DER). It
// lets a key-binding path reject private key material pasted where an api-key id
// belongs — a worse mistake than a public key, since it would write a secret into
// non-secret metadata.
func LooksLikeEd25519PrivateKey(s string) bool { return isEd25519PKCS8(derFromKeyMaterial(s)) }

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
