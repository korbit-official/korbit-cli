// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/shopspring/decimal"

	"github.com/korbit-official/korbit-cli/internal/i18n"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
	"github.com/korbit-official/korbit-cli/internal/tui/uikit"
)

// The command bar (':'): type an order in a terse grammar, watch the echo
// line resolve it against the live market as you type, enter to review, enter
// to place. It reuses the order-entry engine (orderentry.go) end to end — the
// parse produces an orderDraft, the echo is buildPreview, and placement goes
// through the same parent dispatch, gates, and Trader path as the panel.
//
//	side  size          @ price          [tif]
//	b|buy 0.05          @ 163480000      gtc     limit, explicit price (snapped)
//	s     25%           @ a              ioc     sell 25% of balance at best ask
//	b     0.1           @ b-2                    buy at best bid minus 2 ticks
//	b     500k krw      @ mkt                    market buy, spend 500,000 KRW
//	b     10000 krw     @ 50m                    limit buy 10,000 KRW worth (qty derived)
//	s     0.05          @ m+5            po      sell at mid plus 5 ticks
//
// Anchors: b/bid, a/ask, m/mid, l/last, ±N in tick-size grid steps. k/m are a
// thousands/millions multiplier on ANY number (size or price) and say nothing
// about currency. Size: a base quantity (a bare number, or with the base-ccy
// unit), a quote-currency amount (the quote-ccy unit, e.g. `500k krw`), or N%
// of the funding balance. An amount sizes a market buy natively and any limit
// order by deriving the quantity at the resolved price. The symbol is the
// active market.
//
// Arming FREEZES the resolved price — the number on the review is the number
// that goes on the wire even if the book moves during the confirm keystroke.
// The one exception is drift protection on anchored prices: if the anchor
// re-resolves more than cmdDriftTicks grid steps away from the frozen price,
// placing refuses and disarms — the market the intent was relative to is
// gone, so the order must be reviewed again. An explicitly typed price is
// never second-guessed.

// cmdParse is the parsed (not yet market-resolved) command.
type cmdParse struct {
	side   string // buy | sell
	market bool   // @ mkt

	// Size: exactly one of qty / amt / pct is set.
	qty string // base quantity
	amt string // quote-currency amount (market buy: native; limit: derived to qty)
	pct int    // % of the funding balance (0 = unset)

	// Price: explicit XOR anchored (both empty for @ mkt).
	price       string // explicit price
	anchor      string // "bid" | "ask" | "mid" | "last"
	anchorTicks int    // ±N grid steps from the anchor

	tif string // "" = unspecified in the command (the draft's type-appropriate default: gtc for limit, ioc for market)
}

