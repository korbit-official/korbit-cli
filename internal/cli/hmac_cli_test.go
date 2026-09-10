// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliHMACSecret = "ZwKS2evdxj9j3Neir2s0UHAmpNFfo4a0iHawEElGCBs"

// assertHMACSignedQuery recomputes the HMAC-SHA256 over the exact sent query
// (everything before the appended &signature=) and checks the wire signature
// matches — the way Digital X verifies it. Returns the signed-over string.
func assertHMACSignedQuery(t *testing.T, rawQuery, secret string) {
	t.Helper()
	idx := strings.LastIndex(rawQuery, "&signature=")
	if idx < 0 {
		t.Fatalf("query has no appended signature: %q", rawQuery)
	}
	signed := rawQuery[:idx]
	sigParam := rawQuery[idx+len("&signature="):]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	want := hex.EncodeToString(mac.Sum(nil))
	if sigParam != want {
		t.Fatalf("hmac signature mismatch:\n signed=%q\n   got=%q\n  want=%q", signed, sigParam, want)
	}
}

// TestKeyAddHMACThenSigns adds an hmac-sha256 key from a secret file (with a
// stray trailing newline, which must be trimmed) and confirms a later signed
// command produces a hex HMAC over the sent bytes keyed by that secret.
func TestKeyAddHMACThenSigns(t *testing.T) {
	home := t.TempDir()
	secretFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secretFile, []byte(cliHMACSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, code := runCLI(
		[]string{"key", "add", "hmac-bot", "--type", "hmac-sha256", "--api-key", "KEYID-7", "--secret-file", secretFile, "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("key add hmac exit=%d stderr=%s", code, errb)
	}

	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil)}
	_, errb, code = runCLI(
		[]string{"balance", "--key", "hmac-bot", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("balance exit=%d stderr=%s", code, errb)
	}
	if doer.last.Header.Get("x-kapi-key") != "KEYID-7" {
		t.Fatalf("x-kapi-key = %q, want KEYID-7", doer.last.Header.Get("x-kapi-key"))
	}
	assertHMACSignedQuery(t, doer.last.URL.RawQuery, cliHMACSecret)
}

// TestInlineHMACCredentialSigns supplies a full credential via the environment
// (no keystore) and confirms it signs end-to-end, with the right key id header
// and the (environment) signing disclosure.
func TestInlineHMACCredentialSigns(t *testing.T) {
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil)}
	env := map[string]string{
		"DIGITALX_CLI_API_KEY_ID":     "INLINE-KEY",
		"DIGITALX_CLI_API_KEY_SECRET": cliHMACSecret,
		"DIGITALX_CLI_API_KEY_TYPE":   "hmac-sha256",
	}
	_, errb, code := runCLI([]string{"balance", "--compact"}, env, doer)
	if code != 0 {
		t.Fatalf("inline balance exit=%d stderr=%s", code, errb)
	}
	if doer.last.Header.Get("x-kapi-key") != "INLINE-KEY" {
		t.Fatalf("x-kapi-key = %q, want INLINE-KEY", doer.last.Header.Get("x-kapi-key"))
	}
	assertHMACSignedQuery(t, doer.last.URL.RawQuery, cliHMACSecret)
	if !strings.Contains(errb, "(environment)") {
		t.Fatalf("signing disclosure should name the inline credential: %q", errb)
	}
}

// TestInlineEd25519CredentialSigns: the inline path also carries an ED25519
// PKCS#8 PEM; the wire signature must verify against the keypair's public key.
func TestInlineEd25519CredentialSigns(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"krw":{"available":"1"}}}`, nil)}
	env := map[string]string{
		"DIGITALX_CLI_API_KEY_ID":     "INLINE-ED",
		"DIGITALX_CLI_API_KEY_SECRET": pemStr,
		"DIGITALX_CLI_API_KEY_TYPE":   "ed25519",
	}
	_, errb, code := runCLI([]string{"balance", "--compact"}, env, doer)
	if code != 0 {
		t.Fatalf("inline ed25519 balance exit=%d stderr=%s", code, errb)
	}
	q := doer.last.URL.RawQuery
	idx := strings.LastIndex(q, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature in query: %q", q)
	}
	signed := q[:idx]
	unesc, _ := url.QueryUnescape(q[idx+len("&signature="):])
	sig, err := base64.StdEncoding.DecodeString(unesc)
	if err != nil {
		t.Fatalf("ed25519 signature not base64: %v", err)
	}
	if !ed25519.Verify(pub, []byte(signed), sig) {
		t.Fatal("inline ED25519 signature did not verify against the public key")
	}
}

// TestInlineAndFlagMutuallyExclusive: inline env material together with --key is
// a usage error (exit 2), enforced before any work.
func TestInlineAndFlagMutuallyExclusive(t *testing.T) {
	env := map[string]string{
		"DIGITALX_CLI_API_KEY_ID":     "X",
		"DIGITALX_CLI_API_KEY_SECRET": "s",
		"DIGITALX_CLI_API_KEY_TYPE":   "hmac-sha256",
	}
	_, errb, code := runCLI([]string{"balance", "--key", "bot", "--compact"}, env, &stubDoer{})
	if code != 2 {
		t.Fatalf("expected usage exit 2 for mutual exclusion, got %d (%s)", code, errb)
	}
	if !strings.Contains(errb, "mutually exclusive") && !strings.Contains(errb, "not both") {
		t.Fatalf("error should explain the mutual exclusion: %q", errb)
	}
}

// TestKeyAddHMACRequiresApiKeyAndSecret pins the usage guards.
func TestKeyAddHMACRequiresApiKeyAndSecret(t *testing.T) {
	home := t.TempDir()
	// Missing --api-key.
	_, _, code := runCLI(
		[]string{"key", "add", "h", "--type", "hmac-sha256", "--secret-file", "/dev/null", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, &stubDoer{})
	if code != 2 {
		t.Fatalf("missing --api-key should be usage error, got %d", code)
	}
	// Missing --secret-file.
	_, _, code = runCLI(
		[]string{"key", "add", "h", "--type", "hmac-sha256", "--api-key", "KEYID-7", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, &stubDoer{})
	if code != 2 {
		t.Fatalf("missing --secret-file should be usage error, got %d", code)
	}
}
