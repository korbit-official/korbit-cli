// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package keystore

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// TestKeychainGoKeyringWireFormat pins the on-disk value format to the portable
// OS-keyring library's macOS encoding, so the keychain backend can fall back to
// that library without losing access to keys this binary stored. If this test
// fails, the two are no longer interchangeable.
func TestKeychainGoKeyringWireFormat(t *testing.T) {
	secret := "-----BEGIN PRIVATE KEY-----\nabc/def+ghi==\n-----END PRIVATE KEY-----\n"

	// set stores exactly "go-keyring-base64:" + base64(secret).
	got := encodeSecret(secret)
	want := "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(secret))
	if got != want {
		t.Fatalf("encodeSecret = %q, want %q", got, want)
	}

	// Round-trips through our own decode.
	if back, err := decodeSecret(got); err != nil || back != secret {
		t.Fatalf("decodeSecret(encodeSecret(x)) = %q, %v; want %q", back, err, secret)
	}

	// Decodes a value the library wrote with the base64 prefix (with the trailing
	// newline the `security` CLI tends to append — TrimSpace must absorb it).
	libBase64 := "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(secret)) + "\n"
	if back, err := decodeSecret(libBase64); err != nil || back != secret {
		t.Fatalf("decode(lib base64) = %q, %v; want %q", back, err, secret)
	}

	// Decodes the library's hex variant too.
	libHex := "go-keyring-encoded:" + hex.EncodeToString([]byte(secret))
	if back, err := decodeSecret(libHex); err != nil || back != secret {
		t.Fatalf("decode(lib hex) = %q, %v; want %q", back, err, secret)
	}

	// A bare (unprefixed) value is returned as-is (trimmed), matching the
	// library's fallback branch.
	if back, err := decodeSecret("plain"); err != nil || back != "plain" {
		t.Fatalf("decode(bare) = %q, %v; want %q", back, err, "plain")
	}
}
