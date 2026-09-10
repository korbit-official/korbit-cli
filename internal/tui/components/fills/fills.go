// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package fills renders the recent-fills pane: one row per fill, newest first,
// each showing the local time, symbol, side (buy/sell, side-colored), price, and
// quantity. It is a pure view component — [Model.View] is a function of its
// explicit [Key] and [Data], owns no shared state, and caches its render via
// [uikit.Memo] keyed on [Key]. The parent supplies the data and a [Key] whose
// store revisions stand in for "did the fills/health change", so an unrelated
// frame reuses the cached render.
package fills

import (
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/i18n"
	"github.com/digitalx-official/digitalx-cli/internal/stream/state"
	"github.com/digitalx-official/digitalx-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. FillRev and
// HealthRev are the store's section revisions (fills/health change ⇒ rev change
// ⇒ re-render); AccountSeq is the active sub-account whose fills Data carries —
// it MUST be in the key because a pure account switch changes which fills are
// shown (FillsFor) without bumping FillRev, so without it the memo would keep
// the previous account's render; PrivateUp gates the loading state (the fills
// are append-only with no re-snapshot, so freshness is just whether the private
// feed is up); W and H are the OUTER panel size; Style is the palette identity.
type Key struct {
	FillRev    uint64
	HealthRev  uint64
	AccountSeq int
	PrivateUp  bool
	W, H       int
	Style      uikit.StyleID
}

// Data is what the component renders on a cache miss. Fills is the recent-fills
// slice, newest first; a change to it bumps FillRev so the Key still decides the
// hit.
type Data struct {
	Fills []state.Fill
}

// Model is the fills component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a fills component.
func New() *Model { return &Model{} }

// View renders the full bordered pane for k/d, reusing the cached render when k
// is unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	title := i18n.T("fills")
	// The private feed being down could mean recent rows are missing, so hide
	// the fills rather than imply they are current.
	if !k.PrivateUp {
		return uikit.Panel(title, []string{uikit.StyDim.Render(i18n.T("loading…"))}, k.W, k.H)
	}
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)
	w := k.W - 2    // inner content width (panel border)
	rows := k.H - 3 // content rows (panel border + title)
	return uikit.Panel(title, fillLines(w, rows, d, pal), k.W, k.H)
}

// fillLines builds the inner rows, newest first, windowed to rows.
func fillLines(w, rows int, d Data, pal uikit.Palette) []string {
	fills := d.Fills
	if len(fills) == 0 {
		return []string{uikit.StyDim.Render(i18n.T("no fills yet"))}
	}
	if len(fills) > rows {
		fills = fills[:rows]
	}
	// Layout: time(8) + symbol + side(4) + price + qty, four single-space gaps.
	symW := clamp(w/5, 6, 9)
	rest := w - 8 - 4 - symW - 4
	if rest < 8 {
		rest = 8
	}
	priceW := rest / 2
	qtyW := rest - priceW
	lines := make([]string, 0, len(fills))
	for _, f := range fills {
		sty := pal.Up.Fg
		if f.Side == "sell" {
			sty = pal.Down.Fg
		}
		lines = append(lines, strings.Join([]string{
			uikit.StyDim.Render(uikit.FmtClock(f.Time)),
			uikit.PadRight(uikit.FmtSymbol(f.Symbol), symW),
			sty.Render(uikit.PadRight(f.Side, 4)),
			uikit.PadLeft(uikit.ClipTail(uikit.GroupThousands(f.Price), priceW), priceW),
			uikit.PadLeft(uikit.ClipTail(f.Qty, qtyW), qtyW),
		}, " "))
	}
	return lines
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
