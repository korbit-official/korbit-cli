// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/spec"
	"github.com/korbit-official/korbit-cli/internal/stream"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// This is the GOLDEN surface-pin test: a conscious-edit gate over the generated
// korbit.* API, the frozen bare hook bindings, the rejection error field names,
// and the global surface. The expectations are HARDCODED (derived once, by hand,
// from the current spec) — deliberately NOT computed from spec.Registry at test
// time, which would silently absorb a rename/move and defeat the purpose. When a
// spec refactor changes the surface, this test fails and forces a deliberate
// decision: extend the golden list additively, or stop the rename.

// wantKorbitMethods is the EXACT set of dotted korbit.* method names. Adding an
// endpoint command adds a line here; renaming/moving one breaks an existing line
// (that is the gate). korbit.now is asserted separately (it is not spec-derived).
// korbit.time is intentionally ABSENT: the `time` endpoint command exists in the
// spec but is hidden from the bot surface (jsHiddenCommands) — a script reads the
// clock locally via Date.now()/korbit.now(), never a REST round-trip. Do not add
// it back to "fix" a generated-surface change; remove it from jsHiddenCommands.
var wantKorbitMethods = []string{
	"korbit.balance",
	"korbit.candles",
	"korbit.currencies",
	"korbit.deposit.address",
	"korbit.deposit.addresses",
	"korbit.deposit.generate",
	"korbit.deposit.history",
	"korbit.deposit.status",
	"korbit.fees",
	"korbit.fills",
	"korbit.krw.deposit.history",
	"korbit.krw.deposit.request",
	"korbit.krw.withdraw.history",
	"korbit.krw.withdraw.request",
	"korbit.order.cancel",
	"korbit.order.get",
	"korbit.order.history",
	"korbit.order.open",
	"korbit.order.place",
	"korbit.orderbook",
	"korbit.pairs",
	"korbit.tickSize",
	"korbit.ticker",
	"korbit.trades",
	"korbit.whoami",
	"korbit.withdraw.addresses",
	"korbit.withdraw.amount",
	"korbit.withdraw.cancel",
	"korbit.withdraw.history",
	"korbit.withdraw.request",
	"korbit.withdraw.status",
}

// dottedKorbitMethods reads back the dump the surface-probe runtime built in
// --init: the --where evaluation prints the JSON array to (captured) stderr,
// then this decodes it. (--where is the simplest JS->Go channel that does not
// touch the gated korbit./db. surface.)
func dottedKorbitMethods(t *testing.T, r *Runtime) []string {
	t.Helper()
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("surface probe --where failed: ok=%v err=%v", ok, err)
	}
	raw := strings.TrimSpace(capturedSurface)
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("could not decode the surface dump %q: %v", raw, err)
	}
	sort.Strings(got)
	return got
}

// capturedSurface collects the surface-probe runtime's console.error output; a
// package-level var keeps the plumbing simple (these tests run sequentially).
var capturedSurface string

func TestGoldenKorbitMethodSet(t *testing.T) {
	capturedSurface = ""
	ft := newFakeTransport()
	r := newTestRuntime(t, Options{
		API: ft.api(apiExtra{}),
		// Enumerate every dotted function path under korbit, at any nesting depth
		// (skip korbit.now).
		Init: `globalThis.__dump = (function () {
			var out = [];
			(function walk(obj, path) {
				Object.keys(obj).forEach(function (k) {
					if (path === 'korbit' && k === 'now') return;
					var v = obj[k];
					var p = path + '.' + k;
					if (typeof v === 'function') { out.push(p); return; }
					if (v && typeof v === 'object') walk(v, p);
				});
			})(korbit, 'korbit');
			return JSON.stringify(out);
		})();`,
		// --where reports the dump to stderr (captured) and matches.
		Where:  "(console.error(__dump), true)",
		Stderr: writerFunc(func(p []byte) { capturedSurface += string(p) }),
	})
	got := dottedKorbitMethods(t, r)

	want := append([]string(nil), wantKorbitMethods...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("korbit.* surface drift.\n got: %v\nwant: %v\n(extend the golden list ADDITIVELY, or stop the rename)", got, want)
	}
}

// wantStateMethods is the EXACT set of state.* method names. Like the korbit.*
// and ta golden lists, this is HARDCODED so a rename/removal breaks a line and
// forces a deliberate decision — state joins the frozen, extend-only globals.
var wantStateMethods = []string{
	"balances",
	"fills",
	"health",
	"notices",
	"openOrders",
	"order",
	"orderbook",
	"ready",
	"ticker",
	"trades",
}

