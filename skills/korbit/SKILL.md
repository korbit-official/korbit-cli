---
name: korbit
description: >-
  Operate the Korbit cryptocurrency exchange (korbit.co.kr) through the korbit-cli tool — its CLI
  commands or its MCP tools. Consult this skill BEFORE running any `korbit` command or Korbit MCP
  tool: it carries the safety rules (dry-run-first order placement, idempotency, decimal-string
  money) and the right workflow for each task. Use it whenever the user wants to do anything on
  Korbit — place, cancel, or track an order, buy or sell crypto, check prices/orderbooks/candles/
  balances/fees/fills, watch a market or set a price alert, stream live market or account data,
  deposit or withdraw funds, set up or fix Korbit API keys, or work out what happened to a Korbit
  order. Trigger it even when the user only says "Korbit", names a KRW pair like btc_krw, or
  mentions the korbit/korbit-cli command, and even for simple one-step requests. Do NOT use it for
  other exchanges (Upbit, Binance, Coinbase), for building a standalone trading bot from scratch, or
  for hacking on the korbit-cli source itself.
---

# Operating Korbit with `korbit-cli`

You drive the Korbit exchange through one self-describing tool — **never** call the Korbit REST API
directly, handle raw secrets, or compute signatures. The tool owns keys, signing, idempotency,
clock-sync, retries, and a local action journal. Your job is to pick the right command for the
user's goal and follow the safety rules below.

## Two surfaces, one behavior

You'll be operating the tool in one of two ways. **All the workflows and safety rules in this skill
apply identically to both** — only the call syntax differs.

- **Shell (CLI):** run `korbit <command> … --json`. This is the full surface, including setup, keys,
  `doctor`, `logs`, `sandbox`, and `monitor`. If the `korbit` command isn't found, the one-line
  installer places the binary at `~/.local/bin/korbit` (macOS/Linux) or
  `%LOCALAPPDATA%\bin\korbit.exe` (Windows) — invoke it by that full path, or add the directory to
  PATH. (`go install` instead produces a binary named `korbit-cli`.)
- **MCP tools:** if a `korbit` MCP server is connected, each REST command is a tool. `korbit order
  place --symbol btc_krw --side buy …` ≡ calling the **`order_place`** tool with `{"symbol":
  "btc_krw", "side": "buy", …}`. Tool argument names are the long flag names without `--`; money
  stays a **string**; the result is already structured JSON (no `--json` needed). Most local/ops
  commands — `key` (list excepted), `logs`, `debug bundle`, `sandbox`, `monitor` — are **not** tools;
  do those in a shell. **The exceptions, exposed as tools so onboarding works entirely in chat, are
  `setup` and `doctor`** — use them to take a keyless user from nothing → a working key
  without a terminal (see Setup below). `list_keys` and `whoami` tools exist for key/account
  visibility. The **`order_place` tool takes a `dryRun: true`** argument that previews the
  order — it returns the simulated fill + `warnings` and places nothing — so use it before a real
  placement just like `--dry-run` on the CLI.

**A shell-out from an MCP session doesn't inherit the signing key.** An MCP server signs with one
fixed *launch key*, but a shell you spawn resolves its key independently and falls back to the CLI's
*default* — possibly a different account. So on **any** signing CLI-only command run from an MCP
session, pass `--key <the server's launch key>` explicitly. Today that's chiefly
`monitor [--symbols <pairs>] --my-orders/--my-trades/--my-assets` (`--symbols` required except for
`--my-assets`-only). The rule holds for any authenticated shell-out. Find the launch-key name via
`list_keys` (the entry with `isLaunchKey: true`) or the server
instructions. Public reads and account-independent commands (`logs`, `debug bundle`) don't need it.

**When MCP hits a wall, point to the CLI + Claude Code.** MCP is a subset — no `monitor` (streaming:
waiting for a price, live-watching orders/fills/balances, alerting, capture), no `sandbox`, `logs`, or
full key management (only `setup`/`doctor`/`list_keys`) — and some hosts refuse money-moving actions
outright. When the user needs one of those and you have no shell, don't just say you can't: tell them to
install the CLI (`curl -fsSL https://docs.korbit.co.kr/install.sh | sh`; Windows `irm
https://docs.korbit.co.kr/install.ps1 | iex`) and drive `korbit` from **Claude Code** — everything works there.

