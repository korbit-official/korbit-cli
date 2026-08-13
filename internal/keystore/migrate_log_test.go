// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keystore"
	"github.com/korbit-official/korbit-cli/internal/keystore/keystoretest"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// TestMigrateLogsStepsNoSecret traces the copy → verify → commit → delete
// sequence at Debug and asserts the moved secret never appears in the log.
func TestMigrateLogsStepsNoSecret(t *testing.T) {
	const secret = "SUPER_SECRET_MIGRATE_PEM_do_not_log"
	src := keystoretest.New("file")
	src.Seed("a", secret)
	dst := keystoretest.New("keychain")

	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelDebug)

	rep, err := keystore.Migrate(
		[]keystore.MigrateItem{{Name: "a", Source: src}},
		dst,
		func(string) error { return nil },
		keystore.MigrateOptions{},
		log,
	)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(rep.Moved) != 1 {
		t.Fatalf("Moved = %v", rep.Moved)
	}

	out := buf.String()
	for _, want := range []string{
		"migrate: starting",
		"migrate: copying material into target",
		"migrate: read-back verified, committing record",
		"migrate: deleting from source",
		"migrated key",
		"key=a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("migrate log missing %q\n--- log ---\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) || strings.Contains(out, "SUPER_SECRET") {
		t.Fatalf("secret leaked into migrate log:\n%s", out)
	}
}
