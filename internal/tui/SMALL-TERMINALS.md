# Small terminals & constrained space

How the TUI behaves when the terminal is small and how every surface copes
when a line has less room than its content. The rule all of it serves:

> **Space pressure must never silently cost the user a safety-relevant fact.**
> When something has to give, the TUI drops whole low-value facts and says so,
> compacts derived figures honestly, or marks the cut — it does not blind-clip
> a line whose tail might carry a warning, a gate state, or a money figure's
> unit.

## Terminal size floors

Constants in `view.go`, chosen per mode by `sizeFloors`: private mode has the
four-column floors, public mode the smaller three-column floors (no account
right column).

- **Hard floor 100×28 private / 60×18 public.** Below it, `render()` replaces
  the whole screen with a resize notice naming both sizes. The model keeps
  running — data, keys, and subscriptions are unaffected, and resizing back
  restores the full UI.
- **Recommended 120×32 private / 80×24 public.** Between the floor and this, the
  TUI runs normally with a `⚠ WxH — best ≥WxH` chip in the header beside the key
  identity (`crampedNote` → `header.Key.Cramped`): every pane works, but long
  figures and fact lines may truncate.
- At and above the recommended size, everything renders whole with the
  default pane split. (Pane seams are user-draggable, so a deliberately
  narrowed pane can reintroduce pressure at any terminal size — the coping
  rules below apply to the *pane* width, whatever caused it.)

The header's two lines clip at the terminal width rather than wrapping — a
wrapped header would shift every body row under the mouse hit-tests.

## The coping toolkit (`uikit`)

- **`Truncate(s, w)`** cuts a styled line to `w` display cells and marks the
  cut with a trailing `…` — a reader can always tell content was dropped.
- **`ClipTail(s, w)` / `PadLeft(s, w)` / `PadRight(s, w)`** are the cell-level
  (rune-based) primitives, and their truncation *direction* is chosen per
  content type: quantities and labels keep their **head** (`ClipTail`,
  `PadRight` — most-significant digits survive), while prices in dense tables
  keep their **tail** (`PadLeft` — adjacent book levels differ at the least
  significant end, and the shared head is obvious from context).
- **`FactsLine(w, sep, facts)`** joins ranked facts (`Fact{Text, Rank}`) and
  fits the width by **dropping whole low-rank facts**, never by clipping the
  tail. Rank 0 never drops; within a rank, later facts drop first; every drop
  is disclosed by a dim `+n` tail. A line of only rank-0 facts falls back to
  `Truncate`.
- **`Compact(v, w)`** renders a decimal-string figure exactly (grouped) when
  it fits, otherwise as a three-significant-digit unit form (`9.62B`,
  `48.7k`). It is for **derived display figures only** — typed inputs and
  wire values stay exact decimal strings end to end. Compacting beats
  clipping for money: a tail clip fakes precision and a head clip destroys
  magnitude.

## Where each rule is applied

- **Preplace warnings always own their line.** The order panel
  (`warningLines`) and the trade ladder's confirm strip (`confirmStripLines`)
  render each warning on its own line, code first, would-not-execute classes
  styled as errors — a warning is never part of a truncatable facts line.
- **The command-bar echo** (`cmdEcho`) is one line by design, so it fits via
  `FactsLine`: what the order *is* (side/size/type/price), the warning code,
  and the book gate state are rank 0 and always survive; derived figures
  (fee, anchor note, tif/pp, then the notional) drop first. Key hints
  re-attach only when they fit whole — the footer carries them regardless.
- **Units lead figures on tail-truncating lines.** The order panel's `avail`
  line renders `KRW 1,000,000 · BTC 0.5` (`availLine`): if the tail clips, it
  eats an unstarted token instead of leaving a figure wearing the wrong unit
  (`9,621,139,961 K` reads as thousands) — and when the exact line overflows,
  the figures render through `Compact` first.
- **The balances pane** keeps a one-column gutter between the available and
  total columns (two full-width clipped figures must never read as one
  number) and renders its cells through `Compact`, so a narrow account column
  shows `9.62B` rather than a digit soup.
- **The order panel's notional rows** — the form's `notional` row and the
  confirm's `= 12,345,678 KRW` line (`fitQuote`) — render exactly when they
  fit and fall back to `Compact` with the unit kept. The notional is the
  number a mistake hides in, and an order-of-magnitude error is *more*
  visible in `9.62B` vs `962M` than in a clipped digit string.
- **Tables** (open orders, fills, transfers) size columns by weight and clip
  cell content per column (`ClipTail`) instead of dropping columns; the
  status column stays visible at any supported size.

## Adding UI

Read the pane geometry from `colWidths`/`rightHeights` (never hardcode a
width), and when composing a line that could outgrow its pane: give a safety
fact its own line or rank it 0 in a `FactsLine`, route derived money figures
through `Compact`, and let everything else truncate — honestly, with the
`…`.