Examples below are written in CLI form. Translate to a tool call when you're on MCP.

## Pick the right tool for the goal

Match the user's intent to the workflow. When in doubt about exact flags/enums, run `korbit commands
--json` (the full machine catalog) or `korbit <command> --help` — don't guess.

| The user wants to… | Reach for | Details |
|---|---|---|
| Look up a price, orderbook, candles, trading pairs, fees | `ticker` · `orderbook` · `candles` · `trades` · `pairs` · `fees` · `currencies` | one-shot reads |
| Check balances / their account | `balance` · `whoami` | size against `available` |
| Place / cancel / track an order | `order place` · `order cancel` · `order get` · `order open` | **The trading loop** below |
| Reconcile "what's open / did it fill / what have I traded" | `order open` · `order get` · `order history` · `fills` | **Reconciliation** below |
| **Wait** for a price/condition, then act | `monitor … --jq '…' --max-events 1` | `references/monitoring.md` |
| **Live-watch** their orders/fills/balance changes | `monitor [--symbols <pairs>] --my-orders/--my-trades/--my-assets` (`--symbols` required except for `--my-assets`-only) | `references/monitoring.md` |
| **Alert** on a threshold or a reliability gap | `monitor … --jq` (notices included) | `references/monitoring.md` |
| **Capture** a bounded live dataset for analysis | `monitor … --json --duration/--max-events` | `references/monitoring.md` |
| Deposit, withdraw, or check funding status | `deposit *` · `withdraw *` · `krw *` | `references/funding.md` |
| **Test** a flow without real money | `sandbox start` … `--key sandbox` | `references/sandbox.md` |
| Find out "what went wrong / what did I do?" | `logs` · `debug bundle` | `references/debugging.md` |
| Set up API access, or fix a key/config problem | `setup --json` → register → `setup --json --wait` (auto-binds + health-checks) | **Setup** below |

`monitor` is the workhorse for the whole research + monitoring + alerting family — it replaces
polling whenever the user needs to *wait for* or *continuously observe* something. Read
`references/monitoring.md` before reaching for a poll loop.

## Output & exit codes (how to parse results)

Pass **`--json`** on every CLI command — stdout is human-readable text by default, and you want the
one-JSON-document contract. (`--compact` is single-line JSON and also implies `--json`.) Over MCP,
results are already JSON.

- **Success:** exactly one JSON document (the API's `data`, field order preserved). Bare acks like
  cancel normalize to `{"success": true}`.
- **Errors** go to **stderr** and follow the same split: a plain `error: …` line by default, or under
  `--json` the envelope `{"error":{"type","message","httpStatus"?,"code"?,"retryAfterSec"?,"guidance"?}}`. The
  symbolic reason is `error.code` (e.g. `NO_BALANCE`); `httpStatus` is the HTTP status; `guidance`, when
  present, is recovery advice you must act on (e.g. a duplicate place: the order is placed, do not re-send).
- **Incomplete paged results announce themselves:** `order history`/`fills`/`candles`/funding
  histories normally return a bare array, but a partial result is wrapped
  `{"data":[…],"truncated":true,"note":"…"}`. Treat `truncated:true` as "older rows exist that you
  didn't get" — never as complete.

**Exit codes** (gate on these):

| code | meaning | what you do |
|---|---|---|
| `0` | success | — |
| `1` | network failure or internal error | idempotent calls were already auto-retried within budget; a network blip is worth one more try, but a repeat is likely a real internal error — don't loop, report it |
| `2` | usage error | fix your flags — the message says how |
| `3` | API rejected | read `error.code` (see the rejection table below) |
| `4` | key/config problem | go to Setup; the message names the exact fix |

## Safety ground rules

1. **Live orders need explicit authorization — and a dry-run.** Before the first real-money order in a
   session the user must have clearly asked you to trade live (a size/strategy they stated); never
   invent sizes. Always dry-run first — `--dry-run` on the CLI, or `dryRun: true` on the `order_place`
   tool over MCP: it simulates the fill and emits `warnings`, and if it warns you double-check with the
   user before placing (see the trading loop).
2. **Money values are decimal strings** — pass prices/quantities/amounts through verbatim (`"0.001"`,
   `"50000"`). Never do order math in floating point; use integer/string/BigInt arithmetic. A float
   silently corrupts a price.
3. **Idempotency is automatic — you don't track `clientOrderId`.** Every `order place` auto-mints a
   UUIDv7 `clientOrderId`, **records it in the local journal before sending**, and reconciles by it, so
   a retry can never double-place. You don't need to remember, persist, or pass it. If a placement ever
   exits with `UNKNOWN` (the tool couldn't confirm the outcome), recover the id from `korbit logs
   --orders` and check the order's real state with `order get` before doing anything — never blindly
   resend.
4. **Accepted ≠ filled.** `order place` returns the full reconciled order, but *accepted* is not
   *filled* — read the returned `status` (a resting limit order is `open`). Confirm fills with `order
   get`.
5. **Size against `available`, not `balance`** — the difference is locked in open orders/withdrawals.
6. **Never expose key material.** The tool never prints private keys; you must never read the keystore
   file / OS keychain / PEM files into the conversation, and never echo API-key ids into chat
   unprompted. Keys may belong to different accounts.
7. **Moving funds is initiate-and-report.** The tool can *request* deposits/withdrawals but every
   fund-moving action has an out-of-band human gate (registered address, in-app push) you must not try
   to bypass. See `references/funding.md`. Never request a withdrawal the user didn't ask for.

## Setup (one-time, needs the human)

Korbit API keys need human identity verification in the developers portal, so you can't finish setup
alone. Two calls:

```sh
korbit setup --json          # step 1: generate a local ED25519 keypair (key "default"); returns a registrationLink + IP allowlist
korbit setup --json --wait   # step 2: wait for the human to register, then bind + health-check automatically
```

`setup --json` returns immediately — surface the `registrationLink` (and the IP allowlist to whitelist)
to the user. `setup --json --wait` then blocks until they finish registering, binds the key, and runs the
complementary `doctor`, so you **never ask them to paste the id back**. Use the **same `--name`** for both
(the default works; a different name makes a different keypair and link). The bound result lands on stdout
when they finish, or resume instructions after `--wait-timeout` (default 20m). Don't combine `--wait` with
`--api-key`.

**Fallback** — if you can't hold the `--wait` call open (or you're on MCP, below): surface the
`registrationLink`, have the user confirm with MFA and **paste back the issued key id** (a link can't
create a key, so it's safe to hand over), then bind + verify in one step:

```sh
korbit setup --api-key <KEY_ID_the_user_pasted> --json   # binds the id AND runs doctor
```

The `doctor` check is **advisory** — a problem is a warning, never a setup failure — so read the embedded
`doctor.ok`/checks; don't treat exit 0 as "healthy". Lead with *"just paste the key id and I'll finish
setup"*: mention the commands so the user *could* run them, but prefer to run them yourself.

**On MCP** the same flow runs through tools: call **`setup`** (returns the `registrationLink`), then
**`setup`** again with the pasted id as `apiKey`, then **`doctor`** — no terminal needed.

The first key you create becomes the **default**, so you can **omit `--key` on every command** — don't
thread `--key <name>` through routine calls. `doctor` exits `0` healthy (warnings OK), `4` if a
key/config problem blocks trading, `1` if only a network failure prevented verification — and each
non-ok check carries the exact `fix`. Run `doctor` at the start of a session or whenever something's
off. Re-running `setup` is safe: it resumes an unbound key (re-prints the link), or — for a bound key —
reports `alreadyConfigured` and re-runs the complementary `doctor` health check on it.

**Pass `--key <name>` only when there's a real choice to make.** Multiple keys may be different
accounts (`korbit key list --json` shows them); when more than one exists, pass `--key <name>`
explicitly and tell the user which one you used. The tool prints `signing as key "<name>"` to stderr
on every signed call (a safety disclosure, always shown), so you always know which key/account acted.

## The trading loop (placing an order safely)

**The dry-run does the risk-checking for you.** To place an order you need the **sizing matrix**
(which fields each type takes) and a clean dry-run.

**Sizing matrix** (the tool enforces this and its errors explain it):

| type | buy | sell |
|---|---|---|
| `limit` | `--price` + `--qty` | `--price` + `--qty` |
| `market` | `--amt` (quote currency to spend) | `--qty` (coin to sell) |
| `best` | `--amt` + `--tif` + `--best-nth` | `--qty` + `--tif` + `--best-nth` |

**Step 1 — dry-run (simulate + risk-check).** `order place … --dry-run` signs nothing and places
nothing; it prints the unsigned request plus two advisory blocks built from live **public** market
data (the orderbook + tick policy + the pair's order value bounds, at the same base URL the real order
would use, so no credentials are touched):

- `simulation` — the estimated fill against the current book: `bestBid` / `bestAsk` / `mid`,
  `marketable`, `estFilledQty`, `estAvgFillPrice` / `estWorstFillPrice`, slippage, `fullyFilled`, and
  the remaining-qty disposition. A reference price the book can't supply is `""` — an empty book side,
  or either side missing for `mid` — never `"0"`, so check it before comparing or computing with it.
  **Branch on `outcome`, not on `marketable`**: `"fills"` (some or all executes now), `"rests"` (nothing
  executes, the order sits on the book as a maker), `"nothing"` (nothing executes and nothing rests —
  rejected, killed, expired, or canceled in whole). `marketable` only says the order *would take*
  liquidity, which is equally true of a crossing post-only (rejected) and an unfillable fill-or-kill
  (killed); the disposition is prose for a human, never something to pattern-match.
  **Estimate only** — nothing is placed; the real fill moves with the book and fees/hidden liquidity
  aren't modeled.
- `warnings` — an array of `{code, message}`. Codes include `INSUFFICIENT_LIQUIDITY` / high slippage
  on a market order, `PRICE_FAR_ABOVE_MARKET` / `PRICE_FAR_BELOW_MARKET` (fat-finger),
  `POST_ONLY_WOULD_REJECT` (a crossing post-only the server would reject), a fill-or-kill that would be
  killed, `IOC_WOULD_EXPIRE` (an immediate-or-cancel that doesn't cross the book — it takes no liquidity
  and expires immediately with no fill), `PRICE_OFF_TICK`, and `NOTIONAL_ABOVE_MAX` / `NOTIONAL_BELOW_MIN`.
  `PRICE_PROTECTION_CAPPED` means price protection (`--pp`) held the fill inside its band and the
  remainder is canceled rather than filled at a worse price. `BEST_PEG_UNAVAILABLE` means a `best` (BBO)
  order's `--best-nth` level isn't present in the current book, so it can't be priced and would likely
  take no liquidity. `BOOK_DEPTH_LIMITED` means
  the order sweeps the entire visible book without filling — the book returns only so many levels per
  side, so the fill and slippage are a lower bound and the real cost may be worse (a coarser orderbook
  grouping via `--level` shows more depth). `NO_OPPOSING_LIQUIDITY` means the side this order takes from
  holds no resting orders and the order can't rest, so nothing executes — a `gtc`/`po` limit rests
  instead and is not warned about (read `outcome: "rests"` and `estRemainingQty`).
  `MID_PRICE_UNAVAILABLE` means no mid could be computed, so whichever of the far-from-market price
  check and the `--pp` estimate applied to this order did not run — the message names them; a plain
  limit never asked for protection, and a protected market order has no limit price to check. Verify
  the price against another source rather than reading that silence as a pass. An empty array means nothing was flagged. `checksSkipped` — a plan with no `simulation` at
  all — appears only when the market data can't be fetched; an empty or one-sided book is analyzed, not
  skipped. Be cautious on a skip.

```sh
korbit order place --symbol btc_krw --side buy --type market --amt 50000 --dry-run --json
#  -> { "request": {…}, "simulation": {…, "estAvgFillPrice": "94379999…"}, "warnings": [] }
```

**Step 2 — if `warnings` is non-empty, stop and double-check with the user** before placing. Show the
warnings (and the relevant simulation numbers) and get an explicit go-ahead — a warning usually means
the order would fill at an unfavorable price or be rejected outright. *Exception:* if the user already
asked for immediate/unconditional execution, place it and just report what the warnings said. A clean
dry-run (empty `warnings`) → place, but only where a check actually ran. Exactly two signals say one did
not: `checksSkipped` (no `simulation` at all — the market data couldn't be fetched, so nothing was
analyzed) and `MID_PRICE_UNAVAILABLE` (a mid-dependent check was suppressed — it is a warning, so it
also puts you in the non-empty case above). On either, verify the price against another source before
placing. An empty `bestBid`/`bestAsk`/`mid` is **not** such a signal — it states the book's shape, and an
order needing neither (a market sell into a bids-only book, no `--pp`) was checked in full. Place a real
order only once live trading is authorized (ground rule 1); start small.

**Step 3 — place, then confirm.**

```sh
# The tool auto-mints + journals the clientOrderId and echoes it back.
korbit order place --symbol btc_krw --side buy --type market --amt 50000 --json
#  -> full reconciled order: {"orderId":123,"clientOrderId":"019e…","status":"…",…}