// parseOrderCmd parses the bar text against symbol (needed to tell the size's
// currency unit apart: the quote ccy means an amount, the base ccy or no unit
// means a quantity). It is otherwise pure — market resolution (anchors,
// percentages, deriving a limit qty from an amount) happens in resolveOrderCmd
// against live data.
func parseOrderCmd(text, symbol string) (cmdParse, error) {
	var p cmdParse
	base, quote := splitSymbol(symbol)
	base, quote = strings.ToLower(base), strings.ToLower(quote)
	fields := strings.Fields(strings.ToLower(text))
	// Re-join around '@' so "b 0.05 @a-1", "b 0.05 @ a-1", and "b 0.05@a-1"
	// all tokenize the same way.
	var toks []string
	for _, f := range fields {
		for {
			i := strings.IndexByte(f, '@')
			if i < 0 {
				if f != "" {
					toks = append(toks, f)
				}
				break
			}
			if i > 0 {
				toks = append(toks, f[:i])
			}
			toks = append(toks, "@")
			f = f[i+1:]
		}
	}
	if len(toks) == 0 {
		return p, fmt.Errorf("empty command")
	}

	switch toks[0] {
	case "b", "buy":
		p.side = "buy"
	case "s", "sell":
		p.side = "sell"
	default:
		return p, fmt.Errorf("start with b/buy or s/sell")
	}
	toks = toks[1:]
	if len(toks) == 0 {
		return p, fmt.Errorf("missing size — e.g. 0.05, 0.05 %s, 500k %s, or 25%%", base, quote)
	}

	// Size token: a number with an optional k/m multiplier and an optional unit
	// that picks the currency. The quote ccy means a quote-currency amount; the
	// base ccy or no unit means a base quantity; a trailing '%' is a percent.
	size := toks[0]
	toks = toks[1:]
	switch {
	case strings.HasSuffix(size, "%"):
		n, err := strconv.Atoi(strings.TrimSuffix(size, "%"))
		if err != nil || n < 1 || n > 100 {
			return p, fmt.Errorf("percent size must be 1-100, got %q", size)
		}
		p.pct = n
	default:
		num, ok := parseNumMult(size)
		if !ok || !num.IsPositive() {
			return p, fmt.Errorf("bad size %q — a quantity (0.05 %s), a %s amount (500k %s), or N%%", size, base, quote, quote)
		}
		// An optional unit token follows the number (never '@', which starts the
		// price clause). It disambiguates quote-amount from base-quantity.
		unit := ""
		if len(toks) > 0 && toks[0] != "@" {
			unit, toks = toks[0], toks[1:]
		}
		switch {
		case unit == "" || unit == base:
			p.qty = roundQty(num, roundFloor).String()
		case unit == quote:
			// Reject an amount that rounds away to zero so no size path carries a
			// zero amount.
			amt := roundAmt(num, roundFloor)
			if !amt.IsPositive() {
				return p, fmt.Errorf("%s %s is too small (rounds to zero)", size, quote)
			}
			p.amt = amt.String()
		default:
			return p, fmt.Errorf("unexpected %q after the size — use %s for an amount, %s for a quantity, or `@ price`", unit, quote, base)
		}
	}

	// Price clause: @ <price|anchor±N|mkt>.
	if len(toks) == 0 || toks[0] != "@" {
		return p, fmt.Errorf("missing @ price (a number, b/a/m/l ±ticks, or mkt)")
	}
	toks = toks[1:]
	if len(toks) == 0 {
		return p, fmt.Errorf("missing the price after @")
	}
	pt := toks[0]
	toks = toks[1:]
	switch {
	case pt == "mkt" || pt == "market":
		p.market = true
	default:
		anchor, ticks, ok := parseAnchorExpr(pt)
		if ok {
			p.anchor, p.anchorTicks = anchor, ticks
			break
		}
		if d, okd := parseNumMult(pt); okd && d.IsPositive() {
			p.price = d.String()
			break
		}
		return p, fmt.Errorf("bad price %q (a number, b/a/m/l with optional ±N ticks, or mkt)", pt)
	}

	// Optional tif.
	if len(toks) > 0 {
		switch toks[0] {
		case "gtc", "ioc", "fok", "po":
			p.tif = toks[0]
			toks = toks[1:]
		}
	}
	if len(toks) > 0 {
		return p, fmt.Errorf("unexpected %q after the order", toks[0])
	}

	// Cross-shape checks the sizing matrix would reject later — say it now. A
	// limit order accepts an amount (its qty is derived at the resolved price);
	// only a market order constrains the shape.
	if p.market {
		if p.side == "buy" && p.qty != "" {
			return p, fmt.Errorf("a market buy is sized in the quote currency — use an amount (500k %s) or a percent, not a quantity", quote)
		}
		if p.side == "sell" && p.amt != "" {
			return p, fmt.Errorf("a market sell is sized in base quantity, not a %s amount (no price to convert)", quote)
		}
		if p.tif != "" && p.tif != "ioc" {
			return p, fmt.Errorf("a market order takes only ioc")
		}
	}
	return p, nil
}

// parseNumMult parses a bar number with an optional attached k/m thousands/
// millions multiplier ("500k" → 500000, "1.5m" → 1500000, "0.05" → 0.05). The
// multiplier is a pure magnitude shorthand carrying no currency meaning (a unit
// token does that), so it applies to any number — a size or a price alike.
// ok=false when the mantissa is not a decimal.
func parseNumMult(s string) (decimal.Decimal, bool) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1_000, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, s[:len(s)-1]
	}
	d, ok := parseDec(s)
	if !ok {
		return decimal.Decimal{}, false
	}
	if mult != 1 {
		d = d.Mul(decimal.NewFromInt(mult))
	}
	return d, true
}

