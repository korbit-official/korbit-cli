// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"github.com/digitalx-official/digitalx-cli/internal/agentskill"
	"github.com/digitalx-official/digitalx-cli/internal/cmdmeta"
)

// Shared param definitions referenced across the builtin commands.
var (
	// withTransfersFlag opts the registration deep link into the deposit/withdrawal
	// *write* scopes so the generated key can run `deposit generate`, `withdraw
	// request/cancel`, and the `krw deposit/withdraw request` push commands. Off by default — a plain trading
	// key never requests withdrawal permission.
	withTransfersFlag = cmdmeta.Param{
		Flag: "with-transfers", API: "withTransfers", Kind: cmdmeta.KindFlag,
		Desc: "also request deposit/withdrawal write permissions in the registration link (needed for `deposit generate`, `withdraw request/cancel`, `krw deposit/withdraw request`)",
	}

	// keystoreFlag picks the keystore backend for a NEW key's private material
	// (`setup` / `key add`). The backend is recorded per key in keys.json; when
	// the flag is omitted the saved default (`keystore default`) applies.
	keystoreFlag = cmdmeta.Param{
		Flag: "keystore", API: "keystore", Kind: cmdmeta.KindEnum, EnumValues: []string{"file", "keychain"},
		Desc: "keystore backend for this key's private material: file | keychain (default: the saved `{prog} keystore default`)",
	}
)

