// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package notices

import (
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/stream"
)

func sampleData() Data {
	return Data{Notices: []stream.Notice{
		{Code: stream.ConnectionUnreliable, Level: stream.LevelWarn, Message: "stream is lagging", Time: 1_700_000_000_000},
		{Code: stream.Connected, Level: stream.LevelInfo, Message: "private connected", Time: 1_700_000_001_000},
	}}
}

func key() Key {
	return Key{NoticeRev: 1, W: 60, H: 12}
}

func TestLinesReturnsInnerLines(t *testing.T) {
	m := New()
	lines := m.Lines(key(), sampleData())
	if len(lines) != 2 {
		t.Fatalf("Lines = %d, want 2 (one per notice); got %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "stream is lagging") {
		t.Errorf("first line should be the first (newest-first) notice; got %q", lines[0])
	}
	if !strings.Contains(lines[0], string(stream.ConnectionUnreliable)) {
		t.Errorf("a line should show the notice code; got %q", lines[0])
	}
	empty := m.Lines(Key{NoticeRev: 2, W: 60, H: 12}, Data{})
	if len(empty) != 1 || !strings.Contains(empty[0], "no notices") {
		t.Errorf("empty Lines should be the single placeholder; got %q", empty)
	}
}

func TestMemoHitOnUnchangedKey(t *testing.T) {
	m := New()
	k := key()
	first := strings.Join(m.Lines(k, sampleData()), "\n")
	// Different DATA but same KEY → cache hit returns the first render (the
	// revision in the key is the contract for "data changed").
	other := sampleData()
	other.Notices[0].Message = "totally different"
	if got := strings.Join(m.Lines(k, other), "\n"); got != first {
		t.Error("unchanged key should return the cached render")
	}
	// Bumping the revision in the key forces a re-render that reflects new data.
	k.NoticeRev = 2
	if got := strings.Join(m.Lines(k, other), "\n"); !strings.Contains(got, "totally different") {
		t.Error("after a NoticeRev bump the render should reflect the new notices")
	}
}
