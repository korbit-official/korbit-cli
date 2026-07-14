// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"os"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/keystore"
)

// TestPurgeKeysRemovesAll pins that a data purge clears every key's material from
// its backend, empties the registry, AND deletes the key files — the destructive
// core of `self uninstall` removing your keys, with no orphaned files left.
func TestPurgeKeysRemovesAll(t *testing.T) {
	home := t.TempDir()
	m := keys.NewManager(home, "file", func() int64 { return 1 }, nil)
	if _, err := m.Add("a", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("b", "", ""); err != nil {
		t.Fatal(err)
	}
	keyFiles := []string{keys.RegistryPath(home), keystore.FilePath(home)}

	removed, warnings := purgeKeys(m, keyFiles)
	if len(warnings) != 0 {
		t.Errorf("clean file-backed removal should warn about nothing, got %v", warnings)
	}
	// 2 API keys + the 2 key files.
	if len(removed) != 4 {
		t.Errorf("expected 2 key lines + 2 file lines removed, got %v", removed)
	}
	for _, p := range keyFiles {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("key file %s survived the purge (err=%v)", p, err)
		}
	}
	sums, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 0 {
		t.Errorf("keys remain after purge: %v", sums)
	}
}

// TestPurgeKeysEmpty pins that a purge with no keys (and no key files) is a clean
// no-op.
func TestPurgeKeysEmpty(t *testing.T) {
	home := t.TempDir()
	m := keys.NewManager(home, "file", func() int64 { return 1 }, nil)
	removed, warnings := purgeKeys(m, []string{keys.RegistryPath(home), keystore.FilePath(home)})
	if len(removed) != 0 || len(warnings) != 0 {
		t.Errorf("empty purge should be a no-op, got removed=%v warnings=%v", removed, warnings)
	}
}