// parseAnchorExpr parses "b", "ask", "m+5", "l-2" style anchor expressions.
func parseAnchorExpr(s string) (anchor string, ticks int, ok bool) {
	names := map[string]string{"b": "bid", "bid": "bid", "a": "ask", "ask": "ask", "m": "mid", "mid": "mid", "l": "last", "last": "last"}
	body, off := s, ""
	if i := strings.IndexAny(s, "+-"); i > 0 {
		body, off = s[:i], s[i:]
	}
	name, found := names[body]
	if !found {
		return "", 0, false
	}
	if off != "" {
		n, err := strconv.Atoi(off)
		if err != nil {
			return "", 0, false
		}
		ticks = n
	}
	return name, ticks, true
}

// resolveOrderCmd materializes a parse against the live market into a
// placeable draft: anchors resolve to a price and step along the tick grid,
// an explicit price snaps onto it (disclosed via note), and a percent size
// becomes a quantity/amount from the funding balance.
func resolveOrderCmd(p cmdParse, symbol string, book state.Orderbook, hasBook bool, t state.Ticker, hasTicker bool, bals []state.Balance, balancesReady bool, bands []ops.TickBand, fees *FeeRates) (orderDraft, string, error) {
	d := newOrderDraft(symbol, p.side)
	if p.market {
		d.typ = "market"
	}
	if p.tif != "" {
		for i, o := range tifOptions {
			if o == p.tif {
				d.tifIdx = i
			}
		}
	}
	note := ""

	switch {
	case p.price != "":
		// An explicitly typed price is checked, never modified: an off-grid
		// price is rejected with its tick (a programmatic anchor snaps instead).
		d.price = p.price
		if onGrid, ok := ops.OnTick(bands, p.price); ok && !onGrid {
			tick, _ := ops.TickSizeAt(bands, p.price)
			return d, "", fmt.Errorf("price %s is off the tick grid (tick %s)", p.price, tick)
		}
	case p.anchor != "":
		base, ok := anchorPrice(p.anchor, p.side, book, hasBook, t, hasTicker, bands)
		if !ok {
			return d, "", fmt.Errorf("no live %s to anchor to yet", p.anchor)
		}
		price := base
		if p.anchorTicks != 0 {
			stepped, ok := ops.StepTicks(bands, base, p.anchorTicks)
			if !ok {
				return d, "", fmt.Errorf("tick size unknown for %s — cannot offset the %s by ticks yet", symbol, p.anchor)
			}
			price = stepped
		}
		d.price = price
		off := ""
		if p.anchorTicks != 0 {
			off = fmt.Sprintf("%+dt", p.anchorTicks)
		}
		note = p.anchor + off + " → " + price
	}

	switch {
	case p.pct > 0:
		sized, reason := applyPreset(d, bals, balancesReady, fees, p.pct)
		if reason != "" {
			return d, "", fmt.Errorf("cannot size %d%% — %s", p.pct, reason)
		}
		d = sized
	case p.amt != "" && d.typ == "market":
		d.amt = p.amt // market buy: the API sizes it in quote natively
	case p.amt != "":
		// A limit order accepts an amount by deriving the quantity at the
		// resolved price (the API has no limit-amount field).
		_, quote := splitSymbol(symbol)
		qty, ok := qtyFromAmt(p.amt, d.price)
		if !ok {
			return d, "", fmt.Errorf("need a price to size a %s amount as quantity", strings.ToUpper(quote))
		}
		if qty == "" {
			return d, "", fmt.Errorf("%s %s is too small to buy anything at %s", p.amt, strings.ToUpper(quote), d.price)
		}
		d.qty = qty
		deriv := p.amt + " " + strings.ToUpper(quote) + " ÷ " + d.price + " ≈ " + qty
		if note == "" {
			note = deriv
		} else {
			note += "; " + deriv
		}
	default:
		d.qty = p.qty
	}
	return d, note, nil
}

// cmdBarModel is the command bar's state. Like quick search, it lives inside
// the normal view and owns the keyboard while active; unlike the panel it has
// no field cursor — the text IS the order.
type cmdBarModel struct {
	active bool
	input  textinput.Model

	history []string
	histIdx int // -1 = editing a fresh line

	// The armed (frozen) order. armedNote carries the resolution note;
	// armedAnchor/armedTicks re-run drift protection at place time.
	armed       bool
	armedDraft  orderDraft
	armedNote   string
	armedAnchor string
	armedTicks  int
	errText     string // parse/resolve/gate error shown on the echo line
}

