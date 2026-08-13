// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// TestStartLogsLifecycleMilestones asserts a fake-runtime start emits the
// structured diagnostics a --debug run relies on: the resolved runtime, the
// detached spawn pid, a readiness poll, the pidfile read, and the "sandbox
// started" milestone with pid/port — plus a "sandbox stopped" on Stop.
func TestStartLogsLifecycleMilestones(t *testing.T) {
	home := t.TempDir()
	cacheDir := t.TempDir()

	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	apiKey := keys.SandboxAPIKeyPrefix + "ED25519_KEY_00000001_0000002"
	km := keys.NewManager(home, "file", func() int64 { return 1700000000000 }, nil)

	dbPid := filepath.Join(home, "sandbox", "korbit-sandbox.db-pid")
	denoBin := fakeDeno(t, dbPid, kp.PrivatePEM, apiKey)
	denoDir := filepath.Dir(denoBin)

	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})

	var buf bytes.Buffer
	cfg := Config{Home: home, CacheDir: cacheDir, RuntimePref: "deno"}
	deps := Deps{
		Doer:       doer,
		Now:        func() int64 { return 1700000000000 },
		LookPath:   func(name string) (string, error) { return filepath.Join(denoDir, name), nil },
		KeyManager: km,
		Logger:     logging.New(&buf, slog.LevelDebug),
	}
	m := New(cfg, deps)

	if _, err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"sandbox runtime resolved",
		"sandbox spawned detached",
		"sandbox readiness poll",
		"read sandbox pidfile",
		"sandbox started",
		"port=9999",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("start log missing %q\n--- log ---\n%s", want, out)
		}
	}

	buf.Reset()
	if _, err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(buf.String(), "sandbox stopped") {
		t.Errorf("expected a 'sandbox stopped' log\n--- log ---\n%s", buf.String())
	}
}
