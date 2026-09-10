// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/logging"
)

func TestSyncerLogsInstallAtInfo(t *testing.T) {
	var buf strings.Builder
	st := New(func() int64 { return 1000 })
	s := NewSyncer(st, func() (int64, int64, error) { return 500, 30, nil }, func() int64 { return 0 }, DefaultCoolDownMs)
	s.Log = logging.New(&buf, slog.LevelDebug)

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "info: clock corrected") || !strings.Contains(out, "offsetMs=500") {
		t.Fatalf("missing Info install line\n--- log ---\n%s", out)
	}
}

func TestSyncerLogsRecvWindowWiden(t *testing.T) {
	var buf strings.Builder
	st := New(func() int64 { return 0 })
	// lean 1000 => raw recvWindow = 6*1000 = 6000 > 5000 default, so it widens.
	s := NewSyncer(st, func() (int64, int64, error) { return 0, 1000, nil }, func() int64 { return 0 }, DefaultCoolDownMs)
	s.Log = logging.New(&buf, slog.LevelInfo)

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "recvWindow auto-widened") || !strings.Contains(out, "recvWindowMs=6000") {
		t.Fatalf("missing recvWindow widen line\n--- log ---\n%s", out)
	}
}

func TestSyncerLogsFailureAtWarn(t *testing.T) {
	var buf strings.Builder
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) { return 0, 0, errors.New("unreachable host") }, func() int64 { return 0 }, DefaultCoolDownMs)
	s.Log = logging.New(&buf, slog.LevelDebug)

	if err := s.Sync(); err == nil {
		t.Fatal("expected a measurement error")
	}
	out := buf.String()
	if !strings.Contains(out, "warn: clock sync failed") || !strings.Contains(out, "unreachable host") {
		t.Fatalf("missing Warn failure line\n--- log ---\n%s", out)
	}
}

func TestSyncerLogsCooldownReuse(t *testing.T) {
	var buf strings.Builder
	now := int64(1000)
	st := New(func() int64 { return 0 })
	s := NewSyncer(st, func() (int64, int64, error) { return 1, 1, nil }, func() int64 { return now }, 500)
	s.Log = logging.New(&buf, slog.LevelDebug)

	if err := s.Sync(); err != nil { // measures
		t.Fatal(err)
	}
	now = 1200 // within cooldown
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "reusing recent estimate") {
		t.Fatalf("missing cooldown-reuse line\n--- log ---\n%s", out)
	}
}
