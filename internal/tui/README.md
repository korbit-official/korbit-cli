# `internal/tui` — the interactive trading terminal

The full-screen terminal behind `korbit-cli tui`: live market data (ticker,
orderbook, trades) plus the account's open orders, fills, and balances, with
order entry. Built on Bubble Tea v2 (`charm.land/bubbletea/v2`), with the
textinput bubble and lipgloss for layout. The lists (markets, open orders,
balances) are **hand-rendered** with one shared windowing model rather than the
bubbles `table`, so a mouse click maps exactly to a row and scrolling is uniform.

## Layering (who owns what)

```
internal/stream         delivers events + reliability notices (reconnect, backfill)
internal/stream/state   materializes them: "what is true right now"
internal/tui            renders state, routes input, dispatches order actions
internal/cli/tuicmd  command wiring: key/URL resolution, session, Trader impl
```

## Panes are components

Each pane is a small component in its own package under `internal/tui/components/`
(`header`, `footer`, `sidebar`, `orderbook`, `trades`, `orders`, `fills`,
`balances`, `notices`, the trade ladder's `ladder`, plus the funding screen's
`curlist` and `transfers`). A pane is a **pure view component**: `View(Key, Data)
string`, a function of explicit inputs that owns no shared state. The model keeps
the interactive/coupled state (active symbol, focus, cursors, scroll, search) and
builds each component's `Key` and `Data` (the `viewSidebar`/`viewOrderbook`/… helpers
in `view.go`). The lists (markets, open orders, balances) are still hand-rendered
with one shared windowing model rather than the bubbles `table`, so a mouse click
maps exactly to a row.

Shared presentation primitives — styles, the palette, the panel/box helpers, the
decimal/clock formatters — live in `internal/tui/uikit`, the single source for
them (the `tui` package keeps none of its own).

**Render caching.** A component caches its last rendered string via
`uikit.Memo[Key]`, keyed on a comparable `Key` = the geometry, the palette
identity (`uikit.StyleID`, the comparable stand-in for the non-comparable
lipgloss palette), the pane's UI scalars, and the relevant store **revision(s)**.
`state.Store` carries a per-section revision counter (`TickerRev`/`BookRev`/
`TradeRev`/`OrderRev`/`FillRev`/`BalanceRev`/`NoticeRev`/`HealthRev`) bumped at its
only two mutation points (`Apply`, `MarkMarketStale`); a revision stands in for
"did this section's data change", so the `Key` alone decides a cache hit and the
`Data` is read only on a miss. A redraw that doesn't touch a pane reuses its
cached string. Components are pointer fields on the model so the value-receiver
`View` writes through one shared cache (single goroutine — no lock). The orders
pane keys on a model-owned `ordersRev` bumped in `refreshOrders` (its rows are a
model-built projection, with a parallel per-row canceling flag rendered as a
grayed-out row rather than a status column). The candle chart is
the exception: an **interactive** component that owns its own state
(zoom/scroll/selection) plus `Update`, rather than a pure-view one — but its
`View` is frame-cached too (keyed on a content revision + a rendered style probe +
the geometry/selection/toggle scalars), so it re-renders only when something
actually changes rather than every frame it is on screen.

Facts to preserve:

- **The TUI never signs or sends anything itself.** Order placement and
  cancellation go through the `Trader` interface, implemented in
  `cli/tuicmd` over the exact same machinery as `order place` / `order
  cancel`: spec-driven param validation, the cross-field sizing matrix, minted
  `clientOrderId`s, the action journal's hard guarantee (the pre-call order
  row must be written or the order is NOT sent), and the idempotency-gated
  retry layer (place is single-shot; cancel may auto-retry). One deliberate
  deviation: a *post-acceptance* journal-write failure cannot "print the
  result, then exit non-zero" inside a full-screen program, so it surfaces as
  a `Warning` on the result and the UI shows it as an error toast.
- **Order entry is available whenever a `Trader` is wired (private mode); the
  only gate is public mode.** The `b`/`s` order panel, the `:` command bar, and
  the `t` trade ladder open whenever `Config.Trader != nil` — there is no
  experimental opt-in. In public mode (`Trader == nil`) the keys only toast and
  the `b:buy`/`s:sell` footer caps / the order-panel help row are dropped.
  Cancel (`x`) and cancel-all (`X`) stay live regardless.
- **The %-of-balance size presets are config-driven and pinned.** The order
  panel's size chips (`%`, or a click) and the ladder's number keys (or its `←/→`/click
  chip stepper) arm a preset from `Config.OrderLevels` (default `[10,25,50,100]`,
  100 → "max"). The cli layer
  writes the built-in defaults to `config.json`'s `tui.orderLevels` on the first
  `tui` launch and reads them thereafter, so a program update never silently
  changes what a preset key does — muscle memory holds. All chip labels and the
  key/value hints derive from the levels (`sizeKeysLabel`/`sizeValuesLabel`), so
  a customized list never disagrees with what a key actually does. The levels
  count is dynamic (keys `1..N`, capped at 9); editing `config.json` is the only
  way to change them.
- **The state store is single-goroutine by design.** One pump goroutine
  forwards session events into the program (`Program.Send` is safe from other
  goroutines and a no-op after exit); `state.Store` is only touched inside
  `Update`. Don't introduce other writers. The pump also selects on a
  `progDone` channel so it can't block forever on an `Events` channel that
  never closes (independent of whether `StopSession` is wired).
- **At most one money action is in flight at a time — TUI-wide.** A single
  model-level `orderInFlight` gate blocks opening the order panel, arming a
  new order, or starting a cancel while a place or cancel is pending — so
  backing out of an in-flight order can't dispatch a second live one. The funding screen's requests share the
  rule: `moneyActionInFlight()` (`orderInFlight || funding.busy()`) is what the
  order keys check, and the funding dispatch checks `orderInFlight` back — an
  order action and a funding request can never be on the wire together. The
  `Trader` and `Funding` contracts depend on this; don't weaken it.
- **Reconciliation lives in `internal/stream/state`, not here.** Per-key
  newest-`ServerTime`-wins ordering, the terminal-status latch, openOrders
  snapshot reconciliation (`StatusClosedUnknown`), WS/REST field normalization
  (`unfilled`→`open`, `fee`/`feeQty`, `filledAt`/`tradedAt`), and tradeId
  dedupe are documented in that package. The TUI renders what the store says.
- **Reliability stays visible.** Connection dots (pub/prv), backfill and gap
  counters sit in the status line, the latest warn/error notice is shown
  inline, and `n` opens the full notices log. The stream layer's "up to date
  or TOLD" property must never be hidden by the UI.
