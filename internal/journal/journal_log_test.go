// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package journal

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/logging"
)

// TestOpenLogsPragmaAndMilestone asserts Open emits the routine pragma Debug
// line and the "action journal opened" Info milestone with the path.
func TestOpenLogsPragmaAndMilestone(t *testing.T) {
	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), DefaultFileName)
	l, err := Open(path, true /*noFsync*/, logging.New(&buf, slog.LevelDebug))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	out := buf.String()
	for _, want := range []string{
		"opening action journal",
		"journal pragmas applied",
		"synchronous=OFF", // the --no-fsync case
		"action journal opened",
		path,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Open log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// TestWritesLogDebug asserts the api_calls and order-row writes emit a routine
// Debug line (no secret payload — params are pre-signing and not logged here).
func TestWritesLogDebug(t *testing.T) {
	var buf bytes.Buffer
	path := filepath.Join(t.TempDir(), DefaultFileName)
	l, err := Open(path, false, logging.New(&buf, slog.LevelDebug))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	buf.Reset()

	if _, err := l.LogCall(CallRecord{Method: "GET", Path: "/v2/time", BaseURL: "http://x", Success: true}); err != nil {
		t.Fatalf("LogCall: %v", err)
	}
	id, err := l.StartOrder(OrderStart{CreatedAtMs: 1, ClientOrderID: "coid-1", Symbol: "btc_krw"})
	if err != nil {
		t.Fatalf("StartOrder: %v", err)
	}
	if err := l.FinishOrder(id, OrderFinish{FinishedAtMs: 2, Status: "accepted", OrderID: "ord-9"}); err != nil {
		t.Fatalf("FinishOrder: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"journal api_calls row written",
		"journal order intent recorded",
		"clientOrderId=coid-1",
		"journal order finished",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write log missing %q\n--- log ---\n%s", want, out)
		}
	}
}
