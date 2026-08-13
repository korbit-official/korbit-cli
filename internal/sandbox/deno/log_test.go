// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package deno

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// TestInstallLogsSHA256VerifyAndInstall asserts the good-path install logs the
// asset URL, the sha256 verify pass, and the install milestone.
func TestInstallLogsSHA256VerifyAndInstall(t *testing.T) {
	cache := t.TempDir()
	zipBytes, sha := fakeZip(t, "#!/bin/sh\necho fake-deno\n")
	calls := 0
	var buf bytes.Buffer
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: sha}, func() int64 { return 1700000000000 }, nil, logging.New(&buf, slog.LevelDebug))
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"downloading managed Deno",
		"managed Deno sha256 verify ok",
		"managed Deno installed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// TestInstallLogsSHA256MismatchRefusal asserts a tampered download is refused
// with a Warn "sha256 verify MISMATCH" line and nothing is installed.
func TestInstallLogsSHA256MismatchRefusal(t *testing.T) {
	cache := t.TempDir()
	zipBytes, _ := fakeZip(t, "#!/bin/sh\necho fake-deno\n")
	calls := 0
	var buf bytes.Buffer
	// Pin a checksum that won't match the served bytes (32 zero bytes hex).
	m := NewManager(cache, serveBytes(zipBytes, &calls), Config{SHA256: strings.Repeat("00", 32)}, func() int64 { return 1 }, nil, logging.New(&buf, slog.LevelDebug))
	if _, err := m.Ensure(context.Background()); err == nil {
		t.Fatal("expected a checksum-mismatch error, got nil")
	}
	out := buf.String()
	if !strings.Contains(out, "sha256 verify MISMATCH") || !strings.Contains(out, "refusing to install") {
		t.Errorf("expected a refusal log\n--- log ---\n%s", out)
	}
	if strings.Contains(out, "managed Deno installed") {
		t.Errorf("a tampered download must not log an install\n--- log ---\n%s", out)
	}
}