- **No pane shows stale data — it shows "loading…" instead, and "no data yet"
  when that is the truth.** Every live pane is gated on a freshness latch in
  `state.Store` so a value is displayed only while it is current. The latches
  mirror `OpenOrdersReady`: a public one (`OrderbookReady`/`TradesReady`/
  `TickerReady`, cleared on a **public** DISCONNECTED) and a private one
  (`OpenOrdersReady`, `BalancesReady`, cleared on a **private** DISCONNECTED),
  each re-set when the reconnect re-snapshots (a REST snapshot landing while the
  feed is still down applies its data but does not re-set the latch) — so a
  drop-and-recover cycle flips the affected panes to loading and back, not just a
  mode switch. Orderbook/trades are additionally gated on `marketSettled()` (the
  active-symbol re-subscribe has landed) and reset for the incoming pair on a
  switch (`MarkMarketStale`), so a previously-viewed pair's retained book/trades
  never flash as current before the new snapshot.
  **A frame-arrival latch alone cannot say "the server has nothing":** a pair
  that has never traded is sent no ticker/trade snapshot at all (and an
  orderless book may get none either), so those latches would never flip and the
  pane would spin forever. The public market-data panes therefore read a
  three-way `state.DataStatus` — `NotReady` | `Empty` | `Present` — from
  `Store.{Ticker,Orderbook,Trade}Status`, which combines the frame latch with a
  **subscribe-ack latch** fed by the `stream.Subscribed` control event (public
  channels only; cleared on a public disconnect and on a market switch). An ack
  counts as "settled" only once it has stayed frameless for
  `state.SettleDelayMs` — the server acks first and sends the snapshot right
  after, so a bare ack does not yet mean empty. `Empty` renders the pane's own
  wording ("no resting orders", "no trades yet", "awaiting first trade") instead
  of a spinner, and every consumer of the classification — panes, row lists, and
  the place gate — reads it from the same three methods. The **data is kept**, only its
  display is gated (the trade ring's high-water mark still drives gap-patching).
  The sidebar is the exception to hiding: as the top-level selector it always
  lists every symbol (the list is known up front) and gates only the price —
  a ticker with no figure to show — stale, or a pair that has never traded —
  shows the symbol with a compact "…" marker in place of the change% (the row's
  `Ready` reads the same `TickerStatus` classification the panes do, so a
  never-traded pair does not sit on the marker forever). Fills are the other exception:
  append-only with no re-snapshot (snapshot-only backfill), so they gate on the
  private endpoint simply being up (`Health().Private.Up`).
- **The symbol set is broad; the heavy channels follow one active symbol.** The
  watched set is `--symbols` (or every launched pair when omitted; resolved in
  `cli/tuicmd`). `tuiSubscriptions` subscribes **ticker for every symbol**
  (it powers the sidebar, which lists them all and never shrinks) but
  **orderbook + trades for only the active symbol**. Switching the active symbol
  re-points those via the stream session's dynamic `Subscribe`/`Unsubscribe`
  (the `Config.SetActiveMarket` seam, wired to `tuiActiveMarket` in the cli) —
  subscribe-next-then-unsubscribe-prev. The **account channels cover every
  symbol** (live order/fill/balance updates are account-wide), and the session
  runs in `BackfillSnapshotOnly` mode so that breadth re-backfills open orders +
  balances on reconnect without fanning per-symbol order/fill *history* out over
  every pair. `myTrade` therefore has no historical backfill — live fills only.
- **The orderbook can be grouped.** `+`/`-` cycle the active symbol's
  orderbook grouping level between the raw book and the levels
  `tickSizePolicy` publishes as `orderbookLevels` (they ride the same
  per-symbol metadata fetch as the tick bands; the choice is sticky per
  symbol for the session, `model.bookGrp`). A change re-points the orderbook
  subscription alone through `Config.SetOrderbookLevel` —
  unsubscribe-then-subscribe, because an unsubscribe matches channel+symbol
  regardless of level — on the same settle debounce as a symbol switch, and
  hides only the book (`store.MarkBookStale`; the untouched trade feed stays
  current) until the regrouped snapshot lands. A symbol switch carries the
  incoming pair's level (`SetActiveMarket`'s `nextLevel`). The orderbook pane
  and the ladder disclose the active level as a `grp` title chip — the
  disclosure matters because every figure derived from the book (the order
  preview, the slippage sim) reads the displayed granularity; grouped bids
  bucket down and asks up, so a marketable sim over a grouped book
  overestimates cost, never underestimates it. On the ladder, resting orders
  render — and `x`-cancel — at their **bucket** row (`ladder.Bucket`, the one
  mapping both use). The keys work in normal mode, order mode (form view),
  and the ladder (browsing) — never while a review is armed, so the numbers
  under a confirm can't be regrouped mid-decision.
- **Open-order snapshots are lazy: scoped to what you're looking at.** The live
  `myOrder` channel stays subscribed to every pair (account-wide), but the
  authoritative `GET /v2/openOrders` snapshot — the part that is a per-symbol,
  per-account REST call — is taken only for the **tracked**
  `{account, symbol}` scope set, not all of them. The
  session runs `LazyOpenOrders`; the TUI drives the tracked set through the
  `Config.SetTrackedOrders` seam (wired to `Session.SetTrackedOrderScopes`,
  scopes built under the active account via `model.trackScopes`): the
  **active pair** in the default per-pair view, or **every pair** when the user
  toggles "all pairs" (`a`, on the orders pane). Switching the active pair
  re-points the tracked set (debounced, alongside `SetActiveMarket`), and the
  session snapshots newly-tracked scopes on demand and re-snapshots the tracked
  set on every reconnect — but a scope re-tracked **within the same private
  connection** is not re-fetched at all (the session remembers its baseline
  generation; the lossless account-wide feed kept it current), so flipping
  focus back and forth costs no REST calls. This is what keeps launching
  against hundreds of pairs from firing hundreds of sequential `/v2/openOrders`
  calls at startup; the all-pairs view backfills them concurrently (bounded). The
  open-orders panel filters its rows to the same scope and shows the scope + a
  "loading…" hint (derived from `Store.OpenOrdersReady`) until the displayed
  pair(s) have an authoritative snapshot.
- **The active symbol is the single switch point, and switching is immediate.**
  `model.setActive` is the one place `active` moves: the arrows (and a row
  click) change the selection **at once** — the detail panels switch
  immediately. The expensive part (re-pointing the orderbook/trade subscriptions
  via `SetActiveMarket`) is **debounced** (`resubscribeMsg`, generation-gated):
  only the latest selection that has settled for `resubscribeDelayMs` fires a
  `Subscribe`/`Unsubscribe`, so scrubbing the list with the arrow key doesn't
  churn the stream. `subscribed` tracks what is actually subscribed so the
  debounced call passes the correct `prev`.
- **The mouse wheel scrolls; it never selects.** `m.scroll` is the sidebar
  window top, moved only by the wheel (`scrollSidebar`) — wheeling pans the list
  without changing the selected market. A keyboard move instead calls
  `ensureActiveVisible` to keep the selection on screen. (Wheel over the
  orders pane moves the table cursor; wheel over the balances pane moves its
  window.)
- **The balances pane is focusable and scrolls, with no selection.** Like the
  sidebar wheel, `m.balScroll` is just the balances window top. The balances list
  is in the focus ring (`focusBalances`, private mode only); with it focused the
  arrows/paging/home/end scroll the window (`scrollBalancesKey`) — there is no
  selected balance row, so nothing is highlighted and nothing is committed. The
  column header stays pinned while the entries below it scroll (the `balances`
  component windows from the scroll offset). `balancesVisible`/`balScrollMax` derive the
  bounds from the panel height; the stored offset is clamped on read so a resize
  or a shrunk balance set can't strand the window past the end.
- **Each navigable pane shows a position indicator.** The sidebar, the
  orders panel, and the balances list render their position spliced into the
  bottom-right of their border (`uikit.PanelWithFooter`/`uikit.BottomBorderLabel`): the
  sidebar/orders show `selected/total` (e.g. `12/352`); balances, having no
  selection, show the visible row range (e.g. `3-6/20`). The live feeds
  (orderbook/trades/fills) have no scrollback, so they carry no indicator.
- **The trades panel is seeded with depth, per active symbol.** The active
  symbol's trade subscription requests `TradeHistory` recent trades
  (`tuiTradeHistory`, in `cli/tuicmd`) so the panel starts full from REST
  rather than waiting on the server-defined live snapshot (which can be a single
  trade). `tradeHistoryFor` reads that depth from the live subscription registry,
  so the seed fires on a symbol's **first** activation; a later switch-back
  reuses the retained per-symbol `tradeId` high-water mark and **gap-patches** the
  hole instead of re-seeding. Those rows arrive as `origin=backfill`; the state
  layer orders trades by `tradeId`, so seed/patch rows interleave correctly with
  live ones.
- **The candle chart has two views over one feed.** A full-screen overlay (`g`)
  is the interactive view — select (`←/→`, or click a candle), scroll
  (`pgup/pgdn`, wheel), zoom (`+/-`), interval (`[ ]`), jump-to-live (`g`),
  volume toggle (`v`), indicator cycle (`i`) — with scroll-back history backfill.
  The overlay is dismissed by `esc` **or a click outside its box**
  (`inChartOverlay` over `chartOverlayBounds`, the frame counting as inside), so a
  mis-click is reversible with the mouse alone; its hint line leads with the close
  key. The inline pane (`G`, **on by default**, hidden on a short terminal) sits
  over the orderbook/trades center region as a display-only glance: it always shows
  the live edge (no in-pane scroll or selection) and follows the active market on
  the same debounce as the orderbook re-subscribe. Its one mouse gesture is a
  **click to open the full overlay** (`inlineChartAt`). Both render
  the one candle feed (a `candles.Series` from `internal/candles`) for the active symbol+interval, so the inline pane inherits
  every overlay setting (zoom, interval, volume, indicator, color scheme) — it is a
  per-frame value-copy of the overlay's `candlechart.Model`, pinned to live. The
  copy reads the stored chart's candles and overlays **read-only** (no per-frame
  re-feed): the stored chart is held current with the feed by the Update path
  (`foldChartTrades`/`applyCandles` call `SetCandles` on every change), so the
  inline render skips the candle copy and any indicator recompute. The
  feed stays fresh (re-sync loop + live trade folding) whenever **either** view is
  active (`chartActive`). A pair with no candles yet says so rather than
  spinning: once a seed returns (`chartSeeded`), an empty result renders "no
  candles yet", and the pair's first trade pulls ONE authoritative re-fetch
  (`chartSeedRefetching`) — the bucket grid is the server's (KST-anchored) and
  only a fetched row can place the first candle on it; live trades fold in from
  there. The chart is the one float island: a price/qty becomes a
  `float64` only at the chart edge (`chartCandles` in `chart.go`, feeding `candlechart` — a chart is a visual artifact);
  every order/balance value elsewhere stays a decimal string.