// cmdDriftTicks is the anchored-price drift tolerance: if the anchor
// re-resolves further than this many grid steps from the frozen price,
// placing refuses and disarms.
const cmdDriftTicks = 2

func newCmdBar() cmdBarModel {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "b 0.05 @ b-2   ·   s 25% @ a   ·   b 500k krw @ mkt"
	ti.CharLimit = 80
	ti.SetWidth(60)
	return cmdBarModel{input: ti, histIdx: -1}
}

// open activates the bar (fresh line, previous history kept).
func (c cmdBarModel) open() cmdBarModel {
	c.active = true
	c.armed = false
	c.errText = ""
	c.histIdx = -1
	c.input.SetValue("")
	c.input.Focus()
	return c
}

func (c cmdBarModel) close() cmdBarModel {
	c.active = false
	c.armed = false
	c.errText = ""
	c.input.Blur()
	return c
}

// recall moves through the history (delta ±1), re-parsing live.
func (c cmdBarModel) recall(delta int) cmdBarModel {
	if len(c.history) == 0 {
		return c
	}
	switch {
	case c.histIdx < 0 && delta < 0:
		c.histIdx = len(c.history) - 1
	case c.histIdx < 0:
		return c
	default:
		c.histIdx += delta
	}
	if c.histIdx >= len(c.history) {
		c.histIdx = -1
		c.input.SetValue("")
		return c
	}
	c.histIdx = clamp(c.histIdx, 0, len(c.history)-1)
	c.input.SetValue(c.history[c.histIdx])
	c.input.SetCursor(len(c.history[c.histIdx]))
	return c
}

// handleCmdBarKey owns the keyboard while the bar is active.
func (m model) handleCmdBarKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.cmdbar
	switch msg.String() {
	case "esc":
		if c.armed {
			c.armed = false
			c.errText = ""
			m.cmdbar = c
			return m, nil
		}
		m.cmdbar = c.close()
		return m, nil
	case "up":
		m.cmdbar = c.recall(-1)
		return m, nil
	case "down":
		m.cmdbar = c.recall(1)
		return m, nil
	case "enter":
		if c.armed {
			return m.placeFromCmdBar()
		}
		return m.armCmdBar()
	}
	if c.armed {
		return m, nil // the armed review takes enter/esc only
	}
	// Backspace on an already-empty line closes the bar, the same "delete past
	// the start to back out" gesture as quick search.
	if msg.String() == "backspace" && c.input.Value() == "" {
		m.cmdbar = c.close()
		return m, nil
	}
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(msg)
	c.errText = ""
	c.histIdx = -1
	m.cmdbar = c
	return m, cmd
}

// cmdBarMarket bundles the live inputs command resolution reads.
func (m model) cmdBarMarket() (state.Orderbook, bool, state.Ticker, bool) {
	sym := m.symbol()
	book, hasBook := m.store.Orderbook(sym)
	t, hasTicker := m.store.Ticker(sym)
	return book, hasBook, t, hasTicker
}

// cmdBarResolve parses + resolves the bar's current text against the live
// market (the live-echo path and the arm path share it).
func (m model) cmdBarResolve() (orderDraft, string, error) {
	p, err := parseOrderCmd(m.cmdbar.input.Value(), m.symbol())
	if err != nil {
		return orderDraft{}, "", err
	}
	book, hasBook, t, hasTicker := m.cmdBarMarket()
	return resolveOrderCmd(p, m.symbol(), book, hasBook, t, hasTicker,
		m.store.BalancesFor(m.accountSeq()), m.store.BalancesReady(m.accountSeq()), m.order.bands[m.symbol()], m.order.symFeesFor(m.symbol()))
}

// armCmdBar resolves and freezes the order for review.
func (m model) armCmdBar() (tea.Model, tea.Cmd) {
	c := m.cmdbar
	if gate := m.orderGate(); gate != "" {
		c.errText = gate
		m.cmdbar = c
		return m, nil
	}
	d, note, err := m.cmdBarResolve()
	if err != nil {
		c.errText = err.Error()
		m.cmdbar = c
		return m, nil
	}
	if err := d.validate(); err != nil {
		c.errText = err.Error()
		m.cmdbar = c
		return m, nil
	}
	p, _ := parseOrderCmd(c.input.Value(), m.symbol()) // err impossible: resolve parsed it
	c.armed = true
	c.armedDraft = d
	c.armedNote = note
	c.armedAnchor = p.anchor
	c.armedTicks = p.anchorTicks
	c.errText = ""
	m.cmdbar = c
	return m, nil
}

