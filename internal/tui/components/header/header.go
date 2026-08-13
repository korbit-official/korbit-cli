// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package header renders the two-line page header: line one names the app, the
// active-symbol count, and a right-aligned key/base-URL (with a terminal-size
// warning chip beside it when the parent flags a cramped terminal); line two is
// the active symbol's ticker summary (last/bid/ask/24h range/volume) or a
// loading line. It
// is a pure view component — [Model.View] is a function of its explicit [Key]
// and [Data], owns no shared state, and caches its render via [uikit.Memo] keyed
// on [Key]. The parent supplies the data and a [Key] whose ticker revision
// stands in for "did the ticker change", so an unrelated frame reuses the
// cached render.
package header

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. TickerRev is
// the store's ticker section revision (ticker change ⇒ rev change ⇒ re-render);
// Symbol is the active market; MarketCount is the number of subscribed symbols;
// KeyName and BaseURL feed the right-aligned identity; Cramped is the parent's
// terminal-size warning ("" = none), shown as a chip beside the identity; W is
// the line width the right side is aligned against; Style is the palette
// identity.
type Key struct {
	TickerRev   uint64
	Symbol      string
	MarketCount int
	KeyName     string
	BaseURL     string
	// AccountSeq is the active sub-account, shown as an "accountSeq:N" chip beside
	// the key (0 = public/none, chip omitted). AccountCount is how many sub-accounts
	// the session subscribed; when >1 the chip renders underlined as a clickable
	// switch target, else dim.
	AccountSeq   int
	AccountCount int
	Cramped      string
	W            int
	Style        uikit.StyleID
}

// Data is what the component renders on a cache miss. HasTicker mirrors the
// store's "ok" return and TickerReady gates the loading state; the ticker first
// arriving bumps TickerRev, so the Key still decides the hit.
type Data struct {
	Ticker      state.Ticker
	HasTicker   bool
	TickerReady bool
}

// Model is the header component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a header component.
func New() *Model { return &Model{} }

// View renders the two-line header for k/d, reusing the cached render when k is
// unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

// acctChip is the sub-account chip text ("accountSeq:N"), or "" when there is
// none (public, or no active account). The switch-target cue is its style
// (underline), not its text — see render.
func acctChip(k Key) string {
	if k.KeyName == "" || k.AccountSeq <= 0 {
		return ""
	}
	return i18n.T("accountSeq:") + strconv.Itoa(k.AccountSeq)
}

// AcctChipSpan reports the screen column span [start, start+width) of the acct
// chip on the header's first line, so a click on it can be hit-tested (the TUI
// opens the sub-account switcher on such a click). ok is false when there is no
// chip or it is clipped off the right edge. It mirrors render's alignment (the
// same left/right/gap math) so the drawn chip and the hit-test can't drift.
func AcctChipSpan(k Key) (start, width int, ok bool) {
	chip := acctChip(k)
	if chip == "" {
		return 0, 0, false
	}
	left := "korbit-cli tui" + "  " + i18n.T("%d markets", k.MarketCount)
	right := chip + "  " + i18n.T("key:") + k.KeyName + "  " + k.BaseURL
	prefix := 0 // visible cells before the chip within the right block
	if k.Cramped != "" {
		right = k.Cramped + "  " + right
		prefix = lipgloss.Width(k.Cramped) + 2
	}
	gap := k.W - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	start = lipgloss.Width(left) + gap + prefix
	if start >= k.W {
		return 0, 0, false // clipped by the Truncate to W in render
	}
	return start, lipgloss.Width(chip), true
}

func render(k Key, d Data) string {
	pal := uikit.PaletteFor(uikit.ColorScheme(k.Style.Scheme), k.Style.Profile)

	// The symbol list lives in the sidebar; the header names the app and the
	// active symbol count, with the key/endpoint on the right.
	left := uikit.StyTitle.Render("korbit-cli tui") + uikit.StyDim.Render("  "+i18n.T("%d markets", k.MarketCount))
	right := ""
	if k.KeyName != "" {
		tail := i18n.T("key:") + k.KeyName + "  " + k.BaseURL
		if c := acctChip(k); c != "" {
			// Underline the chip (key-cap affordance) when it's a switch target — a
			// multi-account session; dim otherwise. Styling adds no visible cells, so
			// AcctChipSpan's width math is unaffected.
			chipSty := uikit.StyDim
			if k.AccountCount > 1 {
				chipSty = uikit.StyKey
			}
			right = chipSty.Render(c) + uikit.StyDim.Render("  "+tail)
		} else {
			right = uikit.StyDim.Render(tail)
		}
	} else {
		right = uikit.StyDim.Render(i18n.T("public mode") + "  " + k.BaseURL)
	}
	if k.Cramped != "" {
		right = uikit.StyWarn.Render(k.Cramped) + "  " + right
	}
	gap := k.W - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	// A crowded line clips at W rather than wrapping into the body's rows.
	line1 := uikit.Truncate(left+strings.Repeat(" ", gap)+right, k.W)

	line2 := uikit.StyDim.Render(i18n.T("loading…"))
	if d.HasTicker && d.TickerReady {
		t := d.Ticker
		dir := pal.Up.Fg
		arrow := "▲"
		if uikit.IsNegative(t.PriceChange) {
			dir, arrow = pal.Down.Fg, "▼"
		}
		line2 = fmt.Sprintf("%s  last %s %s  bid %s  ask %s  24h %s ~ %s  vol %s",
			uikit.StyTitle.Render(uikit.FmtSymbol(t.Symbol)),
			dir.Render(uikit.GroupThousands(t.Close)),
			dir.Render(arrow+t.PriceChangePercent+"%"),
			pal.Up.Fg.Render(uikit.GroupThousands(t.BestBidPrice)),
			pal.Down.Fg.Render(uikit.GroupThousands(t.BestAskPrice)),
			uikit.GroupThousands(t.Low), uikit.GroupThousands(t.High), t.Volume)
	}
	return line1 + "\n" + uikit.Truncate(line2, k.W)
}