# Confirm actual state when it matters (accepted != resting/filled):
korbit order get --symbol btc_krw --order-id 123 --json
```

`order place` is **reconciled, not blindly retried**: it sends once, resends only with the *same*
`clientOrderId` on an ambiguous failure (network/5xx/429) or after one clock re-sync on
`EXCEED_TIME_WINDOW`, resolves `DUPLICATE_CLIENT_ORDER_ID` by fetching the existing order, and returns
the full order — or exits non-zero with an `UNKNOWN` error if it genuinely can't confirm. It never
double-places, so you don't manage any of this. On `UNKNOWN`, find the `clientOrderId` in `korbit logs
--orders` and run `order get` before deciding anything — don't blindly resend.

Order `status` is `pending`, `open`, or `partiallyFilled` while the order is live (`pending` is not
yet committed); treat any other status as terminal (done). `avgPrice` is absent until something
fills — guard before reading it. Cancel is **asynchronous**: after `order cancel`, confirm with `order
get` before treating funds as free.

## Handling API rejections (exit 3)

| `error.code` | Meaning | What you do |
|---|---|---|
| `DUPLICATE_CLIENT_ORDER_ID` | An earlier attempt already placed this | Treat as success → `order get --client-order-id …`; do not resend |
| `NO_BALANCE` | Insufficient `available` (a buy whose fee is charged in the quote currency also reserves the fee) | Re-read `balance`, shrink the order |
| `ORDER_VALUE_TOO_SMALL` / `_TOO_LARGE` | Notional outside the pair's order value bounds, in the pair's quote currency | Resize; read the pair's own bounds from `pairs` (`minOrderValue` / `maxOrderValue`, in its `quoteCurrency`) — they differ per pair, and a pair that omits one has none |
| `PRICE_TICK_SIZE_INVALID` | Price off the tick grid | Round to the tick grid and retry |
| `TRY_AGAIN` (on cancel) | Order mid-processing | Auto-retried within `--retry-timeout`; if it still surfaces, wait ~1s and run the cancel again |
| `EXCEED_TIME_WINDOW` | Host clock drift | Reads/cancels/`order place` auto-resync once and retry; single-shot withdrawals don't — pass `--time-sync on` to sign with a corrected clock. `doctor` shows the offset |
| HTTP 429 (`retryAfterSec`) | Rate limited | Idempotent calls auto-wait within `--retry-timeout`; for a write, wait `retryAfterSec` then resend yourself |

A `--dry-run` pre-empts most of these (`PRICE_OFF_TICK`, notional bounds where the pair publishes
them, a would-cross/post-only reject) before you ever send — so a dry-run-first habit turns most exit-3 rejections into warnings you
handle up front. The tool auto-retries **idempotent** calls only (all reads, `order cancel`, `withdraw
cancel`, `deposit generate`) within `--retry-timeout` (default 5s). Money-moving writes are never
auto-retried.

## Reconciliation & checking what happened

Start a session (and recover after any crash) by reconciling before placing anything new:

```sh
korbit whoami --json && korbit balance --json && korbit order open --symbol btc_krw --json
```

- `order open` = current resting orders (authoritative for "what's live").
- `order history` / `fills` = last ~36h, may lag a few seconds — **not** for "did my order just land?"
  (use `order get` for that). Watch for `truncated:true`.

When the user asks **"what went wrong?"** or **"what did you do?"**, read the local action journal
rather than guessing — `korbit logs --json` (recent write calls) and `korbit logs --orders --json`
(every order this tool placed and whether it landed). See `references/debugging.md` for the full
playbook.

## Beyond this tool

This tool is for **research, monitoring/alerting, and simple automated trading** driven by an agent.
For a large, long-lived, full-featured custom trading bot, the better path is to build directly
against the Korbit Open API using the LLM docs at **https://docs.korbit.co.kr/llms.txt** (and
`llms-full.txt`) — that bundle is self-sufficient for implementing your own signed client. If you ever
need the tool's source as a reference (last resort), it's open source at
**https://github.com/korbit-official/korbit-cli**; prefer `korbit commands --json` and `--help` first.