- **Chart indicators are pluggable.** `candlechart.Indicator` is the wiring seam
  (candle series → overlay line(s)); the chart owns the wiring and drawing, the
  math lives outside it in `internal/tui/chartind` (over `internal/indicators` /
  go-talib). `candlechart.Model.SetIndicators` registers them and the chart
  recomputes their overlays from the **closed** candles (the live in-progress bar
  is excluded) whenever that closed series changes, caching the result so a
  live-price tick (the per-event re-feed of the stored chart) reuses the cached
  line instead of recomputing. The TUI sets them once.
  **EMA 20 is on by default** (`model.chartInd`, wired onto the chart in
  `newModel`); the `i` key cycles `chartIndicatorOptions` (off → EMA 20 → …). The
  full overlay and the inline pane name the active indicator via the shared
  `chartTitle`. Add an entry there and a `chartind` type to offer another
  indicator — no change to `candlechart` or the candle feed.
- **Decimal strings stay strings.** Display formatting (`uikit.GroupThousands`,
  which groups thousands and, on a large-magnitude value, caps the fraction to one
  digit — a currency-agnostic display rule; `uikit.GroupExact` where the rendered
  figure must equal the wire value, e.g. an order review) renders a copy; order
  values pass through verbatim. The one place a value becomes a float is the
  orderbook **depth bar** (`depthBarW`/`parseDepthQty`, in the `orderbook` component),
  where a level's qty sizes an inline background bar relative to the largest
  visible level — display-only, and the wire string is still rendered untouched.
  No order/balance value is ever parsed into a float.
- **Symbols/currencies render human-friendly, identity stays wire-form.** Every
  visible currency pair is rendered through `uikit.FmtSymbol` (`btc_krw` →
  `BTC/KRW`) and every currency through `uikit.FmtCurrency` (`btc` → `BTC`); the
  raw wire string is never mutated — it remains the identity for map keys, cache
  `Key`s, subscriptions, active-symbol matching, and the order path. Format only
  at the leaf render point, never the data model. The transform assumes nothing
  about length, so panes sizing a column must measure the formatted string. Quick
  search matches case- and separator-insensitively (`uikit.SymbolSearchKey`), so
  typing either `BTC/KRW` or `btc_krw` finds the pair.
- **Up/down coloring is color-scheme- and profile-aware (`uikit/colors.go`).** The semantic
  up/down colors (rising/falling, bid/ask, buy/sell) are NOT fixed styles — they
  resolve from a `uikit.ColorScheme` (green-red, the default, or red-blue as in the
  Korbit app; `C` toggles, and the scheme in effect at exit is written to
  `config.json`'s `tui` block via the `Config.SaveColorScheme` seam — once, on
  quit, so it survives the next launch without a per-keypress write) and
  the terminal's detected color profile (arrived via
  `tea.ColorProfileMsg`, held as `model.profile`). `uikit.PaletteFor` produces the
  `model.pal` up/down `uikit.SideStyle`s, which the chart reads and each pane
  component resolves from its `uikit.StyleID`. On truecolor and 256-color
  the foreground is an EXPLICIT color (not a basic ANSI name) so a remapped
  16-color palette can't repaint "green"/"red" arbitrarily.
- **Orderbook depth bars (`depthRow`/`depthBarW`, in the `orderbook` component).** Each level draws an inline
  background bar sized by its qty relative to the largest visible level on either
  side (≥1 cell for any non-zero level). The bar background is a very dark tint of
  the side color — near-black hex on truecolor, the darkest colored 256-cube cell
  on 256-color — so the foreground text stays legible over it. On a 16-color or
  colorless terminal the palette's `barBG` is nil and the bar is dropped (a dark
  256 tint would collapse to black, i.e. an invisible bar), leaving the plain row.
- **Deliberate stops are success.** `q`/Ctrl-C exit 0. `q` works only from the
  normal screen (it closes no overlay — `esc` does that) and is a *soft* quit:
  with a money action still on the wire it opens `modeQuitConfirm` (enter
  quits, esc stays; the dialog auto-dismisses if the action lands first —
  `closeQuitConfirmIfIdle`). Ctrl-C quits unconditionally from any mode. When
  the session dies
  fatally its events channel closes, the TUI quits, and the cli layer reports
  the session error after the screen is restored (same exit-code
  classification as monitor).
- **The layout is resizable, and it's the single source of geometry.** The
  four body columns (sidebar / orderbook / trades / account) and the account
  column's three stacked panels (orders / fills / balances) are sized from
  seam *fractions* on the model (`sideDiv`/`colDivA`/`colDivB`,
  `rowDivA`/`rowDivB`), not fixed widths. `colWidths`/`rightHeights` derive cells
  from those fractions with per-panel minimums (`minSideW`/`minColW`/`minRightW`/
  `minPanelH`) so a drag can never collapse a panel, and `hitDivider`/`applyDrag`
  (in `view.go`) translate a mouse position to a seam and move it. Public mode
  (`rightPaneVisible` false) drops the account column — three columns, notices in
  the `n` popup not a pane. Defaults
  (`resetLayout`): sidebar 16%, orderbook/trades 26% each, account the rest
  (~32%), the account column split 40/30/30. `=` resets; the geometry is
  session-only (not persisted). Any new size-dependent component must read these
  helpers, never hardcode a width.
- **Terminal size has a hard floor and a recommended size**, per mode via
  `sizeFloors` (in `view.go`): 100×28 / 120×32 private, 60×18 / 80×24 public
  (fewer columns, so comfortable on a classic 80×24). Below the hard floor
  `render()` replaces the whole screen with a resize notice (the model keeps
  running — resizing back restores everything). Between the two, the TUI runs
  normally with a `⚠ WxH — best ≥WxH` chip in the header (`crampedNote` →
  `header.Key.Cramped`): panes work, but long figures and fact lines may
  truncate. At and above the recommended size everything
  renders whole. [`SMALL-TERMINALS.md`](SMALL-TERMINALS.md) is the full
  constrained-space policy — the size floors, the truncation/compaction
  toolkit (`uikit.Truncate`/`FactsLine`/`Compact`), and where each rule
  applies. Read it before composing any line that could outgrow its pane.
- **Focus decides where navigation keys go.** A `focusPane` (market selector /
  orders table / balances list) gates up/down and paging; `tab`/`shift+tab`
  cycle the focus ring (`focusable`: the orders table and balances list are in the
  ring only in private mode, so public mode is single-pane and tab is a no-op).
  The focused pane's border is highlighted (`uikit.StyBorderFocused`) and the footer
  names it.