// placeFromCmdBar re-checks the gate and the anchored-price drift, then
// dispatches the frozen order.
func (m model) placeFromCmdBar() (tea.Model, tea.Cmd) {
	c := m.cmdbar
	if gate := m.orderGate(); gate != "" {
		c.errText = gate
		m.cmdbar = c
		return m, nil
	}
	if c.armedAnchor != "" {
		book, hasBook, t, hasTicker := m.cmdBarMarket()
		bands := m.order.bands[m.symbol()]
		if now, ok := anchorPrice(c.armedAnchor, c.armedDraft.side, book, hasBook, t, hasTicker, bands); ok {
			cur := now
			if c.armedTicks != 0 {
				if stepped, ok := ops.StepTicks(bands, now, c.armedTicks); ok {
					cur = stepped
				}
			}
			if ticksApart(bands, c.armedDraft.price, cur) > cmdDriftTicks {
				c.armed = false
				c.errText = fmt.Sprintf("market moved (%s now resolves to %s) — review again", c.armedAnchor, cur)
				m.cmdbar = c
				return m, nil
			}
		}
	}
	// Record the line, clear for the next order, and dispatch.
	line := strings.TrimSpace(c.input.Value())
	if line != "" && (len(c.history) == 0 || c.history[len(c.history)-1] != line) {
		c.history = append(c.history, line)
	}
	form := c.armedDraft.form()
	c.armed = false
	c.errText = ""
	c.histIdx = -1
	c.input.SetValue("")
	m.cmdbar = c
	return m.dispatchPlaceForm(form, placeFromBar)
}

// ticksApart measures the distance between two prices in grid steps (a large
// number when the grid is unknown but the prices differ — treat as drifted
// only when they differ materially: without a grid, any change counts as one).
func ticksApart(bands []ops.TickBand, a, b string) int {
	if a == b {
		return 0
	}
	ad, ok1 := parseDec(a)
	bd, ok2 := parseDec(b)
	if !ok1 || !ok2 {
		return 0
	}
	diff := ad.Sub(bd).Abs()
	tick, ok := ops.TickSizeAt(bands, a)
	if !ok {
		return 1 // no grid: any move is one nominal step (within tolerance)
	}
	td, ok := parseDec(tick)
	if !ok || !td.IsPositive() {
		return 1
	}
	return int(diff.Div(td).Ceil().IntPart())
}

// symFeesFor is symFees for an explicit symbol (the bar resolves against the
// active market, which may differ from the panel's last-opened draft). The
// lookup is for the ACTIVE sub-account — the cache holds other accounts'
// entries too, but every consumer estimates for the account orders are
// placed under.
func (o orderModel) symFeesFor(symbol string) *FeeRates {
	if f, ok := o.fees[feesKey{accountSeq: o.accountSeq, symbol: symbol}]; ok {
		return &f
	}
	return nil
}

// renderCmdBarLines renders the bar's two lines (input + echo), each fitted
// to w. The echo is the fully resolved order — side, size, snapped price,
// notional, fee, warning count, the book's gate state — before any key
// commits anything.
func (m model) renderCmdBarLines(w int) (string, string) {
	c := m.cmdbar
	bar := uikit.StyTitle.Render(":") + c.input.View()
	var echo string
	switch {
	case c.errText != "":
		echo = uikit.StyErr.Render(uikit.Truncate("⚠ "+c.errText, w))
	case c.armed:
		echo = m.cmdEcho(c.armedDraft, c.armedNote, true, w)
	case strings.TrimSpace(c.input.Value()) == "":
		echo = uikit.StyDim.Render(uikit.Truncate("b|s <qty|amt krw|N%> @ <price|b/a/m/l±N|mkt> [gtc|ioc|fok|po] — enter:review · ↑:history · esc:close", w))
	default:
		d, note, err := m.cmdBarResolve()
		if err != nil {
			echo = uikit.StyDim.Render(uikit.Truncate("… "+err.Error(), w))
		} else {
			echo = m.cmdEcho(d, note, false, w)
		}
	}
	return uikit.Truncate(bar, w), echo
}

