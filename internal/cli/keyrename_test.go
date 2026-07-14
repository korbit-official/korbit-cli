// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
)

// TestKeyRenamePreservesBindingAndDefault: renaming the default bound key keeps
// its binding and default status under the new name, and the old name disappears.
func TestKeyRenamePreservesBindingAndDefault(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home) // bound default key "bot" in the file backend

	out, stderr, code := runCLI([]string{"key", "rename", "bot", "main", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("rename exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"oldName":"bot"`) || !strings.Contains(out, `"newName":"main"`) {
		t.Fatalf("rename output: %s", out)
	}
	if !strings.Contains(out, `"isDefault":true`) {
		t.Fatalf("default must follow the rename: %s", out)
	}

	list, _, _ := runCLI([]string{"key", "list", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if strings.Contains(list, `"name":"bot"`) {
		t.Fatalf("old name must be gone: %s", list)
	}
	if !strings.Contains(list, `"name":"main"`) || !strings.Contains(list, `"defaultKey":"main"`) {
		t.Fatalf("new name must be present and default: %s", list)
	}
	if !strings.Contains(list, `"apiKeyId":"KEYID-1"`) {
		t.Fatalf("binding must be preserved through the rename: %s", list)
	}
}

// TestKeyRenameRejectsCollision: renaming onto an existing name is a usage error
// (exit 2) and leaves both keys untouched.
func TestKeyRenameRejectsCollision(t *testing.T) {
	home := t.TempDir()
	m := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)
	if _, err := m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("b", "", ""); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI([]string{"key", "rename", "a", "b"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 2 {
		t.Fatalf("collision must be a usage error (exit 2), got %d — %s", code, stderr)
	}
	if names, _ := m.Names(); len(names) != 2 {
		t.Fatalf("a failed rename must leave keys untouched, got %v", names)
	}
}

// TestKeyAddWithAPIKeyBindsImmediately: importing a key with --api-key binds it in
// one step — the result is bound, carries the id, and prints no registration link.
func TestKeyAddWithAPIKeyBindsImmediately(t *testing.T) {
	home := t.TempDir()
	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(pemPath, []byte(kp.PrivatePEM), 0o600); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runCLI(
		[]string{"key", "add", "imported", "--from-pem-file", pemPath, "--api-key", "KEYID-XYZ", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("add --api-key exit = %d — %s", code, stderr)
	}
	if !strings.Contains(out, `"bound":true`) || !strings.Contains(out, `"apiKeyId":"KEYID-XYZ"`) {
		t.Fatalf("key must be bound at creation: %s", out)
	}
	if strings.Contains(out, "registrationUrl") || strings.Contains(out, "registrationLink") {
		t.Fatalf("a bound import must not print a registration link: %s", out)
	}
	if !strings.Contains(out, "Verify it works") {
		t.Fatalf("stdout should carry the next step: %s", out)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Fatalf("bound key import must not duplicate its result on stderr: %s", stderr)
	}
}