// TestGoldenStateSurface pins the state.* method names. The global is installed
// regardless of --stateful (only CALLING a method without it throws), so a plain
// runtime can enumerate the names without building a Store.
func TestGoldenStateSurface(t *testing.T) {
	capturedSurface = ""
	r := newTestRuntime(t, Options{
		Init:   `globalThis.__dump = JSON.stringify(Object.keys(state).filter(function (k) { return typeof state[k] === 'function' }))`,
		Where:  "(console.error(__dump), true)",
		Stderr: writerFunc(func(p []byte) { capturedSurface += string(p) }),
	})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("surface probe --where failed: ok=%v err=%v", ok, err)
	}
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(capturedSurface)), &got); err != nil {
		t.Fatalf("could not decode the state surface dump %q: %v", capturedSurface, err)
	}
	sort.Strings(got)
	want := append([]string(nil), wantStateMethods...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("state.* surface drift.\n got: %v\nwant: %v\n(extend the golden list ADDITIVELY, or stop the rename)", got, want)
	}
}

// TestGoldenStateFieldNames pins the RETURNED field names of every state.*
// converter. These are part of the frozen, additive-only contract (deployed bots
// read them by name), so a rename/removal must break this test — the method-name
// pin above does not cover them. The converters are pure Go, so this asserts their
// output keys directly. Extend a want-list ADDITIVELY, or stop the rename.
func TestGoldenStateFieldNames(t *testing.T) {
	keysOf := func(m map[string]any) []string {
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	level := levelsToJS([]state.PriceLevel{{}})[0].(map[string]any)
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"order", keysOf(orderToJS(state.Order{})), []string{"accountSeq", "amt", "avgPrice", "clientOrderId", "createdAt", "filledAmt", "filledQty", "lastFilledAt", "orderId", "orderType", "price", "qty", "side", "status", "symbol"}},
		{"balance", keysOf(balanceToJS(state.Balance{})), []string{"accountSeq", "available", "avgPrice", "balance", "currency", "tradeInUse", "updatedAt", "withdrawalInUse"}},
		{"ticker", keysOf(tickerToJS(state.Ticker{})), []string{"bestAskPrice", "bestBidPrice", "close", "high", "lastTradedAt", "low", "open", "prevClose", "priceChange", "priceChangePercent", "quoteVolume", "symbol", "volume"}},
		{"orderbook", keysOf(orderbookToJS(state.Orderbook{})), []string{"asks", "bids", "symbol", "timestamp"}},
		{"priceLevel", keysOf(level), []string{"price", "qty"}},
		{"trade", keysOf(tradeToJS(state.Trade{})), []string{"isBuyerTaker", "price", "qty", "timestamp", "tradeId"}},
		{"fill", keysOf(fillToJS(state.Fill{})), []string{"accountSeq", "fee", "feeCurrency", "isTaker", "orderId", "price", "qty", "side", "symbol", "time", "tradeId"}},
		{"health", keysOf(healthToJS(state.Health{})), []string{"backfilling", "dataCount", "gapCount", "lastDataAt", "private", "public"}},
		{"endpointHealth", keysOf(endpointHealthToJS(state.EndpointHealth{})), []string{"known", "lastChangeAt", "lastError", "up"}},
		{"notice", keysOf(noticeToJS(stream.Notice{})), []string{"code", "details", "level", "message", "time"}},
	}
	for _, tc := range cases {
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if strings.Join(tc.got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s field-name drift.\n got: %v\nwant: %v", tc.name, tc.got, want)
		}
	}
	// state.ready returns a fixed boolean object built inline (not a converter);
	// pin its keys via a --where dump, the same JS->Go channel the surface probes use.
	capturedReady = ""
	rr := newTestRuntime(t, Options{
		Stateful: true,
		Init:     `globalThis.__rk = JSON.stringify(Object.keys(state.ready('btc_krw')))`,
		Where:    "(console.error(__rk), true)",
		Stderr:   writerFunc(func(p []byte) { capturedReady += string(p) }),
	})
	if ok, err := rr.Match(dataEvent(`{}`)); err != nil || !ok {
		t.Fatalf("ready probe failed: ok=%v err=%v", ok, err)
	}
	var readyKeys []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(capturedReady)), &readyKeys); err != nil {
		t.Fatalf("decode ready keys %q: %v", capturedReady, err)
	}
	sort.Strings(readyKeys)
	wantReady := []string{"balances", "openOrders", "orderbook", "ticker", "trades"}
	if strings.Join(readyKeys, "\n") != strings.Join(wantReady, "\n") {
		t.Errorf("state.ready field drift.\n got: %v\nwant: %v", readyKeys, wantReady)
	}
}

