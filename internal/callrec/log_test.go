// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package callrec

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// TestReadyLogsJournalingDecision asserts the per-call Debug line explains
// whether the call is journaled and why — and that a public, non-debug call is
// logged as a skip without opening the journal.
func TestReadyLogsJournalingDecision(t *testing.T) {
	t.Run("authed cli call is journaled", func(t *testing.T) {
		var buf bytes.Buffer
		r := New(journal.DefaultPath(t.TempDir()), false, false, DefaultPolicy(false), nil, nil)
		r.Log = logging.New(&buf, slog.LevelDebug)
		cr := r.ForCall("")
		if err := cr.Ready(apiclient.CallInfo{Origin: apiclient.Origin{Surface: "cli"}, Auth: true, Method: "POST", Path: "/v2/orders"}); err != nil {
			t.Fatalf("Ready: %v", err)
		}
		defer r.Close()
		out := buf.String()
		for _, want := range []string{"journaling decision", "record=true", "lazily opening journal", "action journal opened"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q\n--- log ---\n%s", want, out)
			}
		}
	})

	t.Run("public non-debug call is skipped, no open", func(t *testing.T) {
		var buf bytes.Buffer
		r := New(journal.DefaultPath(t.TempDir()), false, false, DefaultPolicy(false), nil, nil)
		r.Log = logging.New(&buf, slog.LevelDebug)
		cr := r.ForCall("")
		if err := cr.Ready(apiclient.CallInfo{Origin: apiclient.Origin{Surface: "cli"}, Auth: false, Method: "GET", Path: "/v2/time"}); err != nil {
			t.Fatalf("Ready: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "record=false") || !strings.Contains(out, "read, not in debug") {
			t.Errorf("expected skip-with-reason\n--- log ---\n%s", out)
		}
		if strings.Contains(out, "action journal opened") {
			t.Errorf("public skip must not open the journal\n--- log ---\n%s", out)
		}
		if r.Opened() {
			t.Error("public skip opened the journal DB")
		}
	})

	t.Run("exempt surface logs the surface reason, not read/write", func(t *testing.T) {
		var buf bytes.Buffer
		r := New(journal.DefaultPath(t.TempDir()), false, false, DefaultPolicy(false), nil, nil)
		r.Log = logging.New(&buf, slog.LevelDebug)
		// A doctor write would record on any other surface, but doctor is exempt —
		// the reason must name the exemption, not "read, not in debug".
		if err := r.ForCall("").Ready(apiclient.CallInfo{Origin: apiclient.Origin{Surface: apiclient.SurfaceDoctor}, Auth: true, Method: "POST", Path: "/v2/orders"}); err != nil {
			t.Fatalf("Ready: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "record=false") || !strings.Contains(out, "surface never journaled") {
			t.Errorf("expected the surface-exemption reason\n--- log ---\n%s", out)
		}
		if r.Opened() {
			t.Error("exempt surface opened the journal DB")
		}
	})
}