// cmdEcho renders one resolved order as the echo line, fitted to w by
// dropping whole low-rank facts (uikit.FactsLine) instead of clipping the
// tail — the tail is where the warnings and the book state sit, and a narrow
// bar must never lose those. What the order IS (side/size/type/price), its
// warnings, and the book state always survive; the derived figures go first
// (fee, the anchor note, tif/pp — then the notional), each drop disclosed by
// the +n tail. The key hints re-attach only when they fit whole (the footer
// carries them regardless).
func (m model) cmdEcho(d orderDraft, note string, armed bool, w int) string {
	book, hasBook, _, _ := m.cmdBarMarket()
	p := buildPreview(d, book, hasBook, m.store.BalancesFor(m.accountSeq()), m.order.bands[m.symbol()], m.order.boundsFor(m.symbol()), m.order.symFeesFor(m.symbol()))

	// The size and price are wire values, not estimates. In the armed review —
	// the final confirmation of what is sent — render them exact so the figures
	// on screen equal the bytes on the wire (the order panel's confirm rule); the
	// live echo groups for reading. The derived figures below (notional, fee) are
	// advisory estimates and stay grouped either way.
	grp := uikit.GroupThousands
	if armed {
		grp = uikit.GroupExact
	}

	size := d.sizeValue()
	unit := strings.ToUpper(p.BaseCcy)
	if d.usesAmt() {
		size, unit = grp(size), strings.ToUpper(p.QuoteCcy)
	}
	facts := []uikit.Fact{{Text: strings.ToUpper(d.side) + " " + size + " " + unit}}
	if d.typ != "limit" {
		// "@ price" already says limit — same reading as the ladder strip.
		facts = append(facts, uikit.Fact{Text: d.typ})
	}
	if d.usesPrice() && d.price != "" {
		facts = append(facts, uikit.Fact{Text: "@ " + grp(d.price)})
	}
	facts = append(facts, uikit.Fact{Text: d.tif(), Rank: 2})
	if d.typ == "market" && d.pp {
		facts = append(facts, uikit.Fact{Text: "pp", Rank: 2})
	}
	if p.Notional != "" {
		lead := "= "
		if d.notionalIsEstimate() { // a market sell's notional moves with the book
			lead = "~ "
		}
		facts = append(facts, uikit.Fact{Text: lead + uikit.GroupThousands(p.Notional) + " " + uikit.FmtCurrency(p.QuoteCcy), Rank: 1})
	}
	if p.FeeEst != "" {
		facts = append(facts, uikit.Fact{Text: "fee ~" + uikit.GroupThousands(p.FeeEst), Rank: 2})
	}
	if note != "" {
		facts = append(facts, uikit.Fact{Text: uikit.StyDim.Render("(" + note + ")"), Rank: 2})
	}
	if n := len(p.Warnings); n > 0 {
		// The command-bar grammar stays English, but the preplace warning label
		// localizes off its code (English renders the code itself).
		facts = append(facts, uikit.Fact{Text: uikit.StyWarn.Render(fmt.Sprintf("⚠ %s%s", i18n.PreplaceWarnLabel(string(p.Warnings[0].Code)), cmdMoreWarnings(n-1)))})
	}
	if gate := m.orderGate(); gate != "" {
		facts = append(facts, uikit.Fact{Text: uikit.StyWarn.Render("book ○ blocked")})
	} else {
		facts = append(facts, uikit.Fact{Text: uikit.StyOK.Render("book ●")})
	}

	prefix, hint := uikit.StyDim.Render("→ "), uikit.StyDim.Render("  enter:review")
	if armed {
		prefix, hint = uikit.StyWarn.Render("CONFIRM "), uikit.StyDim.Render("  enter:PLACE esc:edit")
	}
	line := prefix + uikit.FactsLine(maxInt(0, w-lipgloss.Width(prefix)), " · ", facts)
	if lipgloss.Width(line)+lipgloss.Width(hint) <= w {
		line += hint
	}
	return line
}

// cmdMoreWarnings renders the echo's " +N more" warning tail.
func cmdMoreWarnings(extra int) string {
	if extra <= 0 {
		return ""
	}
	return fmt.Sprintf(" +%d more", extra)
}
