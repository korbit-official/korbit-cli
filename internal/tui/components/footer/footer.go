// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package footer renders the two-line status footer: line one is the connection
// dots, stream-health counters, and the latest non-info notice; line two is the
// focus-specific key-hint, replaced by a transient toast while it is unexpired.
// It is a pure view component — [Model.View] is a function of its explicit [Key]
// and [Data], owns no shared state, and caches its render via [uikit.Memo] keyed
// on [Key]. The parent supplies the data and a [Key] whose store revisions stand
// in for "did health/notices change", so an unrelated frame reuses the cache.
package footer

import (
	"fmt"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/components/keystrip"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// Key is the comparable cache key: equal Keys render identically. HealthRev and
// NoticeRev are the store's section revisions (health/notices change ⇒ rev change
// ⇒ re-render). Private adds the private-endpoint dot; HasTrader (order entry is
// wired at all) selects the public vs. account hint line. Focus, OrdersAllPairs,
// Searching, SearchPane and CandlesWired select the rest of
// the hint. ToastText/ToastUntil/ToastIsError carry the transient status; NowSec
// is the current time in whole seconds, so the toast's expiry re-renders the
// footer even though no store revision changed. W is the terminal width (the
// truncation budget); Style is the palette identity. OverlayOpen means a modal
// overlay (confirm/cancel-all/help/notices/chart) owns the keyboard, so
// the main-screen keys are dead — the hint line then steps back to a plain,
// non-clickable reminder of the only key that still works in every mode.
type Key struct {
	HealthRev      uint64
	NoticeRev      uint64
	Private        bool
	HasTrader      bool
	Focus          int
	OrdersAllPairs bool
	// OrdersClosed means the orders pane shows its closed tab, so the hints
	// swap: no cancel target ('x' hidden) and 'o' returns to the open tab.
	OrdersClosed bool
	Searching    bool
	SearchPane   int
	CandlesWired bool
	FundingWired bool
	// MultiAccount is true when the session holds more than one sub-account, so
	// the '@' account-switcher hint is advertised (hidden for a single account).
	MultiAccount bool
	OverlayOpen  bool
	// OrderView is the order panel's view while order mode is open (1 form,
	// 2 confirm, 3 busy), 0 otherwise — it selects the order-mode hints. The
	// zero value deliberately means "order mode closed".
	OrderView int
	// LadderView is the trade ladder's view while ladder mode is open
	// (1 browse, 2 confirm place, 3 confirm cancel, 4 busy), 0 otherwise —
	// it selects the ladder-mode hints, the same shape as OrderView.
	LadderView int
	// SizeKeys is the pre-rendered size-preset key range for the ladder-browse
	// hint (e.g. "1-4"), derived from the configured levels so the footer never
	// disagrees with what a number key actually arms. A string (not the levels
	// slice) so Key stays comparable for the render cache.
	SizeKeys string
	// CmdBarOpen means the command bar owns the keyboard (its own echo line
	// carries the contextual hints, so the footer steps back to prose).
	CmdBarOpen   bool
	ToastText    string
	ToastUntil   int64
	ToastIsError bool
	NowSec       int64
	W            int
	Style        uikit.StyleID
}

// Data is what the component renders on a cache miss: the current health snapshot
// and the latest notice (its level/code/message). HasNotice mirrors the store's
// "a notice exists" return; the latest notice changing bumps NoticeRev, so the
// Key still decides the hit.
type Data struct {
	Health     state.Health
	HasNotice  bool
	NoticeCode stream.NoticeCode
	NoticeMsg  string
	NoticeLvl  stream.Level
}

// Focus identifiers the parent passes in [Key.Focus] / [Key.SearchPane]. They
// mirror the TUI's focus panes; only their integer identity matters here.
const (
	FocusMarket   = 0
	FocusOrders   = 1
	FocusBalances = 2
)

// Model is the footer component. The zero value is ready to use.
type Model struct {
	memo uikit.Memo[Key]
}

// New returns a footer component.
func New() *Model { return &Model{} }

// View renders the two-line footer for k/d, reusing the cached render when k is
// unchanged.
func (m *Model) View(k Key, d Data) string {
	return m.memo.Do(k, func() string { return render(k, d) })
}

func render(k Key, d Data) string {
	h := d.Health
	status := []string{
		connDot("pub", h.Public),
	}
	if k.Private {
		status = append(status, connDot("prv", h.Private))
	}
	if h.Backfilling {
		status = append(status, uikit.StyWarn.Render(i18n.T("backfilling…")))
	}
	if h.GapCount > 0 {
		status = append(status, uikit.StyWarn.Render(fmt.Sprintf("gaps:%d", h.GapCount)))
	}
	// Live degradation tags, derived from health (set by the warning notice,
	// cleared by its recovery notice) so they clear themselves when the
	// condition resolves — unlike the latest-notice slot below. Both conditions
	// mean "up but degraded", so a tag shows only while the endpoint is up; when
	// it is down the connection dot is the controlling signal (and a fatal
	// endpoint that never recovers does not strand a degradation tag).
	if degraded(h.Public, h.Private, func(e state.EndpointHealth) bool { return e.Delayed }) {
		status = append(status, uikit.StyWarn.Render(i18n.T("delayed")))
	}
	if degraded(h.Public, h.Private, func(e state.EndpointHealth) bool { return e.Unreliable }) {
		status = append(status, uikit.StyWarn.Render(i18n.T("unreliable")))
	}
	status = append(status, uikit.StyDim.Render(fmt.Sprintf("events:%d", h.DataCount)))
	// The latest-notice slot surfaces the most recent non-info notice, but it is
	// sticky (only a newer notice displaces it), so it skips the codes already
	// shown as self-clearing tags above — otherwise a resolved warning would
	// linger here after its tag has cleared.
	if d.HasNotice && d.NoticeLvl != stream.LevelInfo && !taggedStatusCode(d.NoticeCode) {
		text := fmt.Sprintf("%s %s", d.NoticeCode, d.NoticeMsg)
		sty := uikit.StyWarn
		if d.NoticeLvl == stream.LevelError {
			sty = uikit.StyErr
		}
		status = append(status, sty.Render(uikit.Truncate(text, k.W/2)))
	}
	line1 := uikit.Truncate(strings.Join(status, "  "), k.W)

	line2, _ := keystrip.Layout(footerItems(k), k.W)
	if k.ToastText != "" && k.NowSec*1000 < k.ToastUntil {
		sty := uikit.StyOK
		if k.ToastIsError {
			sty = uikit.StyErr
		}
		line2 = sty.Render(uikit.Truncate(k.ToastText, k.W))
	}
	return line1 + "\n" + line2
}

// Hits returns the clickable regions of the key-hint line for k, in display
// cells relative to the line's first column (column 0). It is empty while a live
// toast covers the hint line (then it is not a key strip). The caller offsets by
// the strip's screen row/column before matching a mouse position.
func (m *Model) Hits(k Key) []keystrip.Hit {
	if k.ToastText != "" && k.NowSec*1000 < k.ToastUntil {
		return nil
	}
	_, hits := keystrip.Layout(footerItems(k), k.W)
	return hits
}

// footerItems is the keybind hint line as an ordered list of clickable tokens. It
// lists only the keys live for the current focus, so the bindings it shows always
// work: cancel (x) appears only with the open-orders pane focused — the pane its
// selection drives — while cancel-all (X) needs no selection and so is advertised
// from every pane. Public mode has no orders table, so it shows the symbol-nav
// hint without the focus concept. Mode tags and notes are non-clickable prose.
func footerItems(k Key) []keystrip.Item {
	// While an overlay owns the keyboard, the main-screen keys are dead, so the
	// hint line steps back to a non-clickable reminder of the two keys that work in
	// every overlay: esc backs out of it, and ctrl+c quits (handleKey applies
	// ctrl+c before any mode dispatch; esc closes/aborts in every overlay handler —
	// both pinned by tests). Prose, not clickable caps: a click on the footer row
	// while a chart/help/notices overlay is open is the "click outside to close it"
	// gesture, and a clickable quit there would be an accidental-exit footgun. The
	// all-dim line (the cyan caps gone) is itself the "keys disabled" signal.
	if k.OverlayOpen {
		return []keystrip.Item{keystrip.Prose(i18n.T("esc to close overlay, ctrl+c to quit"))}
	}
	if k.CmdBarOpen {
		// The bar's echo line explains itself; keep the footer to prose (a
		// footer click would type nothing useful into a command line).
		return []keystrip.Item{keystrip.Prose(i18n.T("command bar — enter:review/place, ↑/↓:history, esc:close, ctrl+c:quit"))}
	}
	up := keystrip.Btn("↑", "up")
	down := keystrip.Btn("↓", "down")
	left := keystrip.Btn("←", "left")
	right := keystrip.Btn("→", "right")
	// Order mode: the panel owns the keys; the hints follow its view.
	switch k.OrderView {
	case 1: // the draft form
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[order]")),
			keystrip.Multi(i18n.T("field"), up, down),
			keystrip.Multi(i18n.T("adjust"), left, right),
			keystrip.Multi(i18n.T("book price"), keystrip.Btn("j", "j"), keystrip.Btn("k", "k")),
			keystrip.Multi(i18n.T("side"), keystrip.Btn("b", "b"), keystrip.Btn("s", "s")),
			keystrip.Prose(i18n.T("a/m/l:anchor")),
			keystrip.One("enter", i18n.T("review")),
			ordersTabCap(k.OrdersClosed),
			keystrip.One("esc", i18n.T("close")),
		}
	case 2: // the armed review
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[confirm]")),
			keystrip.One("enter", i18n.T("PLACE")),
			keystrip.Multi(i18n.T("±tick"), keystrip.Btn("[", "["), keystrip.Btn("]", "]")),
			keystrip.Multi("±10", keystrip.Btn("{", "{"), keystrip.Btn("}", "}")),
			keystrip.One("esc", i18n.T("back")),
		}
	case 3: // on the wire
		return []keystrip.Item{
			keystrip.Prose(i18n.T("placing…")),
			keystrip.One("esc", i18n.T("hide (still placing)")),
		}
	}
	// Ladder mode: the ladder owns the keys; the hints follow its view.
	switch k.LadderView {
	case 1: // browsing the ladder
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[ladder]")),
			keystrip.Multi(i18n.T("move"), keystrip.Btn("j", "j"), keystrip.Btn("k", "k")),
			keystrip.Prose(k.SizeKeys + ":" + i18n.T("size")),
			keystrip.One("t", "tif"),
			keystrip.Multi(i18n.T("adjust"), keystrip.Btn("←", "left"), keystrip.Btn("→", "right")),
			keystrip.Multi(i18n.T("limit at cursor"), keystrip.Btn("b", "b"), keystrip.Btn("s", "s")),
			keystrip.Multi(i18n.T("market order"), keystrip.Btn("B", "B"), keystrip.Btn("S", "S")),
			keystrip.One("x", i18n.T("cancel row")),
			keystrip.One("c", i18n.T("recenter")),
			ordersTabCap(k.OrdersClosed),
			keystrip.One("esc", i18n.T("exit")),
		}
	case 2: // an armed order awaits confirm
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[confirm]")),
			keystrip.One("enter", i18n.T("PLACE")),
			keystrip.Multi(i18n.T("±tick"), keystrip.Btn("[", "["), keystrip.Btn("]", "]")),
			keystrip.Multi("±10", keystrip.Btn("{", "{"), keystrip.Btn("}", "}")),
			keystrip.One("t", "tif"),
			keystrip.One("esc", i18n.T("back")),
		}
	case 3: // an armed cancel awaits confirm
		return []keystrip.Item{
			keystrip.Prose(i18n.T("[confirm]")),
			keystrip.One("enter", i18n.T("CANCEL order")),
			keystrip.One("esc", i18n.T("back")),
		}
	case 4: // on the wire
		return []keystrip.Item{
			keystrip.Prose(i18n.T("working…")),
			keystrip.One("esc", i18n.T("hide (still working)")),
		}
	}
	if k.Searching {
		// Whole sentences per pane (not composed from a noun), so a translation
		// is a natural phrase rather than English grammar with words swapped.
		prose, act := i18n.T("filter markets: type to filter"), i18n.T("select")
		if k.SearchPane == FocusBalances {
			prose, act = i18n.T("filter balances: type to filter"), i18n.T("scroll to")
		}
		return []keystrip.Item{
			keystrip.Prose(prose),
			keystrip.Multi(i18n.T("move"), up, down),
			keystrip.One("enter", act),
			keystrip.One("esc", i18n.T("cancel")),
		}
	}
	// "g:chart" appears whenever the candle chart is wired (any mode).
	chart := func(items []keystrip.Item) []keystrip.Item {
		if k.CandlesWired {
			items = append(items, keystrip.One("g", i18n.T("chart")))
		}
		return items
	}
	grp := keystrip.Multi(i18n.T("grp"), keystrip.Btn("-", "-"), keystrip.Btn("+", "+"))
	if !k.HasTrader {
		items := []keystrip.Item{
			keystrip.Multi(i18n.T("symbol"), up, down),
			keystrip.One("/", i18n.T("search")),
			grp,
		}
		items = chart(items)
		return append(items,
			keystrip.One("n", i18n.T("notices")),
			keystrip.One("?", i18n.T("help")),
			keystrip.One("q", i18n.T("quit")),
			keystrip.Prose(i18n.T("(public mode — order entry disabled)")),
		)
	}
	// The order-entry caps (buy/sell/ladder/the command bar) show whenever a
	// Trader is wired (public mode returns earlier). "f:funds" appears whenever
	// the funding screen is wired. The tail (notices/help/quit) is shared by
	// every focus.
	tail := func(items []keystrip.Item) []keystrip.Item {
		items = append(items, keystrip.One("b", i18n.T("buy")))
		items = append(items, keystrip.One("s", i18n.T("sell")))
		items = append(items, keystrip.One("t", i18n.T("ladder")))
		items = append(items, keystrip.One(":", i18n.T("cmd")))
		items = append(items, grp)
		if k.FundingWired {
			items = append(items, keystrip.One("f", i18n.T("funds")))
		}
		if k.MultiAccount {
			items = append(items, keystrip.One("@", i18n.T("account")))
		}
		items = chart(items)
		return append(items,
			keystrip.One("n", i18n.T("notices")),
			keystrip.One("?", i18n.T("help")),
			keystrip.One("q", i18n.T("quit")),
		)
	}
	switch k.Focus {
	case FocusOrders:
		scope := i18n.T("all-pairs")
		if k.OrdersAllPairs {
			scope = i18n.T("active-pair")
		}
		items := []keystrip.Item{
			keystrip.Prose(i18n.T("[orders]")),
			keystrip.One("tab", i18n.T("focus")),
			keystrip.Multi(i18n.T("select"), up, down),
		}
		if k.OrdersClosed {
			// The closed tab is read-only: no cancel target, 'o' goes back.
			items = append(items, ordersTabCap(true))
		} else {
			items = append(items,
				keystrip.One("x", i18n.T("cancel")),
				ordersTabCap(false))
		}
		return tail(append(items,
			keystrip.One("X", i18n.T("cancel-all")),
			keystrip.One("a", scope),
		))
	case FocusBalances:
		return tail([]keystrip.Item{
			keystrip.Prose(i18n.T("[balances]")),
			keystrip.One("tab", i18n.T("focus")),
			keystrip.Multi(i18n.T("scroll"), up, down),
			keystrip.One("/", i18n.T("search")),
			keystrip.One("X", i18n.T("cancel-all")),
		})
	default:
		return tail([]keystrip.Item{
			keystrip.Prose(i18n.T("[markets]")),
			keystrip.One("tab", i18n.T("focus")),
			keystrip.Multi(i18n.T("market"), up, down),
			keystrip.One("/", i18n.T("search")),
			keystrip.One("X", i18n.T("cancel-all")),
		})
	}
}