var capturedReady string

// TestTASurfaceDocumentedInMonitorNotes is the docs<->binding drift guard: every
// ta indicator the runtime exposes (the golden wantTASurface list, pinned to the
// binding by TestGoldenTASurface) must be named in the monitor command's spec
// Notes — which feed `commands`, `--help`, and the MCP bot_runtime_reference — so
// an agent that only has the binary can discover the whole ta surface.
func TestTASurfaceDocumentedInMonitorNotes(t *testing.T) {
	c := spec.Find([]string{"monitor"})
	if c == nil {
		t.Fatal("monitor command missing from the spec")
	}
	// The ta reference lives in the experimental notes (the runtime is gated), so
	// join both buckets — they together feed `commands`, `--help
	// --enable-experimental`, and the MCP bot_runtime_reference.
	notes := strings.Join(append(append([]string{}, c.Notes...), c.ExperimentalNotes...), "\n")
	if !strings.Contains(notes, "ta.<name>") || !strings.Contains(notes, "ta.stream") {
		t.Fatal("the monitor Notes must describe the ta/ta.stream surface")
	}
	for _, dotted := range wantTASurface {
		// Reduce ta.sma / ta.stream.sma to the bare indicator name.
		name := strings.TrimPrefix(strings.TrimPrefix(dotted, "ta.stream."), "ta.")
		if !strings.Contains(notes, name) {
			t.Fatalf("ta indicator %q is not mentioned in the monitor command Notes — the agent-facing docs have drifted from the ta binding (update the monitor entry in internal/spec/registry.go)", name)
		}
	}
}

// TestGoldenSampledOptionKeys pins the accepted option keys for a representative
// sample. The JS surface is exactly the ops catalog: order.history/fills/candles
// all accept `limit` (a total-row cap for the histories, the auto-paging ceiling
// for candles). It reads each method's keys from the unknown-option error's
// "valid options:" list — a malformed call throws synchronously, before any network.
func TestGoldenSampledOptionKeys(t *testing.T) {
	cases := []struct {
		call string // a call carrying a bogus option, to trigger the list
		want string
	}{
		{`korbit.order.place({__x:1})`, "symbol, side, orderType, price, qty, amt, timeInForce, bestNth, clientOrderId, pp, ppPercent, accountSeq"},
		{`korbit.order.history({__x:1})`, "symbol, limit, startTime, endTime, accountSeq"},
		{`korbit.fills({__x:1})`, "symbol, limit, startTime, endTime, accountSeq"},
		{`korbit.candles({__x:1})`, "symbol, interval, limit, startTime, endTime"},
		{`korbit.deposit.history({__x:1})`, "currency, limit, accountSeq"},
	}
	for _, tc := range cases {
		ft := newFakeTransport()
		on := `try { await ` + tc.call + `; throw new Error('expected a TypeError'); }
			catch (e) {
				var m = /valid options: (.*)$/.exec(e.message);
				if (!m) throw new Error('no option list in: ' + e.message);
				if (m[1] !== ` + jsString(tc.want) + `) throw new Error('option keys drift: ' + m[1]);
			}`
		if err := runOn(t, Options{API: ft.api(apiExtra{}), On: on}, dataEvent(`{}`)); err != nil {
			t.Fatalf("%s: %v", tc.call, err)
		}
		if len(ft.reqs) != 0 {
			t.Fatalf("%s: a malformed call must send nothing", tc.call)
		}
	}
}

