// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// realSecret is a real-looking ED25519 PKCS#8 PEM used to prove no secret
// material ever reaches the log buffer. Its bytes must never appear in a log.
const realSecret = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIBSUPER_SECRET_KEY_MATERIAL_DO_NOT_LOG_abcdefgh
-----END PRIVATE KEY-----`

// assertNoSecret fails if any recognizable fragment of the secret leaked into
// the buffer.
func assertNoSecret(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	out := buf.String()
	for _, frag := range []string{"SUPER_SECRET", "PRIVATE KEY", "MC4CAQAw", realSecret} {
		if strings.Contains(out, frag) {
			t.Fatalf("secret material leaked into log:\n%s", out)
		}
	}
}

func TestFileKeystoreLogsLifecycleNoSecret(t *testing.T) {
	var buf bytes.Buffer
	ks := NewFile(t.TempDir())
	ks.Log = logging.New(&buf, slog.LevelDebug)

	if err := ks.Set("trader", realSecret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ks.Get("trader")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != realSecret {
		t.Fatalf("round-trip mismatch")
	}
	if err := ks.Delete("trader"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"vault set",
		"vault lock: acquiring",
		"vault lock: acquired",
		"vault write: committed",
		"vault read: entry decrypted",
		"vault delete",
		"key=trader",
		"backend=file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
	assertNoSecret(t, &buf)
}

func TestFileKeystoreLogsDecryptFailureNoSecret(t *testing.T) {
	var buf bytes.Buffer
	ks := NewFile(t.TempDir())
	ks.Log = logging.New(&buf, slog.LevelDebug)
	if err := ks.Set("trader", realSecret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	buf.Reset()

	// Corrupt the entry in place: tamper the ciphertext so the auth tag fails.
	file, err := ks.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e := file.Entries["trader"]
	e.Data = "AAAA" + e.Data
	file.Entries["trader"] = e
	if err := ks.save(file); err != nil {
		t.Fatalf("save: %v", err)
	}
	buf.Reset()

	if _, err := ks.Get("trader"); err == nil {
		t.Fatal("expected a decrypt error on a corrupt entry")
	}
	out := buf.String()
	if !strings.Contains(out, "vault decrypt failed") {
		t.Errorf("expected a decrypt-failure warn log, got:\n%s", out)
	}
	if !strings.Contains(out, "warn:") {
		t.Errorf("decrypt failure should log at warn level, got:\n%s", out)
	}
	assertNoSecret(t, &buf)
}

// TestFileKeystoreNilLoggerSilent proves a nil Log is safe (no panic, no output).
func TestFileKeystoreNilLoggerSilent(t *testing.T) {
	ks := NewFile(t.TempDir())
	if err := ks.Set("k", realSecret); err != nil {
		t.Fatalf("Set with nil logger panicked or errored: %v", err)
	}
	if _, err := ks.Get("k"); err != nil {
		t.Fatalf("Get with nil logger: %v", err)
	}
}
