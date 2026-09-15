# Monitoring, alerting & data capture with `monitor`

`digitalx monitor` is the one **streaming** command — a resilient WebSocket client (auto-reconnect,
re-subscribe, REST backfill of gaps) that emits one event per line. It's the right tool whenever the
user needs to **wait for**, **continuously observe**, **alert on**, or **capture** something, instead
of taking a single snapshot. Reach for it before writing any poll loop.

> Scope note: this skill covers only the **stable** `monitor` surface (`--symbols`, the channel
> flags, `--json`, `--jq`, `--duration`, `--max-events`, `--no-backfill`, `--no-reconnect`,
> `--stream-log-level`). The JavaScript bot runtime (`--init`/`--where`/`--on`, behind
> `--enable-experimental`) is **out of scope** — its API isn't stable; don't build on it.

## Channels

Pass `--symbols btc_krw,eth_krw` once (the shared pair list every channel watches), then turn on at
least one channel — the channel flags are booleans, no per-channel symbols:

- **Public (no key):** `--ticker`, `--orderbook`, `--trades`, `--candles <intervals>`
- **Private (signed):** `--my-orders`, `--my-trades`, `--my-assets` (balance changes)
- `--symbols` is required for every channel except `--my-assets` (account-wide, no symbols); a
  `--my-assets`-only run needs no `--symbols`.
