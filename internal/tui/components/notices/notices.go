// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package notices renders the stream-notices list shown in the TUI's 'n' popup:
// recent notices, newest first, each line "HH:MM:SS CODE message" with the code
// colored by its level (warn yellow, error red, otherwise dim). It is a pure view
// component — [Model.Lines] is a function of its explicit [Key] and [Data], owns
// no shared state, and caches its render via [uikit.Memo] keyed on [Key]. The
// parent supplies the data and a [Key] whose notice revision stands in for "did
// the notices change", so an unrelated frame reuses the cached render. The notice
// styles are fixed ANSI colors, so the palette identity is not part of the Key.
package notices

import (
	"strings"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. NoticeRev is
// the store's notice-section revision (a new notice bumps it ⇒ re-render); W and
// H participate in the key but do not affect the rendered lines (see Lines).
type Key struct {
	NoticeRev uint64
	W, H      int
}

// Data is what the component renders on a cache miss: the notices to draw,
// newest first. The NoticeRev in the Key decides the cache hit.
type Data struct {
	Notices []stream.Notice
}

// Model is the notices component. The zero value is ready to use; its render is
// cached (keyed on [Key]) so an unchanged key returns the previous frame.
type Model struct {
	lineMemo uikit.Memo[Key] // the newline-joined inner lines
}

// New returns a notices component.
func New() *Model { return &Model{} }

// Lines returns the notice lines (newest first), which the caller embeds in the
// notices overlay. Only the notices and their count affect the lines; W and H in
// the key are tolerated (they only over-invalidate, never serve a stale list).
func (m *Model) Lines(k Key, d Data) []string {
	return strings.Split(m.lineMemo.Do(k, func() string {
		return strings.Join(noticeLines(d), "\n")
	}), "\n")
}

// noticeLines builds one line per notice: a dim clock, the code colored by
// level, then the message. An empty list renders a single dim placeholder.
func noticeLines(d Data) []string {
	if len(d.Notices) == 0 {
		return []string{uikit.StyDim.Render(i18n.T("no notices"))}
	}
	lines := make([]string, 0, len(d.Notices))
	for _, n := range d.Notices {
		sty := uikit.StyDim
		switch n.Level {
		case stream.LevelWarn:
			sty = uikit.StyWarn
		case stream.LevelError:
			sty = uikit.StyErr
		}
		lines = append(lines, uikit.StyDim.Render(uikit.FmtClock(n.Time))+" "+sty.Render(string(n.Code))+" "+n.Message)
	}
	return lines
}