// TestGoldenHookBindings pins the bare names available inside --where/--on. This
// set is FROZEN: new event fields are added only as ev.<field>, never as new
// bare names (a new bare name would shadow a user global in deployed bots). The
// canary proves no stray binding leaked in.
func TestGoldenHookBindings(t *testing.T) {
	bare := []string{"channel", "symbol", "origin", "serverTime", "source", "payload", "rows", "ev"}
	// --where sees the same bindings as --on (minus korbit./db.). Assert each is
	// defined and a canary name is not — in a single predicate.
	var checks []string
	for _, n := range bare {
		checks = append(checks, "typeof "+n+" !== 'undefined'")
	}
	checks = append(checks, "typeof __surface_canary === 'undefined'")
	// New event fields are added on ev, never as a new bare name — pin ev.accountSeq.
	checks = append(checks, "('accountSeq' in ev)")
	where := strings.Join(checks, " && ")
	r := newTestRuntime(t, Options{Where: where})
	ok, err := r.Match(dataEvent(`{"data":{}}`))
	if err != nil || !ok {
		t.Fatalf("hook bindings drift in --where: ok=%v err=%v", ok, err)
	}

	// --on sees the same frozen set, plus ev.type. Throw on any mismatch.
	onChecks := append([]string(nil), checks...)
	onChecks = append(onChecks, "ev.type === 'data'")
	on := "if (!(" + strings.Join(onChecks, " && ") + ")) throw new Error('hook bindings drift in --on')"
	if err := runOn(t, Options{On: on}, dataEvent(`{"data":{}}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestGoldenRejectionFields pins the rejection error field names on an API
// failure: code, httpStatus, description, body (+ retryAfterSec when present).
func TestGoldenRejectionFields(t *testing.T) {
	ft := newFakeTransport()
	ra := 7
	ft.on("GET", "/v2/balance", step{err: &output.ApiError{
		Message: "rate", HTTPStatus: 429, Code: "TOO_MANY_REQUESTS",
		RetryAfterSec: &ra, Body: json.RawMessage(`{"success":false,"error":{"message":"TOO_MANY_REQUESTS"}}`),
	}})
	on := `try { await korbit.balance(); throw new Error('expected a rejection'); }
		catch (e) {
			if (e.code !== 'TOO_MANY_REQUESTS') throw new Error('code: ' + e.code);
			if (e.httpStatus !== 429) throw new Error('httpStatus: ' + e.httpStatus);
			if (e.description !== 'rate') throw new Error('description: ' + e.description);
			if (e.retryAfterSec !== 7) throw new Error('retryAfterSec: ' + e.retryAfterSec);
			if (!e.body || e.body.error.message !== 'TOO_MANY_REQUESTS') throw new Error('body missing/parsed wrong');
		}`
	if err := runOn(t, Options{API: ft.api(apiExtra{}), On: on}, dataEvent(`{}`)); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestGoldenGlobals pins that every documented global is present: the timer
// globals, console, korbit.now, the ta indicator library, and db (only when
// DBPath is set). All asserted from a single --where truthiness probe — which
// also proves ta is reachable inside --where.
func TestGoldenGlobals(t *testing.T) {
	ft := newFakeTransport()
	present := []string{
		"setTimeout", "setInterval", "clearTimeout", "clearInterval",
	}
	var checks []string
	for _, n := range present {
		checks = append(checks, "typeof "+n+" === 'function'")
	}
	checks = append(checks,
		"typeof korbit === 'object'",
		"typeof korbit.now === 'function'",
		// ta is a synchronous global usable everywhere, including --where.
		"typeof ta === 'object'",
		"typeof ta.sma === 'function'",
		"typeof ta.stream === 'object'",
		"typeof ta.stream.ema === 'function'",
		// state is always installed (a sync global like ta); its methods throw
		// without --stateful, but the surface is present regardless.
		"typeof state === 'object'",
		"typeof state.openOrders === 'function'",
		// korbit.time is deliberately NOT exposed (jsHiddenCommands): a bot reads
		// the clock locally via Date.now()/korbit.now(), never a REST round-trip.
		"typeof korbit.time === 'undefined'",
		"typeof console === 'object'",
		"typeof console.log === 'function'",
		"typeof db === 'object'", // db is installed (its methods are usable) because DBPath is set
		"typeof db.exec === 'function' && typeof db.query === 'function' && typeof db.get === 'function'",
	)
	where := strings.Join(checks, " && ")
	r := newTestRuntime(t, Options{API: ft.api(apiExtra{}), DBPath: t.TempDir() + "/bot.db", Where: where})
	ok, err := r.Match(dataEvent(`{}`))
	if err != nil || !ok {
		t.Fatalf("global surface drift: ok=%v err=%v", ok, err)
	}

	// Without DBPath the db object is still installed (the surface is stable),
	// but every method throws "no database path configured" — that, not the
	// global's absence, is how db gates off. Probe one method from --on.
	on := `try { await db.get('SELECT 1'); throw new Error('expected db to be unavailable'); }
		catch (e) { if (!/no database path configured/.test(e.message)) throw new Error('wrong gate: ' + e.message); }`
	if err := runOn(t, Options{API: ft.api(apiExtra{}), On: on}, dataEvent(`{}`)); err != nil {
		t.Fatalf("db gate without --db: %v", err)
	}
}

// jsString renders s as a double-quoted JS string literal for embedding in test
// source (the option lists contain no quotes/backslashes, so this is enough).
func jsString(s string) string { return `"` + s + `"` }
