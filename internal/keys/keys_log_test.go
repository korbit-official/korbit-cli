// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// TestManagerLogsLifecycleNoSecret exercises create → bind → use → resolve on a
// Manager with a Debug logger, asserting the lifecycle milestones are logged
// with non-secret identifiers (key name, backend, api-key id) and that no
// private key material reaches the buffer. The keypair is generated, so the
// secret never enters the test as a literal — we additionally plant a
// recognizable secret in the backing store and assert it never appears.
func TestManagerLogsLifecycleNoSecret(t *testing.T) {
	env := newEnv(t, "file")
	var buf bytes.Buffer
	env.m.Log = logging.New(&buf, slog.LevelDebug)

	if _, err := env.m.Add("trader", "", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Whatever PEM Add generated and stored, capture it and confirm it is never
	// logged by any subsequent operation.
	planted, err := env.stores["file"].Get("trader")
	if err != nil {
		t.Fatalf("read planted secret: %v", err)
	}
	if err := env.m.Bind("trader", "APIKEY-PUBLIC-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := env.m.Use("trader"); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if _, err := env.m.Resolve("trader"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"key created",
		"key bound to api-key id",
		"default key set",
		"resolve: signer built",
		"registry write: committed",
		"registry lock: acquired",
		"key=trader",
		"apiKeyId=APIKEY-PUBLIC-1",
		"backend=file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manager log missing %q\n--- log ---\n%s", want, out)
		}
	}
	if planted == "" {
		t.Fatal("expected a generated PEM in the file backend")
	}
	if strings.Contains(out, planted) || strings.Contains(out, "PRIVATE KEY") {
		t.Fatalf("private key material leaked into the manager log:\n%s", out)
	}
}

// TestManagerLoadLogsRecordCount proves the registry-load Debug names the path
// and the record count, so a --debug run explains how many keys were read.
func TestManagerLoadLogsRecordCount(t *testing.T) {
	env := newEnv(t, "file")
	if _, err := env.m.Add("a", "", ""); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if _, err := env.m.Add("b", "", ""); err != nil {
		t.Fatalf("Add b: %v", err)
	}

	var buf bytes.Buffer
	env.m.Log = logging.New(&buf, slog.LevelDebug)
	env.m.Invalidate()
	if _, err := env.m.Names(); err != nil {
		t.Fatalf("Names: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registry load: parsed") || !strings.Contains(out, "records=2") {
		t.Errorf("expected a parsed-records=2 debug log, got:\n%s", out)
	}
}