// ordersTabCap is the 'o' hint, labeled with the tab pressing it switches TO
// (the same convention as the 'a' scope cap).
func ordersTabCap(closed bool) keystrip.Item {
	if closed {
		return keystrip.One("o", i18n.T("open-tab"))
	}
	return keystrip.One("o", i18n.T("closed-tab"))
}

// degraded reports whether either endpoint is up and has the given degradation
// condition set — the gate for showing its status tag.
func degraded(pub, prv state.EndpointHealth, cond func(state.EndpointHealth) bool) bool {
	return (pub.Up && cond(pub)) || (prv.Up && cond(prv))
}

// taggedStatusCode reports whether a notice code is already represented by a
// self-clearing status tag on line one, so the sticky latest-notice slot should
// not also show it.
func taggedStatusCode(c stream.NoticeCode) bool {
	switch c {
	case stream.DataDelayed, stream.ConnectionUnreliable,
		// The recovery edges carry warn (matching their onset), and a recovery
		// CONNECTED is warn too, so without skipping them they would surface in the
		// latest-notice slot. The self-clearing indicators above already represent
		// these states — the delayed/unreliable tags for DATA_CURRENT/
		// CONNECTION_STABLE, the connection dot for CONNECTED — so the slot stays
		// reserved for non-self-clearing events (gaps, failures, server errors).
		stream.DataCurrent, stream.ConnectionStable, stream.Connected:
		return true
	}
	return false
}

// connDot renders one endpoint's connection state: dim ○ before the first
// connect, green ● up, red ● down.
func connDot(name string, eh state.EndpointHealth) string {
	switch {
	case !eh.Known:
		return uikit.StyDim.Render(name + " ○")
	case eh.Up:
		return uikit.StyOK.Render(name + " ●")
	default:
		return uikit.StyErr.Render(name + " ●")
	}
}