- **Quick search (`/`) is an inline, incremental FILTER on the list panes.**
  Pressing `/` with the market selector or the balances list focused starts a
  search: subsequent keys build a query (shown on the **bottom-left** of that
  pane's border via `uikit.BottomBorderLabel`, opposite the position indicator) and the
  pane shows **only the rows whose symbol/currency contains it**, recomputed live
  (`filteredSymbols`/`filteredBalances`). The arrows move within the filtered
  results — the markets candidate (`searchCursor`, reverse-highlighted) or the
  balances window (`searchScroll`). `enter` commits the pick and the **full list
  returns**: markets `setActive` the highlighted symbol, balances `scrollBalancesTo`
  the first match. `esc` just closes — as does `backspace` on an already-empty
  query (deleting past the start backs out of the search). Crucially the
  underlying active symbol and
  balances scroll are **not touched until enter**, so esc has nothing to restore
  and typing causes no subscription churn. It is *not* an overlay — it lives
  inside `modeNormal` (the `searching` flag), so `handleKey` routes to
  `handleSearchKey` while active and search owns the keyboard *and the mouse*
  (clicks/wheel are suppressed while searching) until enter/esc. A command letter
  typed into the search is query text, not a command. Match is a case-insensitive
  substring.
- **The orders table is self-rendered (not the bubbles `table`).** It has a
  selected row (`orderCursor`, the cancel target) and its own window
  (`orderScroll`), the same model as the sidebar/balances. The `orders`
  component's `tableLines` draws the dim column header plus the windowed rows with
  the selected row reverse-highlighted; the model's `refreshOrders` rebuilds the
  row projection (passed as the component's `Data`) and **preserves the selection
  by order id**, re-clamping the window. Hand-rendering replaces the bubbles
  table specifically so a click maps exactly to a row — the bubbles table's
  internal viewport offset isn't observable, so a click couldn't be translated
  to an absolute row reliably.
- **The orders pane has a read-only closed tab** (`o`, or the title's tab
  switcher):
  the terminal orders the session's store has observed
  (`Store.ClosedOrdersFor`), most recently closed first, with the status
  column the open tab deliberately omits — it is where a rejected (`expired`)
  or canceled order becomes visible — and a `filled/qty` column that makes a
  partial fill's returned remainder legible. The pane keeps ONE row projection
  (`ordersClosed` selects which set `refreshOrders` reads), so the
  cursor/scroll/wheel/click plumbing is tab-agnostic; the cursor resets on
  toggle, `x` refuses with a hint, and the `a` scope toggle governs both tabs.
  The tab selection is **sticky** — it survives focus moves and mode changes
  until switched back — and the switch works from anywhere it is safe: `o` in
  the normal view (any pane, no focus round-trip), in ladder browse, and in the
  docked order panel (form view only — never mid-confirm or in a text input), a
  click on the title's tab switcher (`orders: [open] │ closed`; both tabs carry the
  clickable key-cap style and the active one is bracketed and reverse-
  highlighted; per-segment hit-testing in `orders.TitleTab` measures the
  localized label widths past the `orders: ` prefix), and the footer's `o` cap. Every pane-focus move goes through the
  `setFocus` seam, so a future focus side effect has one home.
  The set is **session-scoped** (account backfill is snapshot-only, so closes
  that happened while disconnected show as `closed?` — real status unknown —
  and orders that opened and closed entirely within a gap are absent), and the
  view is capped (`closedOrdersCap`).
- **Mouse is capture-on (`MouseModeCellMotion`) for resize, focus, switching,
  selecting, and scrolling.** A left-press on a seam starts a resize drag; a
  click on a sidebar row switches the active symbol (`sidebarRowAt`); a click on
  an open-orders row focuses that pane and selects the order (`ordersRowAt`, so
  `x` then cancels it); a click on the inline candle strip opens the full chart
  overlay (`inlineChartAt`); a click on a focusable pane focuses it (`paneAt`,
  which reports `ok=false` for the header/footer, the orderbook/trades center, the
  fills panel, and gaps — those clicks are **no-ops** and never steal focus); the
  wheel scrolls the pane under it (`handleWheel` — sidebar window, orders cursor,
  or balances window). Detected in `modeNormal` and not
  while searching — overlays and quick search own the body. The help and notices
  overlays (`modeHelp`/`modeNotices`) are dismissed by a click outside their box
  (`inActiveOverlay` → `centeredOverlayBounds`, the frame counting as inside), the
  same reversible gesture as the chart overlay. Capturing the mouse
  takes over the terminal's native text selection while the TUI runs — an
  accepted trade.
- **The modal dialogs (confirm/cancel-all/quit-confirm) take body clicks too.** Beyond
  their hint caps (below), a cancel-all scope chip selects its scope
  (`setCancelAllScope`, shared with the `←/→/tab` keys); regions are hit-tested
  by `overlayBodyAt`. A click outside the
  box dismisses the dialog (`dismissModal` reuses each dialog's `esc`) — except a
  **running** cancel-all batch, which only its `esc`/stop hint aborts.
- **The key-hint strips are clickable buttons.** The footer hint line, the chart
  overlay hint, and the modal-overlay (confirm/cancel-all) hints are all
  built from one reusable component, `components/keystrip`: an ordered list of
  tokens (a key cap plus its dim label) rendered with the cap styled (underlined +
  cyan) so it reads as pressable, plus a relative hitmap. `stripKeyAt` (`view.go`)
  maps a left click to the cap under it and **dispatches the cap's synthetic
  `tea.KeyPressMsg` through `handleKey`** — so clicking a hint is identical to
  pressing it, single source of truth in the key handlers. A paired command
  (`[`/`]`, `←`/`→`, `+`/`-`, `↑`/`↓`) renders as two separately-clickable caps,
  each showing and pressing its own real key. The footer's clickable regions come
  from `footer.Hits` (computed from the same `footer.Key` as its render, so they
  can't drift from the line on screen); the centered overlays recompute their hint
  and body geometry in `overlayLineOrigin` from the same lines they render.
- **An overlay owns the keyboard; the footer steps back.** Key dispatch is
  per-mode: `handleKey` routes to the open overlay's handler
  (confirm/cancel-all/quit-confirm/help/notices/chart) and the main-screen keys never
  reach `handleNormalKey`. Two keys work in **every** overlay: `esc` backs out of
  it and `ctrl+c` quits (`ctrl+c` is `handleKey`'s unconditional first check;
  `esc` closes/aborts in every overlay handler — `TestCtrlCQuitsInEveryMode` and
  `TestEscClosesEveryOverlay` pin both). So while an overlay is open
  (`footer.Key.OverlayOpen`, set from any mode except normal, order mode, and
  ladder mode — the order panel and the trade ladder dock into the layout, so
  their footers keep live, clickable hints via `footer.Key.OrderView` /
  `footer.Key.LadderView`) the footer stops
  advertising the now-dead main keys and shows a single **non-clickable**
  reminder, `esc to close overlay, ctrl+c to quit`. It is deliberately prose, not
  clickable caps: a footer-row click while a chart/help/notices overlay is open is
  the "click outside the box to close it" gesture, so a clickable quit there would
  be an accidental-exit footgun. The all-dim line (the cyan caps gone) is itself
  the signal that the main keys are off. The overlay's own in-box hint remains the
  source of its contextual keys.
- **Bubble Tea v2 idioms**: `View() tea.View` (alt screen AND `MouseMode` are
  set per-frame on the returned View, not program options), `tea.KeyPressMsg` +
  `msg.String()` for keys, the
  `tea.Mouse{Click,Motion,Release,Wheel}Msg` set with zero-based `X`/`Y` and a
  `Button` for mouse, lipgloss colors are `color.Color` values. Color
  downsampling for the terminal is handled by the runtime; styles here use the
  16 basic ANSI colors on purpose so any theme renders sanely.

## Keybinding conventions

Conventions a new key should follow. They are guidelines, not hard rules — a
pane may deviate where its UX genuinely calls for it — but holding to them keeps
the surface predictable as it grows.

- **Arrows select in lists; `j`/`k` walk prices.** `↑`/`↓` (with
  `pgup`/`pgdn`/`home`/`end`) move a selection or scroll a window — the market
  selector, orders table, balances, the funding lists, and the account
  switcher (`handleNavKey`, `moveOrderCursor`, `scrollBalancesKey`, funding's
  cursors, `handleAccountSwitchKey`). `j`/`k` drive the orderbook **price**
  cursor — the order form's book ladder (`moveLadder`) and the trade ladder
  (`handleBrowseKey`) — so away from a price ladder they are not list navigation.
- **`tab`/`shift+tab` change focus; `enter` commits; `esc` backs out; `ctrl+c`
  quits.** These mean the same thing everywhere — don't repurpose them per pane.
- **One key per action; aliases only when the UX earns it.** Prefer a single key
  per action over an arrow+letter pair for the same move. Multiple keys are fine
  where they genuinely help: the trade ladder is a pure price cursor, so it takes
  both `↑`/`↓` and `j`/`k` (and both `pgup`/`pgdn` and `J`/`K`); a two-option
  scope selector takes both `←` and `→`. Before adding a key, check it isn't
  already an alias of another.
- **Letters are commands, not motion.** Command letters (`b`/`s`, `t`, `x`, `f`,
  `/`, `:`, …) act; they don't move a cursor. A letter typed into a numeric input
  or quick search is text, never a command.
- **A key you surface in a hint is clickable.** Footer and overlay hint caps
  dispatch the real `tea.KeyPressMsg` through `handleKey` (see the key-hint strips
  above), so a documented key works by click through the same handler — no
  parallel path.

## Scope decisions (v1)

- The order panel offers limit and market orders only; best (BBO) orders need
  `bestNth` + a mandatory TIF and stay on `order place`. Market orders carry
  price protection (`pp`) by default (a panel toggle turns it off).
- Multiple sub-accounts per session, one active at a time. `--account-seq`
  selects the subscribed set: a single value (`--account-seq 2`), a
  comma-separated list (`--account-seq 1,2,3` — subscribe several, active on the
  **first** listed), or omitted (subscribe every account the key may access —
  its `allowedAccountSeqs` — active on the key's configured default, else
  1/main). The account channels (myOrder/myTrade/myAsset) cover every subscribed
  sub-account; the panels show ONE active account at a time, switched in-session
  with the `@` switcher (see "Account switcher" below). The single-active model:
  the active account is `model.accountSeq()` (the one normalization point,
  returning `model.activeSeq`); the `order`/`ladder`/`funding` sub-models capture
  it at construction and `switchAccount` re-stamps their `accountSeq` fields on a
  switch — the funding screen included: it always operates on the **currently
  active account** and never defaults to main. Deposits/withdrawals are
  main-account-only server-side (the API accepts only `accountSeq` 1), so on a
  non-main account it shows a warning banner (`fundingModel.onMainAccount`) and
  the requests are rejected — it never silently retargets.
  Every store read goes through the per-account accessors
  (`OpenOrdersFor`/`BalancesFor`/`FillsFor`), so panels flip to "loading…" via the
  per-account readiness latches until the new account's backfill lands; order
  tracking is `{account, symbol}`-scoped and re-pointed on a switch; and the
  `Trader` takes the sub-account explicitly per call (a cancel targets the order
  row's own account). The cli layer (`cli/tuicmd`) resolves the subscribe set and
  the active account from the flag + the key's `allowedAccountSeqs`
  (`resolveTUIAccounts`); a configured default that is not itself allowed is a
  ConfigError before the alt-screen opens. `--account-seq` is private-only —
  passing it with `--public` is a usage error (public channels carry no
  sub-account), and the `@` switcher is a no-op in public mode. When
  `--account-seq` is **omitted**, expanding to `allowedAccountSeqs` depends on the
  startup `/v2/currentKeyInfo` read; if that read fails, the session refuses to
  start (preserving the failure's exit code) rather than silently narrow to the
  single active account — an explicit `--account-seq` needs no read and stays
  resilient to a transient preflight failure.
- Account backfill is snapshot-only (open orders + balances): the
  account channels cover every watched symbol, and full per-symbol history
  recovery on reconnect would be a REST storm over all launched pairs. Live
  fills still flow; historical gap fills are not recovered.
- The live feeds (orderbook/trades/fills) show the most recent rows with no
  scrollback — only the sidebar, orders, and balances are wheel-scrollable /
  indicator-bearing.
- The dragged layout and the chosen active symbol are session-only — not
  persisted to config.

## Localization (`--lang`, `internal/i18n`)

The TUI's core UI chrome — keystrip hints, dialogs, pane titles, form labels,
status words, toast prefixes — localizes through `internal/i18n`. Everything a
rendering path shows a user goes through `i18n.T("english text", args…)`; the
English text is the lookup key AND the fallback, so an untranslated (or orphaned)
entry renders English rather than stale text. Translations live in flat
per-surface files under `i18n/locales/{code}/` — the TUI's are `tui.json`
(key = the call-site literal, sorted; see the `i18n` package doc); the guard
tests in `i18n/guards_test.go` extract every `i18n.T` literal from the tree and
hold the files and call sites to each other — a new string fails until its ko
entry lands (the failure names the file it belongs in), an English copy edit
fails until the dead entry is updated, and an entry filed under the wrong
surface fails placement. `T` renders via `fmt.Sprintf`, so numeric verbs are
never locale-grouped; money still interpolates as a pre-formatted decimal
string (`%s`).

Rules for TUI text:

- **What localizes:** UI chrome only. Trading vocabulary (`buy`/`sell` values,
  `gtc`/`ioc`/`fok`/`po`, `bid`/`ask`/`mid`/`last` anchors, interval labels),
  symbols, currency codes, numbers, wire field names (`orderId`,
  `clientOrderId`, `seq`), the command-bar grammar and its parse prompts, and
  API error passthrough all stay English.
- **Whole sentences, not fragments:** when a message varies by case, pick
  between complete keys (`"filter markets: …"` / `"filter balances: …"`)
  instead of splicing a noun into one English sentence — target languages order
  words differently. When one English word carries two meanings that translate
  differently, give the sites distinct keys and map the extra key back to the
  shared English display with a `locales/en/tui.json` override (the canonical case:
  key `"market order"` displays "market" in English, 시장가 주문 in Korean).
- **Language is fixed at startup.** The display language is resolved once at
  startup by `i18n.Resolve` from the global `--lang` flag (else the host locale —
  `$LANG`/`$LC_*` on Unix, the preferred UI language on Windows — else English);
  `cli/tuicmd` then activates it via `i18n.Activate(i18n.Detected())` before the
  program starts, so the memoized components never see it change and there is no
  in-session toggle. The tui is a surface that localizes; a surface that does not
  activate stays English.
- **Widths derive from the active labels:** the form label columns
  (`orderLabelW`/`fundingLabelW`) and header-derived floors measure the
  localized strings with `lipgloss.Width` at render time — never hardcode a
  column to an English label's length. The uikit pad/clip helpers measure
  display cells (Hangul is two cells per glyph), with a byte-length fast path
  for the ASCII numeric content.

## Order mode (`b`/`s`, `modeOrder`)

`b`/`s` swap the right column for a docked **order
panel**: the orderbook stays live beside it as a *price picker* and the
open-orders slice stays beneath it, so an accepted order lands — and flashes
(`flashOrderID`, `orders.Key.FlashID`) — right below where it was placed.
There is no modal anywhere in the flow: the review (confirm) and busy states
swap the panel's content in place, and a rejection returns to the form with
the error inline and the inputs preserved. Facts to preserve:

- **It is a sub-model in the funding screen's mold** (`ordermode.go` state +
  keys, `orderview.go` render + click spans): the parent owns the mode switch,
  key/mouse routing, geometry, and the money single-flight; `handleKey`
  returns an `orderAction` (`close`/`place`) rather than dispatching itself,
  so `orderInFlight` and the Trader call stay parent-owned (`dispatchPlace`).
- **The engine is pure and lives in `orderentry.go`**: `orderDraft` (the
  sizing matrix as predicates — `usesQty`/`usesAmt` are the WIRE field, `inputAmt`
  is which size the user enters), `buildPreview` → `ops.AnalyzePlace` over the
  LIVE store book (the same simulation/warnings as `order place --dry-run`,
  re-evaluated per frame against `BookRev`-fresh data), `applyPreset`
  (%-of-balance sizing with quote-fee headroom), `anchorPrice`, and
  `placeGateReason`. No I/O — the fetched inputs (tick bands via
  `Config.TickSizePolicy`, fee rates via `Config.Fees`) are cached per symbol
  on the sub-model, best-effort: missing metadata degrades features (no grid,
  no fee estimate), never blocks entry.
- **A limit order can be entered by quantity OR amount** (`sizeInAmt`, toggled
  with `u`). Amount is a limit-only *input*: the wire always carries a quantity,
  derived at the price by `wireQty`/`qtyFromAmt` (the API has no limit-amount
  field). Market buys stay amount-native, market sells quantity-only.
  `orderDraft.validate` is the sizing-matrix guard both arm paths call before a
  review — it rejects any unsupported (type, side) shape so a bad combination is
  caught in tests, never sent.
- **Rounding (one policy, all pairs).** Quantity and amount are floored to the
  request precision (8 dp) by the single-source `roundQty`/`roundAmt` (and
  `divFloorQty`/`divFloorAmt` where a balance-sizing division is floored — same
  precision, exact via `QuoRem`). Price
  rounds to the symbol's **tick grid**, not a decimal count: a *programmatic*
  price (a `mid` anchor) snaps in the side's conservative direction — buy floors,
  sell ceils (`snapPriceForSide` → `ops.SnapToTick`/`SnapUpToTick`) — while an
  *explicitly typed* price is **checked, never modified** (`ops.OnTick`): an
  off-grid price is rejected with its tick, so the number the user typed is the
  number placed. When the tick policy has not loaded, the check defers to the
  server. The `:` command bar applies the identical rules.
- **The funding pane's one key rule, plus trading verbs.** `↑`/`↓` move the
  field cursor; `←`/`→` change the value under it on the selector rows
  (side/type/tif/pp cycle) but move the text caret on the numeric rows
  (price/qty/amt); `[`/`]` and `{`/`}` nudge the price ±1/±10 grid ticks on the
  `ops.StepTicks` grid and `%` cycles the size presets — all from any row (they
  never depend on where the field cursor sits), typing digits edits
  the input under it, `enter` advances (arm → place), `esc` backs out one
  step. On top: `j`/`k` walk the
  orderbook's visible levels (`orderbook.RowPrices`, which mirrors the render
  row for row — both take the header reservation from `splitHeader` and the
  depth slicing from `visibleSides`, and a header row maps to `""`, no price)
  writing the level's price into the draft;
  `b`/`s` declare the side and re-seed the price to the new side's own best
  level (`applySide` — a price picked for one side is usually marketable on
  the other, so it never survives a flip; the side row's `←`/`→` toggle
  applies the same rule), with the current side's key doubling as the
  join-the-touch re-anchor; `a` anchors to the *opposite* touch (the
  would-cross price); `m`/`l` anchor to grid-snapped mid (buy floors / sell
  ceils) / last; `u` toggles a limit order's size unit (quantity ↔ amount),
  carrying the size across at the price. **The price controls are limit-only** —
  on a market order the price row, the `j`/`k`/`a`/`m`/`l`/`[ ]`/`{ }` price
  keys, and the orderbook cursor arrow all disappear (a market order has no
  price, so offering to pick one only confuses). The deliberate
  deviation from the funding pane: the price/qty/amt
  inputs accept **digits and a dot only** (`editInput`/`numericText`), so
  command letters stay commands even while a numeric field holds the cursor —
  speed is the point of this panel, and unlike a free-form amount a stray
  letter is never valid here. A numeric input NOT under the field cursor
  renders its value grouped (`inputView` — a nine-digit price is unreadable
  raw); the draft floors the size to the request precision while the input keeps
  the raw text, so the preview and the placed order match.
- **Placement is freshness-gated, unlike display.** The gate has two halves.
  `orderGate`/`placeGateReason` is draft-independent: no money action in flight,
  the fee policy loaded, and the book **live** — `orderbookStatus != NotReady`,
  which a settled-but-EMPTY book satisfies. `draftGate(d)` adds what depends on
  the order itself: on an empty book only a limit that can REST (gtc/po) is
  allowed — it would be the first maker — while a market order (nothing to fill)
  and an ioc/fok limit (cannot rest, would cancel unfilled) are refused by the
  one shared `emptyBookRefusal`. Every surface passes **its own** draft: the
  panel via `panelGate()`, the command bar its resolved/armed order, the ladder
  each arm's. The gate reason lands inline and on the panel's book-state line. A
  pane can say "loading…" — an order cannot be sent against a book the store no
  longer stands behind.
- **Arming checks and freezes the review.** `arm` validates locally (size
  parses positive; a limit has an on-grid price — off-grid is rejected, never
  silently snapped), then swaps to the in-place review; `[`/`]` and `{`/`}`
  still nudge the armed price ±1/±10 ticks
  (`←`/`→` is inert while armed — it is the form's adjust key). The review gives
  the notional a `=` line of its own — it is the number a mistake hides in,
  so it never shares (or loses) its line to another figure, on any panel
  width. The busy view is
  escapable (the result still lands — as an inline form error, or a toast +
  flash on accept).
- **Drafts are sticky per symbol, per session** (`drafts`): reopening a pair
  restores its size/type/tif, but the price is re-seeded from the side's own
  best level (best bid for a buy, best ask for a sell) so a stale price never
  carries over silently.
- **Render and hit-test share one line builder** (`panelLines` →
  `orderFormLine`/`orderSpan`, consumed by `renderOrderPanel` and
  `panelClick`), so a row's on-screen position can never drift from its click
  behavior — the funding pane's pattern. The panel renders directly (not
  memoized): it is on screen only in order mode and its preview reads the live
  book anyway. Mouse: a click on an orderbook level writes that price into the
  draft (`bookPriceAt`), the wheel over the book walks the ladder cursor, a
  panel click moves the cursor / flips the value / presses the button, and
  clicks anywhere else are inert — leaving order mode is a deliberate `esc`,
  never a mis-click.

## Command bar (`:`)

`:` opens a one-line command bar above the footer (the body yields two rows;
`cmdBarVisible` folds them out of `bodyHeight`, so every geometry consumer
stays consistent). Like quick search it lives inside the normal view and owns
the keyboard while active (`handleCmdBarKey`); the footer steps back to prose
(`footer.Key.CmdBarOpen`). Facts to preserve:

- **The grammar parses to the same engine.** `parseOrderCmd` (pure; table
  test) → `resolveOrderCmd` materializes anchors (`b/a/m/l ±N` grid steps via
  `anchorPrice`+`ops.StepTicks`), checks an explicit price is on the tick grid
  (rejected off-grid, never snapped), and sizes percents
  through `applyPreset` — the result is an `orderDraft`, and the echo line is
  `buildPreview` of it, re-resolved every frame until armed. Size is a base
  quantity, a quote-currency amount (the quote-ccy unit, e.g. `500k krw`), or
  `N%`; `k`/`m` multiply any number (size or price) and carry no currency
  meaning. An amount sizes a market buy natively and any limit order by deriving
  the quantity at the resolved price (`qtyFromAmt`) — the API has no
  limit-amount field.
- **The echo fits its width by dropping facts, never by clipping the tail**
  (`cmdEcho` → `uikit.FactsLine`). What the order IS (side/size/type/price),
  the warning code, and the book state are rank 0 and always survive; the
  derived figures drop first (fee/note/tif/pp, then the notional), each drop
  disclosed by a dim `+n` tail. The key hints re-attach only when they fit
  whole — the footer carries them regardless.
- **Arming freezes; anchors carry drift protection.** The armed draft's price
  is what goes on the wire. If an ANCHORED price's anchor re-resolves more
  than `cmdDriftTicks` grid steps away, placing refuses and disarms with the
  reason; an explicitly typed price is never second-guessed.
- **One dispatch point.** `dispatchPlaceForm(form, origin)` is the only place
  an order leaves the TUI; `placeDoneMsg.origin` routes the result (bar
  rejections toast; panel and ladder rejections land inline on their surface).
  The bar never touches the panel's sticky draft.
- **Same gates as the panel**: private mode (`Trader` wired) to open;
  `draftGate(d)` on its OWN resolved (then armed) order to arm and to place —
  never the panel's draft, which may be a different order type entirely. The
  bar kicks `fetchMeta` on open so the tick/fee caches warm for its
  resolution. The normal-mode footer advertises it (a clickable `::cmd` cap,
  gated with the other order-entry caps), and the empty bar's echo line shows
  the grammar.
- **History is per session** (`recall`): ↑/↓ move through placed lines, which
  re-parse live (anchors stay market-relative until armed again).

## Trade ladder (`t`, modeLadder)

`t` rebuilds the orderbook+trades center region as a full-height, DOM-style
**trade ladder** — MINE | bid qty | price | ask qty | MINE rows over the live
book, asks above a last/spread rule, bids below — while the sidebar and the
account column stay. It is the fastest order loop the TUI offers: the size is
decided calmly in advance (a preset), the price is picked spatially (the
cursor), and a money mover still always costs exactly two deliberate keys
(arm + confirm). Facts to preserve:

- **It is a sub-model in the funding/order-panel mold** (`laddermode.go` state
  + keys, `ladderview.go` render + geometry, `components/ladder` the pure
  body): the parent owns the mode switch, key/mouse routing, geometry, and the
  money single-flight; `handleKey` returns a `ladderAction`
  (`close`/`place`/`cancel`) and only the parent dispatches
  (`dispatchPlaceForm`/`dispatchLadderCancel`). Opening shares the order
  panel's gate (`Trader` wired) and warms the same
  per-symbol tick/fee caches (`m.order.fetchMeta`) — one fetch feeds the
  panel, the command bar, and the ladder.
- **`ladder.Rows` is the single row-layout source.** Rows are the live book's
  **levels** (not per-tick — where the tick is small against the price, a
  per-tick ladder would show a sliver of the market); the first row is the
  column-header label row (`Row{Header:true}`, dropped on a pane shorter than
  `uikit.MinHeaderRows` so price rows win the scarce space), which carries no
  price and is inert to the cursor;
  tick-fine placement happens on the armed strip via
  `[`/`]` (±1) and `{`/`}` (±10) grid nudges. The render, the j/k cursor
  walking, and the click mapping all read the same Rows, so they can never
  drift. The component memoizes on `BookRev`+`OrderRev`+`TickerRev`+geometry+
  cursor/armed/flash scalars; the foot strip and title are model-rendered per
  frame (tiny), and the body-row count shrinks by exactly the strip's height.
- **Resting orders live on the ladder** (`store.OpenOrdersFor(accountSeq, symbol)`): each
  order's remaining size shows in the MINE column at its price row (buys left,
  sells right); an order on no visible level gets a **synthetic row** — a
  distant one pulls in as an edge row displacing the level farthest from the
  spread, an inside-spread one inserts at its sorted position — so nothing the
  account owns is ever off-screen. A just-accepted order flashes its MINE cell
  (the shared `flashOrderID`), and `x` at a MINE row arms a cancel of the
  resting order there (the newest when several rest at one price), capturing
  its identity at arm time.
- **The size chip is armed state, per symbol, per session** (`sizePcts`):
  number keys arm a %-of-balance preset from the configured levels (default
  `1-4` → 10/25/50/max; see the size-presets note above); the preset resolves to a
  quantity through the engine's `applyPreset` **at arm time, at the cursor
  price** (so a limit buy's 100% preset is exact and fee headroom applies).
  There is no typed custom size on the ladder — the panel and the command bar
  cover that. While browsing, the foot strip resolves the armed preset live
  at the cursor price, per side (`presetEstimates`, the same
  `applyPreset`→`buildPreview` path the arm and its confirm strip run),
  flagging a notional the server would reject (`⚠ >max` / `⚠ <min`) — a
  %-preset on a large balance can breach the order-notional cap, and that
  must be visible before the arm, not on the confirm strip. The foot line reads
  as three groups: the buy/sell **estimates** carry the book's bid/ask colors
  (buy = `pal.Up`, sell = `pal.Down`, the same colors the ladder rows use — so
  the live block stands out and buy vs sell reads at a glance), a `│` marks the
  boundary, then the dim **action hints**; `⚠` stays warn-yellow throughout. Each
  fragment is styled on its own — the line is never wrapped in one outer style,
  since an inner color/warn segment's ANSI reset would otherwise leak past it.
- **The default-tif chip is armed state too, but session-wide** (`defaultTifIdx`,
  set with `t` while browsing, shown as a title chip beside the size). Unlike the
  per-symbol size preset it is one preference across symbols, and it is **not**
  persisted (resets to `gtc` on restart). Every limit arm inherits it
  (`armOrder` stamps `draft.tifIdx`); a market arm ignores it (ioc-only). The
  per-order `t` in the confirm strip edits the armed copy, never this default.
- **The size and tif title chips are real steppers, not decoration**
  (`titleLayout`, `titleFocus`). Both render guilleted (`‹ … ›`), and exactly
  **one is focused** (reverse-video; browse only). The direct keys set the focus
  (a number → size, `t` → tif) as well as acting on that chip; `←`/`→` then steps
  the focused chip (`stepChip`→`stepSize`/`stepTif`; the tif
  wraps, the size clamps at both ends — matching the order panel), and a click on
  `‹`/`›` steps it directly while a click on the value
  just focuses it (`ladderTitleChipAt`, wired in `model.go`'s `modeLadder` click
  block). Render and hit-test share `titleLayout`, so the click targets can't
  drift from what's drawn; the columns are derived by `lipgloss.Width`
  accumulation over the plain fragments, never by scanning the styled string
  (the same technique as the order panel's `selector`/`presetLine`). Vertical
  arrows stay the cursor (`j`/`k`/`↑`/`↓`); only horizontal arrows touch chips.
- **Arming freezes; the same gates hold.** `b`/`s` arm a limit at the cursor
  row, `B`/`S` a market order (pp on by default); the confirm strip at the
  ladder's foot echoes the fully resolved order via `buildPreview` (notional,
  fee, book freshness — plus the preplace warnings, each on its own strip
  line like the panel: the facts line truncates at narrow widths, and a
  clipped warning is a safety disclosure silently lost) live each frame,
  `t` cycles the tif of **this** limit arm only (the draft is a copy — it
  never moves the ladder's default tif; a market order is ioc-only),
  and enter places under `draftGate(l.armed.draft)` — re-judged at confirm
  because `t` may have cycled the tif since arming, which changes the verdict on
  an empty book. Arm and place both refuse against a stale book. A cancel arms/places under the money single-flight only (like
  `x` elsewhere, it must not require a fresh book). A rejection returns to the
  armed strip with the error inline (nudge and re-place, or esc out); a cancel
  result returns to browsing and reports via the standard toast.
- **Mouse:** a click on a price row moves the cursor, the wheel walks it one
  row per notch (browse only); a click on a title chip's `‹`/`›` steps it (its
  value focuses it) — the only in-panel click spans (`ladderTitleChipAt`). The
  armed *strip* is confirmed by keyboard or the footer's clickable caps and
  carries no click spans. `c` recenters the cursor on the row nearest the last
  trade. The footer follows
  the ladder's views (`footer.Key.LadderView`), staying live and clickable
  like order mode (the ladder docks into the layout; it is not an overlay).

## Cancel all (`X`)

`X` opens the **cancel-all** dialog (`modeCancelAll`, state in
`cancelAllState`). Unlike `x` (cancel one), it acts on a *snapshot* of the
open-orders set rather than the table selection, so it needs **no pane focus** —
it is live from any pane and advertised in every pane's footer. Facts to
preserve:

- **The set to cancel is SNAPSHOTTED, not live.** Open orders come and go
  constantly, so the dialog freezes the target set (`snapshot`, captured by
  `tryCancelAllSnapshot`) the moment it opens **or the scope changes** — and only
  once that scope's orders are loaded. The confirmed count can't drift under the
  user. Once `snapped`, new order events do **not** re-snapshot
  (`maybeSnapshotCancelAll` is a no-op when `snapped`/`running`).
- **Two scopes: the active pair or all pairs** (`allPairs`, toggled with
  `←/→/tab`). The default follows the current open-orders view. Selecting all
  pairs calls `prepareCancelAllScope` → `SetTrackedOrders(allSymbols)` to turn on
  track-all, and while any watched pair lacks an authoritative snapshot
  (`cancelAllScopeReady`) the dialog shows a **loading** state with an `N/M pairs
  ready` count (`cancelAllLoaded`) and **refuses to confirm** — the user waits
  until the count settles. (The count also makes a stall visible: readiness only
  advances as each pair's `/v2/openOrders` backfill succeeds, so a failed per-pair
  fetch would otherwise look like a silent spinner.)
- **No bulk endpoint exists, so the batch issues one cancel at a time.** Confirm
  runs the snapshot sequentially through the same `Trader.Cancel` as `x`
  (`dispatchNextCancelAll` → `cancelAllStepMsg` → next), so the single-action
  `orderInFlight` gate holds throughout and the per-order canceling row style
  shows progress. `esc` mid-run sets `aborted` (stop after the in-flight cancel
  returns); the step handler tallies ok/failed and closes with a summary toast.
- **State resets on close** (`closeCancelAll` zeroes `cancelAllState` and restores
  the tracked-order set to the visible scope via `applyOrderTracking`), so a
  reopen always starts clean — no stale snapshot, scope, or progress.

## Account switcher (`@`, `modeAccountSwitch`)

`@` opens a small centered popup listing the session's subscribed sub-accounts
as a **`seq` · `name` table** (state in `model.accountSeqs`; seq 1 is named
`main` — `accountName` in `view.go` — other sub-accounts have no name), for
switching which account the panels, tracking, and order entry follow. It is a
private, multi-account feature — inert (a toast) with a single account. Its
footer/`?` hints and the chip's underlined switch affordance appear only then
(the header's `accountSeq:` chip itself shows in any private session, just dimmed).
Facts to preserve:

- **The active account is the single value `model.accountSeq()` returns** (from
  `model.activeSeq`), and `switchAccount` is the one place it moves: it re-stamps
  the `order`/`ladder`/`funding` sub-models' captured `accountSeq`, re-points the
  session's `{account, symbol}` order tracking, and rebuilds the open-orders
  projection. The balances/fills/orders panes read through the per-account store
  accessors, so they follow on the next render — showing "loading…" via the
  per-account readiness latches until the new account's snapshot/backfill lands.
  Funding also drops its per-account caches (`setAccountSeq`) so a reopen never
  shows the previous account's transfer history or withdrawable amounts.
- **Layout** (`accountSwitchParts`): a shared `overlayParts`, so rows and the
  clickable hint caps (`↑/↓`, `enter`, `esc`) hit-test through the same machinery
  as every other modal (`modalOverlayParts`/`stripKeyAt`/`overlayBodyAt`) — render
  and click map can't drift. The active account is marked by a `›` chevron (no
  "active" label); the pending selection is a separate reverse-highlighted row. The
  box shows at most `accountSwitchMaxRows` rows and scrolls (`acctScroll`, clamped
  by `ensureAcctVisible`, with an `n-m/total` indicator), so it stays a stable size
  as the account set grows.
- **Selecting is not committing.** `↑/↓`/`pgup`/`pgdn`/`home`/`end`/wheel move the
  highlight; `enter` commits (switch + close); `esc`/outside-click/close-cap closes
  without switching. A click **selects** a row (moves the highlight, no switch); a
  click on the **already-selected** row **confirms**, like `enter` (`clickAccountRow`).
  So a stray click never switches, and the popup is fully mouse-drivable.
- **Opening the switcher is gated (`openAccountSwitch`, the single entry point for
  both `@` and the chip click).** It opens only from the normal view, private and
  multi-account, with no money action in flight; otherwise it toasts why. The
  money-in-flight guard (`moneyActionInFlight()`, which folds in `funding.busy()`)
  is load-bearing: switching the displayed account out from under a
  place/cancel/withdrawal still on the wire breaks "what you see is what is
  happening", so the in-flight action finishes first and the switch waits.
- **What moves with the active account.** Store reads (orders/fills/balances),
  order tracking, new-order placement, and the order/ladder balance-sizing all read
  `accountSeq()`. Fee estimates are per sub-account too: the `Fees` seam takes the
  account explicitly, `switchAccount` clears the order panel's fee cache
  (`clearFees`), and a fee reply for a since-switched-away account is dropped
  (`orderFeesMsg.accountSeq`) — so a preview never mixes tiers. A **cancel** always
  targets the order's OWN account (captured), which need not be the active one.
- **The header `accountSeq:` chip is the always-visible indicator and a second entry
  point.** It shows the active account left of `key:<name>`, **underlined** (the
  key-cap affordance) when the session is multi-account to mark it a clickable
  switch target, else dim. A click routes through `openAccountSwitch` (`acctChipAt`
  over `header.AcctChipSpan`, the header's single render/hit-test source).

## Funding screen (`f`, `modeFunding`)

`f` (or `enter` on the focused balances pane) opens the **deposit/withdrawal
screen** — a full-screen master–detail view, crypto and KRW: the currency list
on the left, and on the right a **persistent request pane** over the transfer
history. It is a self-contained sub-model (`fundingModel` in `funding.go`,
rendering and hit-testing in `fundingview.go`, the inline form helpers in
`fundingform.go`); the parent model owns only the mode switch, message/key
routing, and the shared money gate. All data and actions go through the
`Funding` seam (implemented in `cli/tuicmd/funding.go` over the same ops
operations as the `deposit`/`withdraw`/`krw` commands — validation,
journaling, and the single-shot policy for money movers included). Facts to
preserve:

- **The left list merges three live sources**: the public currency catalog
  (`/v2/currencies`, fetched once), the streamed balances, and the streamed
  tickers. Order: KRW pinned, held assets by estimated KRW value descending
  (balance × the pair's last price — shown **only** when that pair's ticker is
  subscribed and ready; unpriced holdings sort after, alphabetical), a divider,
  then the unheld rest. The estimate is display-only arithmetic; wire values
  never derive from it.
- **The request pane IS the form — there are no funding overlays.** The pane
  always shows the selection's fields (direction chips, `‹ network ›`,
  `‹ destination ›`, the amount input, the action button) plus the facts the
  request is checked against (withdrawable amount, fee/min/precision, the KRW
  app-push note) — visible *before* anything is committed, not inside a modal.
  The **confirm** and **busy** states swap the pane's content in place
  (`fundingView`); the history below stays visible throughout. The pane is
  content-sized (`formHeight`); only the list | detail seam is draggable.
- **One key rule, everywhere in the pane** (`handleBrowseKey`): `↑`/`↓` move
  the field cursor, `←`/`→` change the value of the selector under it
  (direction: ← deposit / → withdraw; network and destination: step), typing
  edits the amount input under it, `enter` activates — the button acts, the
  deposit address/memo copy, anything else just advances, so enter alone walks
  the form. While the amount input holds the cursor every unclaimed key is
  text (a command letter typed there must never fire a command; the strip
  drops the `/`/`r` caps then). There are **no** per-action letter keys
  (no `w`/`d`/`g` or `[`/`]`); the active keys are `x` (cancel a history row),
  `r`, `/`, `=`. `tab` cycles the focus ring: list → request pane → history. The `/`
  currency filter follows the main quick search's contract: typing moves a
  **highlight** only (`searchSel` — the real selection and detail panes don't
  move), `enter` commits it, `esc`/backspace-on-empty cancel with no effect,
  and the mouse (row clicks, wheel) is suppressed while filtering.
- **Render and hit-test share one line builder** (`paneLines` →
  `fundingFormLine`): each pane row carries its field identity and clickable
  sub-spans (chips, `‹`/`›` arrows, copy targets, the button, the in-pane
  confirm's enter/esc), so a click's meaning can never drift from what is drawn.
- **The three request actions** (crypto withdrawal, KRW deposit push, KRW
  withdrawal push) require a key provisioned with `setup --with-transfers` (the
  registration link omits the transfer permission bits otherwise). Deposit-address
  **generation** (idempotent server-side, returns the existing address) and
  withdrawal **cancel** need no such key — stopping money movement must not be
  gated.
- **The withdraw flow reads top to bottom: network → address → amount.** The
  `‹ destination ›` selector offers only registered addresses
  (`withdraw addresses`) **filtered to the selected network**, and it is
  **never auto-selected**: it starts on `‹ select — n registered ›` — even
  with a single registration — and the button refuses until the user picks one
  explicitly (`←` from unchosen is a no-op; there is no way back to "none").
  The pick is **dropped whenever the ground moves under it**: a network cycle,
  a currency switch, or a refetched address list (`fundingWdAddrsMsg`) — a
  kept index could silently point at a different address. The amount is the
  form's only free-form input — validated against the network's min/precision
  and the withdrawable amount (`validateWithdraw`), then re-validated by the
  ops layer. With **no** addresses registered on the network, the address row
  itself explains that (with the developers-portal pointer) in place.
- **The deposit address and its memo/tag are copy targets.** `enter` (or a
  click on the value) copies via **OSC 52** (`tea.SetClipboard`) — the TUI
  captures the mouse, so native terminal text selection is unavailable and
  this is the copy path. The ack is a parent footer toast (`fundingCopiedMsg`,
  deliberately not the sticky banner); OSC 52 support depends on the terminal.
- **Every request passes an explicit in-pane confirm step**, and its outcome
  handling is asymmetric on purpose (`applyActionResult`): a **definite
  rejection** (a Korbit error envelope or a validation error — provably not
  executed) returns to the form with the error inline and inputs preserved; an
  **ambiguous failure** (timeout/transport) is NEVER retried — the view lands
  on the freshly refetched history under a standing "outcome UNKNOWN — verify
  below" banner, because resending an unacknowledged money mover can
  double-spend. Success lands on the same refetched history with the receipt
  and **clears the form** (a stale amount or pick must not ride into the next
  request). The banner renders FIRST in the pane (it tail-truncates) and
  persists until a refresh or the next action — deliberately, for the unknown
  case.
- **Detail fetches are seq-stamped and debounced.** Scrubbing the currency list
  debounces the fetch (`fundingSettleMs`), and every history/withdrawable reply
  carries the seq of the request that asked for it — a stale reply for a
  currency the user moved past is dropped, never rendered.

## Tests

`tui_test.go` drives the model directly (messages in, rendered frames out,
ANSI stripped) — no real terminal. The Trader is faked; the cli-side
implementation is covered end-to-end in `cli/tuicmd_test.go` through the
`Deps.TUIRun` seam with a stub HTTP doer and fake WebSocket conns.
`funding_test.go` covers the funding screen the same way (a fake `Funding`
seam; the send-exactly-once, rejection-vs-ambiguous, and gating properties are
pinned there), and `cli/tuicmd/funding_test.go` pins the seam's endpoint
routing and response mapping over a scripted wire.