// Registry is the declarative BUILTIN command surface. The endpoint command
// surface lives in the ops catalog; this holds the local builtins (setup,
// doctor, ip, key/keystore management, monitor, mcp, logs, debug, commands,
// license, sandbox) with their hand-written handlers.
var Registry = []Command{
	{
		ID: []string{"monitor"}, Section: cmdmeta.SectionMarket,
		Summary: "stream real-time market data and account activity over WebSocket, with an optional experimental JavaScript bot runtime",
		Params: []cmdmeta.Param{
			{Flag: "symbols", API: "symbols", Kind: cmdmeta.KindSymbols, Desc: "comma-separated trading pairs to watch, e.g. btc_krw,eth_krw — the shared symbol set every enabled channel (--ticker/--orderbook/--trades/--my-orders/--my-trades) subscribes to. Required unless you subscribe only --my-assets (which is account-wide and takes no symbols)"},
			{Flag: "ticker", API: "ticker", Kind: cmdmeta.KindFlag, Desc: "stream the ticker channel for the --symbols pairs"},
			{Flag: "orderbook", API: "orderbook", Kind: cmdmeta.KindFlag, Desc: "stream the orderbook channel for the --symbols pairs"},
			{Flag: "trades", API: "trades", Kind: cmdmeta.KindFlag, Desc: "stream the public trade channel for the --symbols pairs"},
			{Flag: "trade-history", API: "tradeHistory", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(500), Desc: "with --trades, seed each trade subscription with up to N recent trades fetched from REST before live streaming (emitted as origin=backfill) — the live WebSocket snapshot's row count is server-defined and may be a single trade"},
			{Flag: "candles", API: "candles", Kind: cmdmeta.KindString, Desc: "stream real-time OHLCV candles for the --symbols pairs as a `candle` channel, synthesized by the CLI (the Digital X WebSocket API has no candle channel — see the candle note). Comma-separated intervals, the candles endpoint's values only: 1, 5, 15, 30, 60, 240 (minutes), 1D, 1W — e.g. --candles 1,60. Subscribes the trade channel internally to fold from; raw trade events are emitted only if you also pass --trades. Cannot be combined with --no-backfill (candles are seeded and gap-healed from REST)"},
			{Flag: "candle-history", API: "candleHistory", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(5000), Desc: "with --candles, emit up to N closed candles (final:true, oldest first) per symbol×interval before live streaming, fetched from REST — enough history to warm up technical indicators before acting on live bars"},
			{Flag: "my-orders", API: "myOrders", Kind: cmdmeta.KindFlag, Desc: "stream your order updates (private) for the --symbols pairs"},
			{Flag: "my-trades", API: "myTrades", Kind: cmdmeta.KindFlag, Desc: "stream your fills (private) for the --symbols pairs"},
			{Flag: "my-assets", API: "myAssets", Kind: cmdmeta.KindFlag, Desc: "stream your balance changes (private, account-wide — no symbols)"},
			{Flag: "account-seq", API: "accountSeqs", Kind: cmdmeta.KindString, Desc: "comma-separated sub-account sequence numbers the private channels (--my-orders/--my-trades/--my-assets) cover, e.g. 1,2 (default: the key's configured default accountSeq, else 1/main). Every subscribed private channel covers all listed accounts; each order/fill/balance event carries accountSeq so you can tell them apart"},
			{
				Flag: "jq", API: "jq", Kind: cmdmeta.KindString,
				Desc: "filter and/or transform the stream with a jq program (github.com/itchyny/gojq) evaluated over every emitted JSON line — data events ({type:\"data\",channel,symbol,origin,serverTime,source?,accountSeq?,payload}) AND notices ({type:\"notice\",code,level,message,details,time}), exactly like piping to jq. A line is emitted only if the program produces output — use select(...) to filter (guard on .type==\"data\" before reading .payload); any other output replaces the line, so the program can also reshape it (one output line per produced value). Implies --json. Money/quantity fields are decimal strings — compare with (.payload.data.close|tonumber). Stable; needs no --enable-experimental, and cannot be combined with --where/--on/--init",
			},
			// Experimental scripting flags: Experimental:true marks them as the
			// opt-in JS bot runtime, so plain `--help` hides them and
			// `--enable-experimental --help` reveals them tagged. Do NOT write
			// "[experimental]" into a Desc — the bit drives it. (See "Marking a
			// feature experimental" in AGENTS.md.) The runtime gate lives in
			// runMonitor.
			{
				Flag: "where", API: "where", Kind: cmdmeta.KindString, Experimental: true,
				Desc: "JavaScript expression evaluated per data event; only truthy events are emitted. In scope: channel, symbol, origin, serverTime, source, payload (the verbatim event document), rows (its records as an array), ev (all of the above, plus ev.accountSeq — the sub-account of a private event, null otherwise), plus the synchronous ta/ta.stream indicator library. Synchronous only — the async api.*/db.* API is not available here",
			},
			{
				Flag: "on", API: "on", Kind: cmdmeta.KindString, Experimental: true,
				Desc: "JavaScript handler run for each data event that passes --where (every data event if no --where) AND for every stream notice (ev.type is 'data' or 'notice'). May use await; the full api.*/db.* API is available. Handlers run one at a time, in event order; an unhandled error stops the monitor (API rejection: exit 3, otherwise exit 1)",
			},
			{
				Flag: "init", API: "init", Kind: cmdmeta.KindString, Experimental: true,
				Desc: "JavaScript run once before streaming, in the same runtime as --where/--on — define helpers, seed indicator state (e.g. warm a ta.stream indicator from api.candles history), create db tables, cancel stale orders. May use await (then assign shared state via globalThis.x or bare x = ..., not var); no time budget — Ctrl-C interrupts it cleanly",
			},
			{
				Flag: "db", API: "db", Kind: cmdmeta.KindString, Experimental: true,
				Desc: "SQLite file behind the script's async db.exec/db.query/db.get API (default: bot.db under the CLI home; created on first use). The bot owns the schema — strictly separate from the action journal",
			},
			{Flag: "max-concurrency", API: "maxConcurrency", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(64), Experimental: true, Desc: "max api.*/db.* calls in flight at once (default 8) — bounds burst pressure on the API; Promise.all runs up to this many calls concurrently"},
			{Flag: "stateful", API: "stateful", Kind: cmdmeta.KindFlag, Experimental: true, Desc: "maintain a materialized current-state read-model from the same stream and expose it as the synchronous state.* API (open orders, fills, balances, tickers, orderbooks, trades, health) for state-level alerts; without it state.* throws. Requires --where or --on; cannot combine with --jq"},
			{Flag: "max-events", API: "maxEvents", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(1000000000), Desc: "exit 0 after emitting this many data events (with --where/--jq: wait for the Nth emitted match/output)"},
			{Flag: "duration", API: "duration", Kind: cmdmeta.KindDuration, Desc: "stop streaming and exit 0 after this duration, e.g. 90s (units: ms, s, m, h; range 1ms–168h)"},
			{Flag: "no-backfill", API: "noBackfill", Kind: cmdmeta.KindFlag, Desc: "disable REST gap recovery (reconnects then raise BACKFILL_DISABLED and gaps are not patched)"},
			{Flag: "no-reconnect", API: "noReconnect", Kind: cmdmeta.KindFlag, Desc: "exit instead of reconnecting: the first time any connection drops (or the initial connect fails) the session emits its usual DISCONNECTED/CONNECT_FAILED notice and a terminal FATAL notice, then exits non-zero (a network drop exits 1; a rejected handshake exits 3). Without this the session reconnects indefinitely with backoff and REST backfill"},
			{Flag: "stream-log-level", API: "streamLogLevel", Kind: cmdmeta.KindEnum, EnumValues: []string{"debug", "info", "warn", "error", "off"}, Desc: "log level for the stream's reliability NOTICES (debug|info|warn|error|off). INDEPENDENT of --log-level and OFF by default: notices already appear on this command's stdout, so this is the opt-in to ALSO write them to the log (the --log-file file, else stderr), tagged component=stream kind=stream_notice code=<CODE> so they filter cleanly — useful when --jq/--max-events narrows stdout or to keep a persistent reliability trail. A problem and its recovery share a level, so warn shows each onset AND its clear (DISCONNECTED/CONNECT_FAILED, CONNECTION_UNRELIABLE↔CONNECTION_STABLE, DATA_DELAYED↔DATA_CURRENT, DATA_GAP, plus a reconnect CONNECTED) with no routine chatter; info adds the routine lifecycle (a clean first CONNECTED, KEEPALIVE, BACKFILL_START/DONE); error is the unrecoverable subset (FATAL, BACKFILL_FAILED, BACKFILL_NOT_VIABLE); off (the default) logs no notices"},
		},
		Notes: []string{
			"--jq is the stable way to filter and reshape the stream (the JavaScript runtime is the separate experimental path for acting on it). The program runs over every emitted JSON line — BOTH data events and notices — exactly like piping to jq, so guard on .type: select(.type==\"data\" and .channel==\"ticker\" and (.payload.data.close|tonumber) < 140000000) keeps only those data events; select(.type==\"data\") | {sym: .symbol, px: .payload.data.close} also reshapes the output. A program that yields no output drops the event; one that yields several emits several lines. Notices flow through too, so a data-only filter drops them — to keep reliability signals (DATA_GAP, DISCONNECTED, ...) add or .type==\"notice\". Lead with .type==\"data\" before digging into .payload (jq's `and` short-circuits, so a payload lookup never runs on a notice). --max-events counts emitted output lines. Numbers are exact: a filtered (unreshaped) event is re-emitted byte-for-byte, and gojq preserves JSON numbers and decimal strings — precision is lost only where the program itself does arithmetic (tonumber, +, *), so never feed a jq-computed number back into an order. A per-event evaluation error (e.g. tonumber on a non-numeric value, or reading .payload on a notice) skips just that event with a rate-limited stderr warning — one odd frame never stops the stream; an invalid program is rejected at startup. Equivalent to piping `{prog} monitor --json | jq`; --jq needs no external jq and lets --max-events stop after the Nth match.",
			"Streams until stopped (Ctrl-C exits 0; see --max-events/--duration). With --json each event is ONE JSON object per line: {\"type\":\"data\",channel,symbol,origin,serverTime,source?,accountSeq?,payload} for data, {\"type\":\"notice\",code,level,message,details,time} for stream-health notices (CONNECTED, DATA_GAP, BACKFILL_*, KEEPALIVE, ...). Notices are reliability signals; --jq, being a jq program over every line, runs over them too (guard on .type to keep them).",
			"payload is the verbatim source document: a WebSocket frame for origin realtime/snapshot, or REST response data for origin backfill (whose field shapes differ; `source` carries the producing REST path). Money/quantity fields are decimal strings. The one exception is origin \"derived\" (the --candles channel), whose payload the CLI authors itself.",
			"--candles adds a `candle` channel that does NOT exist on the Digital X WebSocket API: the CLI derives it (origin \"derived\") from the deduped, gap-patched public trade stream, seeded and healed from the REST candles endpoint — the payload is a stable CLI-owned shape, {interval,timestamp,open,high,low,close,volume,final}, using the same field names and decimal strings as the candles REST rows. final:false lines UPDATE the still-open bucket (re-emitted as it changes: per trade batch, on seed, and on the quiet-market tick); a final:true line CLOSES the bucket — while connected, a bucket with no trades still closes on time as a flat zero-volume bar (open=high=low=close=previous close), so intervals stay contiguous. Key candles by {interval, timestamp}: the NEWEST line for a key supersedes earlier ones — after a reconnect, DATA_GAP, or gap patch the affected bars are re-fetched from REST and re-emitted corrected, so feed only final:true bars to indicators and recompute if a key repeats. The channel reports its own reliability through the usual notices, carrying channel \"candle\": BACKFILL_START/DONE/FAILED for seeding (a failed seed retries with backoff), and SUBSCRIBE_FAILED — the same notice any dropped channel gets — when the underlying trade subscription is rejected and candles for that symbol end for the session. Pair with --candle-history N to warm up indicators before the live edge. Because these candles are computed client-side, a bar can differ slightly from what the candles REST endpoint later reports for the same bucket (the public trade feed can drop a frame mid-connection without detection, and boundary/empty-bucket handling is the CLI's own) — fine for real-time signals; use the REST endpoint where exact historical values matter.",
			"Private channels (--my-orders/--my-trades/--my-assets) sign the connection with your key and backfill state from REST on every (re)connect, so the first events after CONNECTED are an origin=backfill snapshot of balances/open orders. They cover the key's configured default accountSeq by default, falling back to 1/main; pass --account-seq 1,2 to cover several sub-accounts at once — every private data event then carries accountSeq so you can tell them apart.",
			"Events are not globally ordered across origins (a REST backfill patching a gap arrives after newer live frames) and the same record can repeat (an overlapping backfill row, or a re-sent frame) — reconcile, do not count. Key public trades and fills by tradeId (monotonic per symbol, but NOT contiguous — never assume the next id is +1) and drop duplicates. Key orders by orderId and advance each by its OWN lifecycle, not by serverTime (which is the frame's transport time, or a fetch-start estimate for backfill — not the order's event time): the open statuses are exactly pending/open/partiallyFilled and any other status is terminal and final; for two non-terminal views take the more-advanced of pending<open<partiallyFilled, then the larger cumulative filledQty. Raw WebSocket frames spell the open status `unfilled`.",
			"Public trades are best-effort, not a guaranteed-complete ledger: a gap exposed when a (re)subscribe snapshot's oldest tradeId sits above the last delivered one is patched from REST, and any unrecoverable remainder is flagged with DATA_GAP — but a trade dropped mid-connection (without a reconnect) is indistinguishable from the normal non-contiguous tradeId gaps and is NOT flagged. Private fills (myTrade) are unaffected: the private connection is lossless while up, and fills are deduped by tradeId across live and backfill.",
			"By default the session reconnects forever (jittered backoff + REST backfill), so a drop is a recoverable hiccup, not the end. Pass --no-reconnect to instead exit on the first drop or failed connect: it still emits the usual DISCONNECTED/CONNECT_FAILED notice and a terminal FATAL one (so a --jq/--on consumer sees them), then exits non-zero (network drop: 1, rejected handshake: 3). Use it for a one-shot run where a silent reconnect would hide a problem, or to let a supervising process own the restart.",
			"Stream-layer logs go to the operational logger's destination (the --log-file file, else stderr). The connection mechanics and the materialized state follow --log-level (tagged component=stream and component=stream/state; Debug-level, so silent unless --log-level debug). The reliability NOTICES are separate: they already appear on stdout, so logging them is opt-in via --stream-log-level, which is INDEPENDENT of --log-level and OFF by default. Set it (debug|info|warn|error) to ALSO record notices in the log, tagged component=stream kind=stream_notice code=<CODE> — useful when stdout is narrowed (--jq filters them out, --max-events caps the line count) or to keep a persistent reliability trail. So `grep component=stream` isolates the whole layer and `grep kind=stream_notice` just the notices. A problem and the notice that resolves it share a level, so warn shows each degradation AND its recovery with no routine chatter. It never affects stdout, exit codes, or --max-events accounting.",
		},
		// Notes describing the experimental JS bot runtime go here, NOT in Notes
		// above — plain `--help` hides these and shows them only under
		// --enable-experimental (the catalog and MCP bot_runtime_reference still
		// include them). Keep each note in exactly one bucket.
		ExperimentalNotes: []string{
			"EXPERIMENTAL: the JavaScript bot runtime (--where/--on/--init, the api.*/db.*/ta API, and the in-scope event bindings) is not yet stable — it may change in future versions or have rough edges. It is OFF by default; pass --enable-experimental (or set DIGITALX_CLI_ENABLE_EXPERIMENTAL=1) to use it. Plain streaming (the channel flags with --jq/--json/--duration/--max-events/--no-backfill) is stable and needs no opt-in. Because the API may change, do NOT save and reuse these scripts or build long-lived bots on them — a script that works today can break when digitalx-cli is updated; treat them as throwaway until the runtime is stable.",
			"Script state persists across events (globals survive), so a ta.stream indicator accumulates naturally: create it in --init (e.g. ema20 = ta.stream.ema(20)) and feed each event in --where/--on. Feed BAR-based indicators (ema, rsi, macd, ...) from --candles final:true bars — their periods are defined over fixed-interval bars, and updating one per ticker tick computes a different (wrong) quantity. Warm-up needs no --init fetch: --candle-history N streams N closed bars through the same hooks before the live edge, so the indicator warms up on its own; the manual route ((await api.candles('btc_krw', {interval:'1', limit:200})).forEach(function(c){ ema20.update(c.close) })) remains for windows deeper than the history flag.",
			"Technical indicators are built in as the synchronous ta global (no await; usable in --where, --on and --init): ta.<name>(...) returns the whole series aligned to the input (leading null until warmed up), and ta.stream.<name>(...) returns a live object whose update(...) feeds one sample/bar and returns the latest value (null until warmed up). Inputs accept numbers or decimal strings; outputs are SIGNALS — never feed one back into an order price/qty/amount. By argument shape: sma,ema,wma,dema,tema,rsi,roc,mom take (values, period), stream (period).update(value); cci,willr,atr,natr,adx take (high, low, close, period), stream (period).update(high, low, close); mfi takes (high, low, close, volume, period); obv takes (close, volume) (cumulative, no warm-up); macd(values, fastPeriod, slowPeriod, signalPeriod) -> {macd, signal, hist}; bbands(values, period, nbDevUp=2, nbDevDn=2, maType=0) -> {upper, middle, lower}; stoch(high, low, close, fastKPeriod, slowKPeriod, slowDPeriod, slowKMAType=0, slowDMAType=0) -> {k, d}; stochrsi(values, period, fastKPeriod, fastDPeriod, fastDMAType=0) -> {k, d}; aroon(high, low, period) -> {down, up}. maType ints: 0=SMA,1=EMA,2=WMA,3=DEMA,4=TEMA,5=TRIMA,6=KAMA,7=MAMA,8=T3.",
			"The api.* API in --init/--on mirrors the CLI commands (api.ticker, api.order.place/get/cancel/open/history, api.balance, api.fills, api.deposit.*, api.withdraw.*, api.krw.*, ...): every method takes one options object keyed on the wire parameter names from `{prog} commands` (the obvious single positional like a symbol may be passed directly) and returns a Promise of the response data. Price/qty/amount values MUST be strings. Failures reject with err.code/err.httpStatus/err.description/err.body.",
			"Reliability is built in, not scripted: reads and idempotent writes (order.cancel, withdraw.cancel, deposit.generate) auto-retry through the standard ladder; api.order.place auto-mints clientOrderId, resends ONLY with that same id on ambiguous failures, resolves DUPLICATE_CLIENT_ORDER_ID by fetching the existing order, and always returns the full fetched order; withdraw.request and krw.deposit.request/krw.withdraw.request are strictly single-shot (no idempotency key exists — any error throws and YOU decide). order.history/fills page transparently (cursorless endpoints) and mark the result array truncated:true when a window cannot be fully covered.",
			"setTimeout/setInterval/clearTimeout/clearInterval are available for --init/--on (heartbeats, periodic repricing). Their callbacks always run later ON THE EVENT LOOP, never inside the synchronous --where filter even if scheduled there, and are serialized with other jobs — so a timer callback may interleave with an awaiting --on handler at its await points (mind shared state).",
			"With --where/--on the signing key is resolved eagerly even for public-only channels, so a bot on public data can still place orders.",
			"--stateful maintains a materialized read-model of CURRENT state, fed by the SAME stream, exposed as the synchronous state.* global (usable in --where/--on/--init like ta; it throws without --stateful). It reconciles the way this command's Notes describe — orders keyed by orderId and advanced by lifecycle, trades/fills deduped by tradeId, open-order snapshots reconciled across reconnects — so you read current state instead of folding events yourself. Methods: state.openOrders(symbol?) -> Order[] (omit symbol for all, newest first); state.order(orderId) -> Order|null; state.fills() -> Fill[] (newest first); state.balances() -> Balance[]; state.ticker(symbol) -> Ticker|null; state.orderbook(symbol) -> Orderbook|null; state.trades(symbol) -> Trade[] (newest first); state.health() -> {public,private,backfilling,gapCount,lastDataAt,dataCount}; state.ready(symbol?) -> {ticker,orderbook,trades,openOrders,balances} booleans (false = no data applied yet for that section — still loading after a (re)connect, OR a pair the server has no data for at all: a market that has never traded sends no ticker/trade frame, so those two stay false indefinitely and must not be waited on as if transient; with several --account-seq values, openOrders/balances are true only once EVERY subscribed sub-account has snapshotted); state.notices(n?) -> recent notices (newest first). Account views (openOrders/order/fills/balances) are populated only for the private channels you subscribed (--my-orders/--my-trades/--my-assets). Each order/fill/balance carries accountSeq (the sub-account it belongs to, 1 = main), and balances are kept per account+currency, so a session on more than one sub-account is never collapsed. Money/quantity fields are decimal strings — never feed one back into an order after arithmetic. In --init the store is still empty (no events applied yet), so reads return empty arrays / null. An order that vanished from an open-orders snapshot during a gap shows status \"closed\" (terminal for display) until a history row delivers its real terminal status.",
		},
		Examples: []string{
			"{prog} monitor --symbols btc_krw,eth_krw --ticker",
			"{prog} monitor --symbols btc_krw --candles 1,60",
			`{prog} monitor --symbols btc_krw --candles 1 --candle-history 200 --jq 'select(.type=="data" and .channel=="candle" and .payload.final)' --stream-log-level warn`,
			"{prog} monitor --symbols btc_krw --my-orders --my-assets",
			"{prog} monitor --symbols btc_krw --my-orders --my-assets --account-seq 1,2",
			`{prog} monitor --symbols btc_krw --ticker --jq 'select(.type=="data" and (.payload.data.close|tonumber) < 140000000)' --stream-log-level warn`,
			`{prog} monitor --symbols btc_krw --trades --jq 'select(.type=="data" and any(.payload.data[]; (.qty|tonumber) > 0.5))' --max-events 1 --stream-log-level warn`,
			`{prog} monitor --symbols btc_krw --ticker --jq 'select(.type=="data") | {sym: .symbol, px: .payload.data.close, ts: .serverTime}' --stream-log-level warn`,
			`{prog} monitor --symbols btc_krw --my-orders --my-assets --no-reconnect`,
		},
		ExperimentalExamples: []string{
			`{prog} monitor --enable-experimental --symbols btc_krw --trades --where 'rows.some(function(r){ return Number(r.qty) > 0.5 })' --max-events 1`,
			`{prog} monitor --enable-experimental --symbols btc_krw --candles 60 --candle-history 100 --init 'ema20 = ta.stream.ema(20)' --where 'channel === "candle" && payload.final && (v = ema20.update(payload.close)) !== null && Number(payload.close) > v * 1.01'`,
			`{prog} monitor --enable-experimental --symbols btc_krw --candles 1 --candle-history 50 --init 'rsi = ta.stream.rsi(14)' --where 'channel === "candle" && payload.final && (v = rsi.update(payload.close)) !== null && v < 30'`,
			`{prog} monitor --enable-experimental --symbols btc_krw --ticker --where 'Number(payload.data.close) < 140000000' --on 'if (ev.type === "data") { var o = await api.order.place({symbol:"btc_krw", side:"buy", orderType:"limit", price:"139000000", qty:"0.001"}); console.log(o.orderId) }' --max-events 1`,
			`{prog} monitor --enable-experimental --symbols btc_krw --my-orders --on 'if (ev.type === "notice" && payload.code === "DATA_GAP") { var open = await api.order.open({symbol:"btc_krw"}); console.log("open orders:", open.length) }'`,
			`{prog} monitor --enable-experimental --symbols btc_krw --my-orders --stateful --on 'if (state.openOrders().length > 10) console.error("ALERT:", state.openOrders().length, "open orders")'`,
		},
	},
	{
		ID: []string{"tui"}, Section: cmdmeta.SectionMarket,
		Summary: "demo interactive trading terminal (full-screen): live market data, open orders, fills, and balances over WebSocket",
		Params: []cmdmeta.Param{
			{Flag: "symbols", API: "symbols", Kind: cmdmeta.KindSymbols, Desc: "comma-separated trading pairs to watch, e.g. btc_krw,eth_krw (default: every currently launched pair)"},
			{Flag: "public", API: "public", Kind: cmdmeta.KindFlag, Desc: "market data only: skip the account channels and order entry (no key required)"},
			{Flag: "account-seq", API: "accountSeqs", Kind: cmdmeta.KindString, Desc: "sub-account(s) to subscribe: a single value, a comma-separated list (e.g. 1,2,3 — the account channels cover all of them and the terminal starts on the FIRST listed), or omit it to subscribe every account the key may access, starting on the key's configured default accountSeq (else 1/main). The panels show one active sub-account at a time; switch it in-session with '@' (or click the header's accountSeq: chip)"},
		},
		Notes: []string{
			"DEMO — this terminal showcases the CLI's real-time and trading surface; it is NOT a production trading interface. Do not use it for serious or high-stakes trading. For real trading, drive the scriptable commands (`order place`/`order cancel`/`monitor`) or your own tooling.",
			"Interactive and full-screen — needs a real terminal (TTY). It is built for humans; agents should use `{prog} monitor` (--json is rejected here, except with --dry-run).",
			"Symbols: pass --symbols btc_krw,eth_krw to watch specific pairs; omit it to watch every currently launched pair (from /v2/currencyPairs).",
			"Without --public the account channels (myOrder/myTrade/myAsset) are signed with your key and backfilled from REST on every (re)connect, so orders/fills/balances stay correct across network drops; reliability signals (reconnects, backfill, DATA_GAP) appear in the status bar and the `n` notices view.",
			"Sub-accounts: --account-seq picks which sub-accounts to subscribe — a single value, a comma-separated list (1,2,3, active on the first), or omitted to subscribe every account the key may access (active on the key's configured default, else 1/main; a default the key is not allowed to use is refused before launch). The account panels show one active sub-account at a time — shown as an `accountSeq:N` chip in the header — and `@` (or a click on that chip) opens a switcher to change it; the account channels stay subscribed to every listed sub-account underneath. It is private-only — passing it with --public is an error (public market data has no sub-account).",
			"Order entry (available in private mode): `b`/`s` open the docked order panel (limit/market; the fields follow the order-sizing matrix, the price is picked from the live orderbook or stepped on the tick grid, and an in-place review with the pre-place market checks precedes every send), `:` opens a one-line command bar (type a terse order such as `b 0.05 @ b-2`, watch it resolve live, review, place), and `t` opens a trade-ladder view (arm a size preset once, walk the book's price ladder, arm at the cursor row, one confirm; resting orders show on the ladder and `x` cancels at the row). All three surfaces share the same pre-place checks and review step. Cancellation is always available: `x` cancels the selected open order after a confirmation, and `X` cancels every order in scope. The open-orders panel shows the active pair by default (snapshotting only it, so launching against many pairs stays fast); `a` toggles between the active pair and all pairs. Placement and cancellation use the same validation and local action journal as `order place` / `order cancel`. Best (BBO) orders are not on the panel — use `order place --type best`.",
			"Deposits & withdrawals: `f` opens the funding screen (crypto and KRW) — currencies with live balances on the left, per-currency deposit/withdraw tabs with the transfer history, deposit address (a button generates one when missing), and withdrawable amount on the right. Requesting a transfer (crypto withdrawal to a registered address, or a KRW deposit/withdrawal push confirmed in the Digital X app) requires a key provisioned with `setup --with-transfers`; viewing, address generation, and canceling a pending withdrawal (`x`) are not gated. Requests run through the same validation and action journal as the `withdraw`/`krw` commands.",
			"Orderbook grouping: `+`/`-` cycle the active pair's orderbook grouping level (the levels come from the tick-size policy; a `grp` chip on the orderbook/ladder title shows the active level, and resting orders on the ladder aggregate into their bucket row).",
			"Resizable panels: drag a border between panels with the mouse to resize them; `=` resets the layout. (Mouse capture takes over the terminal's text selection while running.)",
			"Quit with q or Ctrl-C (a deliberate stop exits 0).",
		},
		Examples: []string{
			"{prog} tui",
			"{prog} tui --symbols btc_krw,eth_krw",
			"{prog} tui --symbols btc_krw --public",
			"{prog} tui --account-seq 2",
			"{prog} tui --account-seq 1,2,3",
		},
	},
	{
		ID: []string{"setup"}, Section: cmdmeta.SectionKeys,
		Summary: "set up an API key: generate an ED25519 keypair, guide you through registering it in the developers portal, then bind and health-check the issued key",
		Params: []cmdmeta.Param{
			{Flag: "name", API: "name", Kind: cmdmeta.KindString, Desc: `name for the new key (default: "default")`},
			{
				Flag: "api-key", API: "apiKey", Kind: cmdmeta.KindString,
				Desc: "finish setup for an already-generated key: bind this portal-issued API key id (re-run `setup --api-key <KEY_ID>` after you have registered the key's public key)",
			},
			keystoreFlag,
			withTransfersFlag,
			{
				Flag: "no-interactive", API: "noInteractive", Kind: cmdmeta.KindFlag,
				Desc: "skip the interactive prompt even on a terminal: print the registration link and exit (the default when output is piped or --json is set)",
			},
			{
				Flag: "wait", API: "wait", Kind: cmdmeta.KindFlag,
				Desc: "print the registration link, then keep running and finish setup automatically once you register the key — no prompt and no need to paste the issued id back (for scripts/agents; works without a terminal). Cannot be combined with --api-key",
			},
			{
				Flag: "wait-timeout", API: "waitTimeout", Kind: cmdmeta.KindDuration,
				Desc: "how long --wait keeps waiting before giving up and printing the resume instructions (default 20m; 0 waits indefinitely)",
			},
		},
		Notes: []string{
			"On a terminal, setup is interactive: it shows the registration link, waits for you to paste the issued key id, verifies the id with a signed request before binding it (an id the server rejects is never stored), then runs a health check — all in one session. --json and --compact imply --no-interactive (so does piping the output): setup prints the link and the result document and exits, to be finished later with `{prog} setup --api-key <KEY_ID>` (or with --wait, which finishes on its own).",
			"--wait is the hands-off form: it keeps running and binds + health-checks the key automatically as soon as you finish registering it — you never copy the issued id back. It needs no terminal. An agent typically runs a plain `{prog} setup` first (it returns the registration link immediately, to relay), then `{prog} setup --wait` to finish on its own — use the same --name for both, since a different name generates a different keypair. Run standalone with --json, --wait writes the registration-link document to stdout up front (so the link is relayed before it blocks), then the final result document when registration completes — the last document is the outcome. It gives up after --wait-timeout (default 20m) and prints how to resume.",
			"The private key never leaves this machine — it is stored in the key's keystore backend (--keystore, default per `{prog} keystore default`) and only used to sign requests.",
			"After registering the public key at https://developers.digitalx.miraeasset.com, bind the issued key id: `{prog} key bind <name> --api-key <KEY_ID>`, or re-run `{prog} setup --name <name> --api-key <KEY_ID>` to bind and health-check in one step.",
			"Once a key is bound, setup runs a complementary `doctor` health check for it; the result is advisory — a problem is reported as a warning and never fails setup.",
			"Grant the key only the permissions your bot needs (a trading bot needs read + order permissions, not withdrawal) and set an IP allowlist. Add --with-transfers only if you intend to move funds via the deposit/withdrawal commands.",
			"--base-url and --ws-base-url are global flags, but setup treats them specially: when it creates the key it pins them onto the key — every later call for that key then uses that host (--ws-base-url is derived from --base-url when omitted; the DIGITALX_CLI_BASE_URL / DIGITALX_CLI_WS_BASE_URL env vars pin the key the same way). Rarely needed (only if your account is on a non-default Digital X host), and applied only at creation, never to an existing key; change it later with `{prog} key set-base-url`.",
		},
		Examples: []string{"{prog} setup", "{prog} setup --name trading-bot", "{prog} setup --name trading-bot --api-key <KEY_ID>", "{prog} setup --wait"},
	},
	{
		ID: []string{"doctor"}, Section: cmdmeta.SectionKeys,
		Summary: "check that your key setup can reach and trade on Digital X, and explain any fix",
		Params: []cmdmeta.Param{
			{
				Flag: "diagnose-clock", API: "diagnoseClock", Kind: cmdmeta.KindFlag,
				Desc: "when investigating a clock skew, additionally diagnose this machine's OS time-sync configuration, naming the exact commands to fix it. On Windows: the W32Time service state and start type, its configured NTP source, and the live sync status. On Linux: the systemd time-sync state via `timedatectl`. Read-only and opt-in — the default run never inspects OS services; the diagnosis needs no administrator rights (its fixes do). macOS reports it isn't supported yet.",
			},
		},
		Notes: []string{
			"Read-only health check: inspects your keys, runs `whoami` for the selected key (production unless --base-url), warns if the effective default accountSeq is not in the key's allowedAccountSeqs, reports the public IP for the allowlist, checks clock skew, and dials the WebSocket endpoint `monitor` would use. Pass --key to check a specific key instead of the default.",
			"Reports the REST and WebSocket endpoints it targeted, so a check run against the sandbox or a per-key host is never mistaken for production.",
			"Pass --diagnose-clock to additionally inspect the operating system's own time-sync configuration (opt-in, read-only) when the clock-skew check warns and you need to know why the clock drifted. Implemented for Windows and Linux; macOS reports it isn't supported yet.",
			"Exit code 0 when healthy (warnings allowed), 4 when a key/config problem blocks trading, 1 when only a network failure prevented verification — so `{prog} doctor && ...` can gate a session.",
		},
		Examples: []string{"{prog} doctor", "{prog} doctor --key trading-bot", "{prog} doctor --diagnose-clock"},
	},
	{
		ID: []string{"ip"}, Section: cmdmeta.SectionKeys,
		Summary: "show the public IP(s) Digital X sees you from, for the API-key IP allowlist",
		Notes: []string{
			"Queries the production API (api.digitalx.miraeasset.com) over both IPv4 and IPv6 (whichever your host has) and prints the source IP it sees for each — `setup` runs this for you. Always uses production, ignoring --base-url.",
			"Register the IPv6 /64 prefix (not the full /128) so the allowlist keeps matching as your address rotates within the prefix; for IPv4 register the full address (a /32).",
			"Signed requests must originate from an allowlisted IP — requests from other IPs are rejected.",
		},
		Examples: []string{"{prog} ip"},
	},
	{
		ID: []string{"key", "add"}, Section: cmdmeta.SectionKeys,
		Summary:     "add a named key (ED25519 by default: generates a keypair and prints the public key)",
		Positionals: []cmdmeta.Positional{{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"}},
		Params: []cmdmeta.Param{
			{
				Flag: "type", API: "type", Kind: cmdmeta.KindEnum, EnumValues: []string{"ed25519", "hmac-sha256"},
				Desc: "signing scheme: ed25519 (default) or hmac-sha256",
			},
			{
				Flag: "from-pem-file", API: "fromPemFile", Kind: cmdmeta.KindString,
				Desc: "import an existing ED25519 private key (PKCS#8 PEM file) instead of generating one",
			},
			{
				Flag: "secret-file", API: "secretFile", Kind: cmdmeta.KindString,
				Desc: "for --type hmac-sha256: file holding the shared secret ('-' reads stdin)",
			},
			{
				Flag: "api-key", API: "apiKey", Kind: cmdmeta.KindString,
				Desc: "bind this portal-issued API key id immediately, skipping the separate `key bind` step (for importing an already-registered key — pair with --from-pem-file; required for --type hmac-sha256)",
			},
			keystoreFlag,
			withTransfersFlag,
		},
		Notes: []string{
			"The first key you add becomes the default automatically.",
			"Each key picks its own keystore backend at creation (--keystore) — e.g. an order-placing key in the OS keychain while read-only keys stay in the file keystore. Move it later with `{prog} keystore migrate`.",
			"--api-key binds the key id at creation so you skip `key bind` — use it when importing a key whose public key is already registered (its newly generated public key is NOT registered yet, so on a generated key you would still register and re-bind).",
			"--type hmac-sha256 stores a Digital X-issued shared secret (no keypair, no public key); it requires --api-key and --secret-file.",
			"Pin the key to a non-default API host with --base-url (and --ws-base-url, else derived from it) — rarely needed, only if your account is on a different Digital X host; change it later with `{prog} key set-base-url`.",
		},
		Examples: []string{"{prog} key add trading-bot", "{prog} key add hot-wallet --keystore keychain", "{prog} key add imported --from-pem-file ./ed25519.pem --api-key <KEY_ID>", "{prog} key add hmac-bot --type hmac-sha256 --api-key <KEY_ID> --secret-file ./secret.txt"},
	},
	{
		ID: []string{"key", "bind"}, Section: cmdmeta.SectionKeys,
		Summary:     "attach the portal-issued API key id (X-KAPI-KEY) to a named ED25519 key",
		Positionals: []cmdmeta.Positional{{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"}},
		Params: []cmdmeta.Param{
			{Flag: "api-key", API: "apiKey", Kind: cmdmeta.KindString, Required: true, Desc: "API key id issued by the developers portal for this ED25519 public key (this is an id, not a secret)"},
		},
		Notes:    []string{"HMAC-SHA256 keys are issued as an api-key id + shared-secret pair and are added already bound with `{prog} key add --type hmac-sha256`; use remove/re-add to replace them."},
		Examples: []string{"{prog} key bind trading-bot --api-key <KEY_ID>"},
	},
	{
		ID: []string{"key", "list"}, Section: cmdmeta.SectionKeys,
		Summary:  "list keys (names, binding state, default) — never prints private keys",
		Examples: []string{"{prog} key list"},
	},
	{
		ID: []string{"key", "show"}, Section: cmdmeta.SectionKeys,
		Summary:     "show one key's metadata and public key",
		Positionals: []cmdmeta.Positional{{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"}},
		Examples:    []string{"{prog} key show trading-bot"},
	},
	{
		ID: []string{"key", "use"}, Section: cmdmeta.SectionKeys,
		Summary:     "set the default key used when --key is not passed",
		Positionals: []cmdmeta.Positional{{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"}},
		Notes:       []string{"Keys may belong to different Digital X accounts — every private endpoint command prints `signing as key ...` on stderr so you always know which account acted."},
		Examples:    []string{"{prog} key use trading-bot"},
	},
	{
		ID: []string{"key", "rename"}, Section: cmdmeta.SectionKeys,
		Summary: "rename a key, keeping its keypair and binding (no re-registration)",
		Positionals: []cmdmeta.Positional{
			{Name: "old-name", API: "oldName", Kind: cmdmeta.KindString, Required: true, Desc: "current key name"},
			{Name: "new-name", API: "newName", Kind: cmdmeta.KindString, Required: true, Desc: "new key name"},
		},
		Notes: []string{
			"Only the label changes: the keypair, the API key id binding, the keystore backend, and any per-key base URL are preserved — so there is nothing to re-register or re-bind (unlike remove + re-add, which destroys the keypair).",
			"If you rename the default key the default follows it (it is the same key). A key whose keystore backend is unavailable here can't be renamed — discard it with `{prog} key remove --force` instead.",
		},
		Examples: []string{"{prog} key rename trading-bot main-bot"},
	},
	{
		ID: []string{"key", "remove"}, Section: cmdmeta.SectionKeys,
		Summary:     "remove a key and destroy its private key material",
		Positionals: []cmdmeta.Positional{{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"}},
		Params: []cmdmeta.Param{
			{Flag: "force", API: "force", Kind: cmdmeta.KindFlag, Desc: "remove the key's record even when its secret cannot be deleted (its keystore backend is unknown to this build or unavailable) — the secret, if any, is left behind in that backend"},
		},
		Notes: []string{
			"If you remove the default key the default becomes unset — it is never silently re-pointed at another (possibly different-account) key.",
			"Without --force, a key whose backend this build doesn't know or can't reach cannot be removed (its secret would be orphaned); --force removes the record anyway and reports where the secret was left.",
		},
		Examples: []string{"{prog} key remove old-bot", "{prog} key remove stranded-key --force"},
	},
	{
		ID: []string{"key", "set-base-url"}, Section: cmdmeta.SectionKeys,
		Summary: "pin a key to a specific API base URL (rarely needed)",
		Positionals: []cmdmeta.Positional{
			{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"},
			{Name: "url", API: "url", Kind: cmdmeta.KindString, Desc: "API base URL to use for this key (omit and pass --clear to revert to the default)"},
		},
		Params: []cmdmeta.Param{
			{Flag: "clear", API: "clear", Kind: cmdmeta.KindFlag, Desc: "remove the per-key base URL, reverting this key to the default endpoint"},
			{Flag: "no-verify", API: "noVerify", Kind: cmdmeta.KindFlag, Desc: "skip the post-set reachability check of the REST and WebSocket endpoints"},
		},
		Notes: []string{
			"Most users never need this — the default endpoint is correct unless your account has been provisioned on a different Digital X API host. The command line (--base-url) and the DIGITALX_CLI_BASE_URL env var still override a key's stored URL.",
			"This also stores the matching WebSocket base URL (used by `monitor`): pass --ws-base-url to set it explicitly, otherwise it is derived from the API host (e.g. https://api-test.digitalx.miraeasset.com → wss://ws-api-test.digitalx.miraeasset.com). --clear removes both.",
			"After storing the URLs it smoke-tests both endpoints (an unauthenticated request to the REST host and a public dial to the WebSocket host) and reports reachability; the URLs are saved regardless, and if the WebSocket host is unreachable it shows the command to pin a correct one. Pass --no-verify to skip the check.",
		},
		Examples: []string{
			"{prog} key set-base-url trading-bot https://api.digitalx.miraeasset.com",
			"{prog} key set-base-url trading-bot https://api-test.digitalx.miraeasset.com --ws-base-url wss://ws-api-test.digitalx.miraeasset.com",
			"{prog} key set-base-url trading-bot --clear",
		},
	},
	{
		ID: []string{"key", "set-default-account-seq"}, Section: cmdmeta.SectionKeys,
		Summary: "set the default sub-account sequence number for a key",
		Positionals: []cmdmeta.Positional{
			{Name: "name", API: "name", Kind: cmdmeta.KindString, Required: true, Desc: "key name"},
			{Name: "accountSeq", API: "accountSeq", Kind: cmdmeta.KindInt, Desc: "sub-account sequence number (>= 1; omit with --clear)"},
		},
		Params: []cmdmeta.Param{
			{Flag: "clear", API: "clear", Kind: cmdmeta.KindFlag, Desc: "remove the per-key default, reverting to the main account (1)"},
		},
		Notes: []string{
			"When set, every command using this key defaults to the configured sub-account instead of 1 (the main account). An explicit --account-seq still overrides it.",
		},
		Examples: []string{
			"{prog} key set-default-account-seq trading-bot 2",
			"{prog} key set-default-account-seq trading-bot --clear",
		},
	},
	{
		ID: []string{"keystore", "status"}, Section: cmdmeta.SectionKeys,
		Summary: "show each backend's availability and key count, and the default for new keys",
		Notes: []string{
			"Every key picks its own keystore backend (recorded in keys.json; see `{prog} key list`). `file` (an encrypted file under the CLI home) needs no UI and works on headless hosts; `keychain` (the OS-native keyring: macOS Keychain, Windows Credential Manager, Linux Secret Service) is the more secure option where the OS keyring is reachable.",
			"Availability is probed read-only — running this never triggers an OS keychain prompt.",
		},
		Examples: []string{"{prog} keystore status"},
	},
	{
		ID: []string{"keystore", "migrate"}, Section: cmdmeta.SectionKeys,
		Summary: "move keys' private material to another keystore backend",
		Positionals: []cmdmeta.Positional{
			{Name: "backend", API: "backend", Kind: cmdmeta.KindString, Required: true, Desc: "target backend: file | keychain"},
			{Name: "key", API: "key", Kind: cmdmeta.KindString, Variadic: true, Desc: "key name(s) to migrate (omit only with --all)"},
		},
		Params: []cmdmeta.Param{
			{Flag: "all", API: "all", Kind: cmdmeta.KindFlag, Desc: "migrate every registered key instead of naming them"},
			{Flag: "keep-source", API: "keepSource", Kind: cmdmeta.KindFlag, Desc: "leave each key in its old backend instead of deleting it once it is verified in the new one"},
		},
		Notes: []string{
			"Per key: copies the private material from the backend the key's record names into the target, verifies it reads back correctly, re-points the record (keys.json), and only then removes the original. Keys already in the target are skipped, so re-running after a partial failure simply continues.",
			"Safe by construction: nothing is deleted until the copy is verified, and a failure leaves the in-flight key exactly as it was (keys finished earlier stay migrated — each key is individually consistent).",
			"Migrating does NOT change where new keys go — that is `{prog} keystore default`.",
		},
		Examples: []string{"{prog} keystore migrate keychain trading-bot", "{prog} keystore migrate file --all --keep-source"},
	},
	{
		ID: []string{"keystore", "default"}, Section: cmdmeta.SectionKeys,
		Summary: "set which keystore backend newly created keys go to",
		Positionals: []cmdmeta.Positional{
			{Name: "backend", API: "backend", Kind: cmdmeta.KindString, Required: true, Desc: "backend for new keys: file | keychain"},
		},
		Notes: []string{
			"Applies only to keys created afterwards (`setup` / `key add` without --keystore). Existing keys keep their own backend — move them with `{prog} keystore migrate`.",
		},
		Examples: []string{"{prog} keystore default keychain"},
	},
	{
		ID: []string{"commands"}, Section: cmdmeta.SectionMeta,
		Summary:  "print a machine-readable JSON catalog of every command, flag, enum, and exit code",
		Examples: []string{"{prog} commands"},
	},
	{
		ID: []string{"license"}, Section: cmdmeta.SectionMeta,
		Summary: "show the copyright, open-source license, and where to read the full terms",
		Notes: []string{
			"digitalx-cli is under the Apache License 2.0; this points at the full text and terms rather than reproducing them. The Digital X API Sandbox is separately licensed — see `{prog} sandbox license`.",
			"The binary links open-source Go modules; their notices ship in each release archive (THIRD_PARTY_LICENSES.txt) and are linked from this output.",
		},
		Examples: []string{"{prog} license"},
	},
	{
		ID: []string{"agent", "skill", "install"}, Section: cmdmeta.SectionMeta,
		Summary: "install the bundled Digital X Agent Skill into your AI agent's skills directory",
		Params: []cmdmeta.Param{
			{Flag: "all", API: "all", Kind: cmdmeta.KindFlag, Desc: "install for every supported agent (Claude and Codex)"},
			{Flag: "claude", API: "claude", Kind: cmdmeta.KindFlag, Desc: "install into Claude's skills directory (~/.claude/skills — used by Claude Code in the terminal, the IDE extension, and the desktop app's Code tab)"},
			{Flag: "codex", API: "codex", Kind: cmdmeta.KindFlag, Desc: "install into Codex's user skills directory (~/.agents/skills — used by the Codex CLI, IDE extension, and Codex app)"},
			{Flag: "project", API: "project", Kind: cmdmeta.KindFlag, Desc: "install into the current directory's project skills dir (e.g. ./.claude/skills or ./.agents/skills) for repo-scoped use instead of your user-wide one"},
		},
		Notes: []string{
			"The skill is bundled inside this binary, so this writes it straight to disk — nothing is downloaded. It teaches an agent to drive this CLI (and its `mcp serve` tools) safely; the agent loads it automatically when a task involves Digital X.",
			"Pick the agent(s) with --claude / --codex (or --all). Re-running is safe and idempotent: it refreshes the skill to match this binary and removes any stale files an older version left behind, so upgrade the skill by upgrading the CLI and re-running this.",
			"This assumes `" + agentskill.SkillBinary + "` is already on your PATH (the skill shells out to it). Verify the whole setup with `{prog} agent skill doctor`.",
		},
		Examples: []string{"{prog} agent skill install --all", "{prog} agent skill install --claude", "{prog} agent skill install --codex --project"},
	},
	{
		ID: []string{"agent", "skill", "doctor"}, Section: cmdmeta.SectionMeta,
		Summary: "check the bundled Agent Skill install and that `" + agentskill.SkillBinary + "` is reachable on PATH",
		Notes: []string{
			"Reports, for each supported agent: whether the skill is installed, whether the installed copy matches this binary's bundled skill (stale copies are flagged), and whether `" + agentskill.SkillBinary + "` resolves on PATH (the skill shells out to it). Each problem carries the exact fix.",
			"Checks your user-wide installs (~/.claude/skills, ~/.agents/skills); it does not inspect a `--project` (repo-scoped) install.",
			"Exit code 0 when everything detected is fine, 4 when there's something to fix (a stale skill, or a skill installed for a local agent while `" + agentskill.SkillBinary + "` isn't on PATH).",
		},
		Examples: []string{"{prog} agent skill doctor"},
	},
	{
		// Hidden: run only by the managed install script (`curl … | sh`), never an
		// agent operation. It copies the running binary to the stable PATH location,
		// wires PATH, and writes the manifest — reconciling, so re-running the
		// installer repairs a broken install.
		ID: []string{"self", "install"}, Section: cmdmeta.SectionMeta, Hidden: true,
		Summary: "install the running binary onto PATH and write the manifest (run by the install script)",
	},
	{
		ID: []string{"self", "update"}, Section: cmdmeta.SectionMeta,
		Summary: "update this CLI to the latest release (managed installs only)",
		Params: []cmdmeta.Param{
			{Flag: "tag", API: "tag", Kind: cmdmeta.KindString, Desc: "update to this exact release tag (e.g. v1.2.3) instead of the latest — for pinning or downgrading"},
		},
		Notes: []string{
			"Only works for an install created by the managed install script (the `curl … | sh` one-liner). A Homebrew / `go install` / hand-downloaded / dev build is refused with the right way to upgrade it instead.",
			"Resolves the latest release, downloads the archive for your platform, verifies its SHA-256 against the release checksums, then replaces the binary in place. Pass --dry-run to only check whether a newer version exists. Trust is TLS + SHA-256.",
			"It swaps a binary and nothing else. No command in this CLI moves or renames your CLI home, your artifact cache, or a file inside them. Upgrading from an earlier release? See MIGRATION.md in the repository.",
		},
		Examples: []string{"{prog} self update", "{prog} self update --dry-run", "{prog} self update --tag v1.2.3"},
	},
	{
		ID: []string{"self", "uninstall"}, Section: cmdmeta.SectionMeta,
		Summary: "remove a managed install (interactive)",
		Notes: []string{
			"Interactive only: it asks what to remove and needs a terminal. It refuses to run piped, redirected, from an agent, or with --json/--compact.",
			"It lists each item's files, then asks, then removes. You choose, piece by piece: the installed binary + manifest; your config, API keys and action journal (removing keys is irreversible and also clears keychain-stored keys); the regenerable sandbox caches (the state dir + the shared Deno/module cache, stopping a running sandbox first); and the installer's PATH change.",
			"Undoing the PATH change edits your shell startup files, it never deletes them: for each file that carries the digitalx-cli block it shows a diff and removes only that block, leaving the rest untouched (on Windows it removes only the digitalx-cli entry from your User PATH). A file you decline is reported so you can edit it yourself. On Windows the running binary is locked and cannot delete itself; it is reported for you to delete manually.",
		},
		Examples: []string{"{prog} self uninstall"},
	},
	{
		ID: []string{"self", "doctor"}, Section: cmdmeta.SectionMeta,
		Summary: "check the install's health",
		Notes: []string{
			"Read-only: reports whether this is a managed install, whether the installed binary and the alias beside it match what was recorded, whether the install dir is on PATH, and whether a CLI home holds data this install is not reading — each problem naming its fix. Distinct from `{prog} doctor`, which checks your key/API health. Exit code 0 when healthy, 4 when there's something to fix.",
		},
		Examples: []string{"{prog} self doctor"},
	},
	{
		ID: []string{"mcp", "serve"}, Section: cmdmeta.SectionMeta,
		Summary: "run a Model Context Protocol (MCP) stdio server exposing the Digital X API as tools",
		Params: []cmdmeta.Param{
			{Flag: "read-only", API: "readOnly", Kind: cmdmeta.KindFlag, Desc: "expose only read tools (GET endpoints); omit all order/withdrawal/transfer write tools"},
			{Flag: "multi-key", API: "multiKey", Kind: cmdmeta.KindFlag, Desc: "let one server span keys: add an optional `key` argument (a configured key name) to every authenticated tool, defaulting to the launch --key. Off by default — prefer one server per key. Passing the wrong key targets the wrong Digital X account"},
		},
		Notes: []string{
			"Speaks MCP over stdio (stdin/stdout); meant to be launched as a subprocess by an MCP host (Claude Desktop, Claude Code, ...). Runs until the client disconnects or Ctrl-C/SIGTERM (exit 0). stdout carries only the JSON-RPC protocol — all diagnostics go to stderr.",
			"Every Digital X REST endpoint command becomes a tool named like its command key with spaces as underscores (order place -> order_place, krw deposit history -> krw_deposit_history); most local builtins (key management, logs, sandbox, monitor) are not tools. Tool input is one JSON object keyed on the wire parameter names from `{prog} commands`; money/quantity values stay decimal strings. A read-only list_keys tool reports the configured keys.",
			"A read-only digitalx_guide tool returns the bundled Agent Skill's workflow guidance — the safety rules (dry-run-first placement, idempotency, decimal-string money) and the recommended workflow for each task. Call it with no topic for the overview and task router, or a topic for a focused playbook (monitoring, funding, debugging, ...). It is how an MCP-only host reaches the guidance Claude Code would get from the installed skill; consult it before placing an order or driving an unfamiliar flow.",
			"For setup without a terminal, two tools are exposed in every mode (including --read-only): `setup` (generates a local keypair and returns a registration link to open + confirm with MFA; call it again with the issued id as `apiKey` to bind the key) and `doctor` (runs read-only signed diagnostics to verify the key works). `setup` only changes the local keystore, and additionally runs the complementary `doctor` once a key ends up configured (bound or supplied inline). `doctor` (and that check) may sign read-only diagnostics that call Digital X, but never moves money, so a keyless first-timer can get set up entirely through the MCP host. A key bound this way (setup, then setup with `apiKey`) is usable by the tools immediately, with no server restart — unless the server was launched with inline env credentials or an explicit --key whose name you did not set up.",
			"By default ONE server signs with ONE key, resolved at launch from --key / DIGITALX_CLI_KEY (and the usual --base-url/--timeout/--retry-timeout globals). Signed calls self-correct reactively on a clock-drift rejection (the --time-sync auto default); pass --time-sync on to measure the server clock at startup instead, or off to disable correction. To use more than one Digital X account, register multiple named MCP servers, each with its own --key. With --multi-key a single server instead accepts an optional `key` on each authenticated tool (default = launch key). --read-only and --multi-key also honor truthy DIGITALX_CLI_MCP_READ_ONLY / DIGITALX_CLI_MCP_MULTI_KEY env vars.",
			"Write tools (order place/cancel, withdraw *, krw deposit/withdraw request) are exposed and carry destructive/non-idempotent annotations so the host can confirm; the same out-of-band gates as the CLI still apply (pre-registered withdrawal addresses, in-app KRW confirmation, the key's permission bits). Use --read-only to drop every write tool. Key/config failures point you at `{prog} doctor`.",
		},
		Examples: []string{
			"{prog} mcp serve",
			"{prog} mcp serve --key trading",
			"{prog} mcp serve --read-only",
			"{prog} mcp serve --key trading --multi-key",
		},
	},
	{
		ID: []string{"logs"}, Section: cmdmeta.SectionMeta,
		Summary: "show recent API calls and orders this CLI recorded locally",
		Params: []cmdmeta.Param{
			{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(1000), Desc: "max rows to show (default 20)"},
			{Flag: "orders", API: "orders", Kind: cmdmeta.KindFlag, Desc: "show recorded orders instead of raw API calls"},
			{Flag: "operations", API: "operations", Kind: cmdmeta.KindFlag, Desc: "show recorded operations (the call groups) instead of raw API calls"},
		},
		Notes: []string{
			"Reads the local action journal (the SQLite file under the CLI home) — the automatic record of the write calls this CLI made (reads are recorded only under --debug). Nothing is sent to Digital X; the data never left your machine.",
			"Built for agent self-inspection: when asked what it did, an agent can read this back instead of guessing. Set DIGITALX_CLI_NO_JOURNAL=1 to disable journaling entirely.",
		},
		Examples: []string{"{prog} logs", "{prog} logs --orders --limit 50"},
	},
	{
		ID: []string{"debug", "bundle"}, Section: cmdmeta.SectionMeta,
		Summary: "write a redacted diagnostic bundle (config, key metadata, recent activity) to send to support",
		Params: []cmdmeta.Param{
			{Flag: "limit", API: "limit", Kind: cmdmeta.KindInt, Min: intPtr(1), Max: intPtr(100000), Desc: "max recent calls/orders to include (default 200)"},
			{Flag: "out", API: "out", Kind: cmdmeta.KindString, Desc: "output file path (default: a timestamped file under the CLI home)"},
		},
		Notes: []string{
			"Contains no secrets: keys are listed by name/binding only (private keys are never read), and the request params/api-key-ids are the same non-secret values already in the journal.",
			"Send the resulting file to Digital X support when reporting a problem.",
		},
		Examples: []string{"{prog} debug bundle", "{prog} debug bundle --out ./dgx-cli-debug.json"},
	},
	{
		ID: []string{"sandbox", "start"}, Section: cmdmeta.SectionMeta,
		Summary: "run and connect to a local Digital X API Sandbox in one step",
		Params: []cmdmeta.Param{
			{Flag: "port", API: "port", Kind: cmdmeta.KindInt, Min: intPtr(0), Max: intPtr(65535), Desc: "port to bind (default 9999; on a collision the CLI retries on an OS-assigned free port). A non-zero value forces that exact port with no fallback"},
			{Flag: "db", API: "db", Kind: cmdmeta.KindString, Desc: "sandbox database path (default: a file under the CLI home's sandbox/ dir)"},
			{Flag: "key-name", API: "keyName", Kind: cmdmeta.KindString, Desc: `name for the imported seeded key (default "sandbox"); must contain "sandbox" so its use stays explicit on the command line`},
			{Flag: "runtime", API: "runtime", Kind: cmdmeta.KindEnum, EnumValues: []string{"deno", "managed-deno"}, Desc: "which Deno runs the sandbox: managed-deno (a pinned Deno the CLI downloads and SHA-256-verifies) or deno (a `deno` already on your PATH). Default: managed-deno, falling back to a system `deno` only where no managed-Deno build exists (musl Linux). There is no Node runtime."},
			{Flag: "url", API: "url", Kind: cmdmeta.KindString, Desc: "override the bundle source Deno runs (default: the Official Source). A local path or file:// URL is run from disk"},
			{Flag: "reimport", API: "reimport", Kind: cmdmeta.KindFlag, Desc: "replace the imported key's material even if the key already exists"},
			{Flag: "skip-version-check", API: "skipVersionCheck", Kind: cmdmeta.KindFlag, Desc: "start even if the bundle is older than the minimum this CLI supports (skips the auto-update-and-retry)"},
			{Flag: "paper", API: "paper", Kind: cmdmeta.KindFlag, Desc: "paper trading: initialize a fresh database with the seeded pairs' market data mirrored LIVE from production Digital X (fills stay simulated; needs network). Seeds the bundle's built-in fixture pairs unless --all-pairs is also set. A pair production reports non-launched stays on the simulated market — the result names such pairs in `walkPairs`. On an existing database this only verifies the mode — add --fresh to recreate it in paper mode, or flip pairs with `sandbox exec set-market --symbol <pair> --source live`"},
			{Flag: "all-pairs", API: "allPairs", Kind: cmdmeta.KindFlag, Desc: "seed a fresh database from a live production snapshot so every LAUNCHED currency pair exists (the tradable universe), instead of the bundle's built-in fixture pairs (needs network; the first run fetches a per-pair tick-size policy, so it takes a few seconds longer). The snapshot is cached beside the database, so a repeated start (including --fresh) reseeds from cache in well under a second. Pair with --paper to mirror those pairs LIVE; alone, they are seeded on the simulated market. Affects initialization only — on an existing database add --fresh to reseed"},
			{Flag: "fresh", API: "fresh", Kind: cmdmeta.KindFlag, Desc: "start from a brand-new database: stop any running sandbox for it, DELETE the database (balances, orders, and trades are discarded — sandbox data is disposable), and initialize a new one in the selected mode. The seeded keys are deterministic, so the imported key stays valid. This is how you switch between --paper and the default simulated market, or reseed with --all-pairs"},
		},
		Notes: []string{
			"Goes from nothing to a signed, working local exchange: resolves a Deno (downloading the managed one on first use), hands it the bundle URL to fetch+run, initializes the database if needed, runs the sandbox as a background server, waits until it answers, and imports the seeded ED25519 key into your keystore. The exact command line — including the bundle URL Deno is handed — is printed before it runs.",
			"--paper enables the sandbox's paper-trading mode: real production prices, order book, and trades are mirrored while the server runs, and your orders fill against them — locally simulated, no real money. By default only the bundle's built-in fixture pairs are seeded; add --all-pairs to seed every LAUNCHED production pair from a live snapshot (the tradable universe) so the sandbox carries that pair set. The result reports `paper: true` when active, with `walkPairs` naming any pairs left on the simulated market (not launched on production); the sandbox's own help (`sandbox exec help`) and `sandbox exec status` carry the full detail (market modes, mirror age, live-feed state).",
			"Surfaces the sandbox's short license notice (its own `license --show-banner`) at the top — the full terms are one `sandbox license` away. Before starting, it checks the bundle is at least the minimum version this CLI supports; an older bundle is auto-updated from the Official Source and retried once (use --skip-version-check to bypass).",
			"The imported key is a SANDBOX key: it is bound to a SANDBOX_… api-key id, pinned to the loopback URL, stored in the file keystore, and never made the default — so a bare command can't accidentally sign against the mock. Use it explicitly with --key sandbox. The global config (config.json) is never touched.",
			"The managed Deno and Deno's bundle cache live in a shared directory (override with DIGITALX_CLI_SANDBOX_CACHE); the database, pidfile, and run.log live under the CLI home (DIGITALX_CLI_HOME). Run parallel sandboxes by giving each its own DIGITALX_CLI_HOME. Tail the log at <home>/sandbox/run.log.",
			"This is a deliberately limited convenience feature (one seeded account, a single loopback server). For advanced setups — custom seed data, multiple accounts/markets, your own flags — download the bundle from the Official Source (https://docs.digitalx.miraeasset.com/digitalx-sandbox.mjs) and run it yourself with Deno or Node.",
			"The sandbox is a mock: code that works against it is not guaranteed to behave the same in production. Always verify against production, and start with small amounts.",
		},
		Examples: []string{
			"{prog} sandbox start",
			"{prog} sandbox start --paper --fresh",
			"{prog} sandbox start --paper --all-pairs --fresh",
			"{prog} sandbox start --fresh",
			"{prog} sandbox start --runtime deno",
			"{prog} sandbox start --port 9000",
		},
	},
	{
		ID: []string{"sandbox", "stop"}, Section: cmdmeta.SectionMeta,
		Summary:  "stop the running local sandbox",
		Notes:    []string{"Reads the sandbox pidfile and signals a graceful shutdown. Runtime-free — needs neither Node nor Deno."},
		Examples: []string{"{prog} sandbox stop"},
	},
	{
		ID: []string{"sandbox", "status"}, Section: cmdmeta.SectionMeta,
		Summary:  "show the sandbox source, runtime, server state, and imported key",
		Notes:    []string{"Runtime-free inspection: reports the bundle source and whether Deno has cached it, the runtime that would be used and the Deno binary path, whether a server is running and reachable, and whether the seeded key is imported."},
		Examples: []string{"{prog} sandbox status"},
	},
	{
		ID: []string{"sandbox", "update"}, Section: cmdmeta.SectionMeta,
		Summary:  "re-fetch the sandbox bundle from the Official Source",
		Params:   []cmdmeta.Param{{Flag: "url", API: "url", Kind: cmdmeta.KindString, Desc: "override the bundle source (default: the Official Source; a local path or file:// is run from disk)"}},
		Notes:    []string{"Forces Deno to re-fetch the bundle from the source URL into its module cache (`deno cache --reload`), replacing a stale copy. A local / file:// source needs no refresh (it is read fresh each run). The managed Deno binary itself is updated separately with `{prog} sandbox runtime update`."},
		Examples: []string{"{prog} sandbox update"},
	},
	{
		ID: []string{"sandbox", "license"}, Section: cmdmeta.SectionMeta,
		Summary: "show the sandbox's own license / terms of use",
		Notes: []string{
			"The Digital X API Sandbox is separately licensed: it is proprietary software of Digital X Co., Ltd. under its own terms, NOT covered by digitalx-cli's open-source license. This prints those terms straight from the bundle (a shortcut for `sandbox exec license`). digitalx-cli only fetches and runs the bundle from the Official Source for local development and testing — any fork or downstream use must keep that use conformant.",
			"The global --lang selects the language when the bundle offers the terms in more than one; with none it follows your locale.",
		},
		Examples: []string{"{prog} sandbox license", "{prog} sandbox license --lang ko"},
	},
	{
		ID: []string{"sandbox", "exec"}, Section: cmdmeta.SectionMeta,
		Summary: "run a sandbox bundle subcommand against the managed database",
		Notes: []string{
			"Passes its arguments straight through to the bundle under the resolved Deno runtime against the managed database, so you can poke sandbox state without re-implementing its subcommands (set-balance, set-market, add-pair, add-user, …).",
			"Put the subcommand and its flags after the command, e.g. `{prog} sandbox exec set-balance --user 1 --currency btc --available 5`. Everything after `exec` is forwarded verbatim — including `--help` (so `{prog} sandbox exec set-balance --help` shows the bundle's own help) — so the global flags (`--json`, `--key`, …) are not interpreted here. Run `{prog} sandbox exec` with no subcommand for this command's help.",
			"Use `--` to forward a leading token that would otherwise look like a flag, e.g. `{prog} sandbox exec -- --help`.",
		},
		Examples: []string{
			"{prog} sandbox exec set-balance --user 1 --currency btc --available 5",
			"{prog} sandbox exec status --json",
		},
	},
	{
		ID: []string{"sandbox", "runtime", "status"}, Section: cmdmeta.SectionMeta,
		Summary: "show the managed Deno's cached vs pinned version",
		Notes: []string{
			"The managed Deno is the default sandbox runtime — a single pinned, SHA-256-verified Deno binary the CLI downloads into the shared cache, which runs the bundle under a least-privilege permission sandbox. (A `deno` already on your PATH can be used instead with `sandbox start --runtime deno`.) `sandbox start` installs the managed Deno automatically when needed; the `install`/`update` subcommands make it explicit (e.g. to pre-warm CI).",
		},
		Examples: []string{"{prog} sandbox runtime status"},
	},
	{
		ID: []string{"sandbox", "runtime", "install"}, Section: cmdmeta.SectionMeta,
		Summary:  "download the pinned managed Deno if it is missing",
		Notes:    []string{"Downloads the pinned, SHA-256-verified managed Deno into the shared cache if it is not already present, then reports its status. A no-op when the pinned version is already installed."},
		Examples: []string{"{prog} sandbox runtime install"},
	},
	{
		ID: []string{"sandbox", "runtime", "update"}, Section: cmdmeta.SectionMeta,
		Summary:  "force a re-download of the pinned managed Deno",
		Notes:    []string{"Re-downloads and SHA-256-verifies the pinned managed Deno into the shared cache, replacing any cached copy, then reports its status."},
		Examples: []string{"{prog} sandbox runtime update"},
	},
}