- `--account-seq 1,2` — accounts the private channels cover (default: the key's configured accountSeq, else 1/main).
- `--trade-history N` — with `--trades`, seed up to N recent REST trades (tagged `origin:backfill`)
  before live data.
- `--candles 1,60` — real-time OHLCV candles per listed interval (the candles endpoint's values:
  `1,5,15,30,60,240,1D,1W` — no aliases). This channel is **synthesized by the CLI** (`origin:
  "derived"`) from the trade stream plus REST seeds; the API itself has no candle WebSocket channel.
  Payload: `{interval,timestamp,open,high,low,close,volume,final}` — `final:false` updates the open
  bucket, `final:true` closes it (a no-trade bucket still closes on time, flat with volume `"0"`).
  Key bars by `{interval,timestamp}`; the newest line for a key supersedes earlier ones (corrections
  after a reconnect/gap re-emit). Feed **only `final:true` bars** to bar-based indicators (EMA/RSI/…)
  — per-tick updates compute a different, wrong quantity. `--candle-history N` streams N closed bars
  first to warm indicators up. Cannot combine with `--no-backfill`. Derived bars can differ slightly
  from the REST candles endpoint (client-side aggregation); use REST where exact history matters.
  Reliability rides the usual notices with `channel:"candle"` (`BACKFILL_*` for seeding,
  `SUBSCRIBE_FAILED` if the channel is dropped — same handling as any channel).

## Stopping is success

`monitor` runs until you stop it, and **every deliberate stop exits `0`**:

- **Ctrl-C / SIGTERM** — manual stop.
- **`--duration <dur>`** — stop after a fixed time, given as a duration string with a unit (ms, s, m, h), e.g. `--duration 90s`, `--duration 1m`, `--duration 1h30m`.
- **`--max-events N`** — stop after N emitted events (with `--jq`, after the Nth output line).

`--duration` and `--max-events` are what make `monitor` safe to run from an agent without hanging —
**always bound it** with one of them unless the user explicitly wants an open-ended stream.

Drops auto-recover by default. Add **`--no-reconnect`** to instead exit non-zero on the first drop or
failed connect (network drop `1`, rejected handshake `3`), after emitting the usual
`DISCONNECTED`/`FATAL` notices — for one-shot runs, or when a supervisor owns the restart.

## Output contract (NDJSON, `--json`)

One JSON object per line, data and notices interleaved in emission order:

- **Data:** `{"type":"data","channel","symbol"?,"origin","serverTime","source"?,"accountSeq"?,"payload"}`
  — `payload` is the verbatim source document (money/qty are decimal strings).
- **Notice:** `{"type":"notice","code","level","message","details"?,"time"}` — reliability signals:
  `CONNECTED`, `DISCONNECTED`, `DATA_GAP`, `BACKFILL_*`, `KEEPALIVE`, …

**Notices are the safety channel** — they tell you when the stream may have missed data. Treat
`DATA_GAP` / `DISCONNECTED` / `BACKFILL_NOT_VIABLE` as "your live view may be stale — re-reconcile via
REST (`order open`, `balance`) before trusting it."

**Reconcile, don't count.** Events aren't globally ordered across origins and can repeat (the backfill
guarantees at-least-once, not exactly-once). Key trades/fills by `tradeId`, orders by `orderId`, and
advance order state by lifecycle — never assume one event per change.

## `--jq`: the stable filter/transform path

`--jq '<program>'` runs a built-in jq program (no external `jq` needed) over **every** emitted line —
data events *and* notices — exactly like `digitalx monitor --json | jq`. It **implies `--json`**, and
`--max-events` then counts matching output lines. A line is emitted only if the program produces
output: use `select(…)` to filter, any other expression to reshape.

Gotchas that matter:
- **Guard on `.type`** before reading `.payload`, because notices flow through too:
  `select(.type=="data" and …)`.
- **Money fields are decimal strings** — compare with `tonumber` (`(.payload.data.close|tonumber) >
  100000000`), and never feed a jq-computed float back into an order price/qty.
- A passing, **unreshaped** event is re-emitted byte-for-byte (exact decimals preserved); only a
  reshaped value goes through jq's encoder.
- Notices appear on stdout by default, but a **data-only filter drops them**. `--stream-log-level` is
  **off by default** and independent of `--log-level`; set **`--stream-log-level warn`** to *also* log
  stream notices (at that level or above) so reliability signals — and their recoveries — are recorded
  without polluting your filtered/captured stdout. They go wherever logs go (the `--log-file` file,
  else stderr), tagged `component=stream kind=stream_notice` (so `grep kind=stream_notice`). (`warn` covers each
  problem *and* its resolution, since a recovery notice shares its onset's level.)

## Use-case patterns

### 1. Wait for a condition, then act (gating)
Block until something happens, exit `0`, then run the next command. The cleanest way to "do X when
price crosses Y":

```sh
# Exit as soon as BTC trades above 100,000,000 KRW:
digitalx monitor --symbols btc_krw --ticker --max-events 1 --stream-log-level warn \
  --jq 'select(.type=="data" and (.payload.data.close|tonumber) > 100000000)'
# ... then place/cancel an order, notify the user, etc.
```

### 2. Threshold / event alert
Stream and surface only the lines that matter, for as long as the user wants to watch:

```sh
digitalx monitor --symbols btc_krw,eth_krw --ticker --duration 1h --stream-log-level warn \
  --jq 'select(.type=="data") | {sym:.symbol, px:.payload.data.close, t:.serverTime}'
```

### 3. Live-watch the user's own account
Order/fill/balance changes in real time (signed — needs a key):

```sh
digitalx monitor --symbols btc_krw --my-orders --my-trades --my-assets --json
```

If you came from an MCP session, pass `--key` matching the MCP server (`list_keys` → the entry with
`isLaunchKey: true`) — this shell-out otherwise signs with the CLI's default key, a possibly different
account.

### 4. Capture a bounded dataset for analysis
Collect a fixed window of live data to a file, then analyze it:

```sh
digitalx monitor --symbols btc_krw --trades --orderbook --duration 2m --json > /tmp/btc_stream.ndjson
# Each line is one event; parse with any NDJSON reader.
```

### 5. Reliability / health watch
React to gaps without caring about the data itself:

```sh
digitalx monitor --symbols btc_krw --my-orders --jq 'select(.type=="notice" and (.code=="DATA_GAP" or .code=="DISCONNECTED"))'
```

### 6. Multi-symbol watch
List several pairs in `--symbols`; each event carries its own `symbol`:

```sh
digitalx monitor --symbols btc_krw,eth_krw,xrp_krw --ticker --duration 10m --json
```

### 7. Real-time candles for indicator logic
Stream closed 1m bars (with warm-up history) and act on each bar close — the right feed for any
EMA/RSI/MACD-style rule, instead of approximating candles from tickers:

```sh
digitalx monitor --symbols btc_krw --candles 1 --candle-history 100 --stream-log-level warn \
  --jq 'select(.type=="data" and .channel=="candle" and .payload.final)'
# Each line is one closed OHLCV bar; pipe into your indicator computation.
```

## When NOT to use `monitor`

For a single current value (one price, one balance, the current open orders) use the one-shot read
command (`ticker`, `balance`, `order open`) — don't spin up a stream. `monitor` earns its cost only
when you need to *wait* or *keep observing*.

`--dry-run` prints the resolved subscriptions + endpoints without dialing — useful to confirm what a
`monitor` invocation will watch before committing to it.
