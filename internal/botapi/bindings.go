// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/stream/state"
)

// The korbit.* surface is GENERATED from the ops catalog — every endpoint
// operation becomes a method: one-segment ids attach to korbit directly
// (korbit.ticker), deeper ids nest one object per segment (korbit.order.place,
// korbit.krw.deposit.history). Arguments are a
// single options object keyed on WIRE parameter names (the names the `commands`
// catalog publishes), with the obvious single positional (symbol/currency/
// amount) allowed positionally. Values run through the same ops validation
// engine (CoerceValue + NormalizeValue + the operation's CrossValidate) as the
// CLI, then the operation runs — so the bindings carry NO call policy of their
// own. The operation decides every behavior: order.place runs the clientOrderId
// reconcile protocol, order.history/fills page transparently, candles auto-pages
// past the server's 200-row cap, the funding histories cap honestly, and the
// single-shot money movers send exactly once.

// jsNames maps a command-id segment to its JS name where they differ.
var jsNames = map[string]string{"ticksize": "tickSize"}

// jsHiddenCommands are endpoint operations deliberately NOT exposed as korbit.*
// methods, even though they remain CLI commands. korbit.time (GET /v2/time) is
// a footgun in a bot: it is a network round-trip for the current time, which a
// script should read locally with Date.now() (or the clock-corrected
// korbit.now()). Exposing it only invites a needless REST call — and rate-limit
// exposure — for a value the runtime already has.
var jsHiddenCommands = map[string]bool{"time": true}

func jsName(segment string) string {
	if n, ok := jsNames[segment]; ok {
		return n
	}
	return segment
}

// jsMethodName renders the user-facing method name for error messages.
func jsMethodName(id []string) string {
	parts := make([]string, len(id))
	for i, s := range id {
		parts[i] = jsName(s)
	}
	return "korbit." + strings.Join(parts, ".")
}

// installKorbit builds the korbit global from the ops catalog.
func (r *Runtime) installKorbit(vm *goja.Runtime) error {
	korbitObj := vm.NewObject()
	groups := map[string]*goja.Object{}

	// groupFor returns the nested object a command-id prefix maps to, creating
	// every intermediate level on demand. The empty prefix is korbit itself, so a
	// one-segment id attaches its method directly and a deeper id nests one object
	// per leading segment (korbit.order, korbit.krw.deposit).
	var groupFor func(prefix []string) (*goja.Object, error)
	groupFor = func(prefix []string) (*goja.Object, error) {
		if len(prefix) == 0 {
			return korbitObj, nil
		}
		key := strings.Join(prefix, ".")
		if g, ok := groups[key]; ok {
			return g, nil
		}
		parent, err := groupFor(prefix[:len(prefix)-1])
		if err != nil {
			return nil, err
		}
		g := vm.NewObject()
		groups[key] = g
		if err := parent.Set(jsName(prefix[len(prefix)-1]), g); err != nil {
			return nil, err
		}
		return g, nil
	}

	for _, op := range ops.Catalog() {
		m := op.Meta()
		if jsHiddenCommands[m.Key()] {
			continue
		}
		parent, err := groupFor(m.ID[:len(m.ID)-1])
		if err != nil {
			return err
		}
		if err := parent.Set(jsName(m.ID[len(m.ID)-1]), r.makeMethod(vm, op)); err != nil {
			return err
		}
	}

	// korbit.now(): the session's server-clock estimate in unix ms.
	now := func(goja.FunctionCall) goja.Value {
		if r.opts.ServerNow != nil {
			return vm.ToValue(r.opts.ServerNow())
		}
		return vm.ToValue(time.Now().UnixMilli())
	}
	if err := korbitObj.Set("now", now); err != nil {
		return err
	}
	return vm.Set("korbit", korbitObj)
}

// makeMethod builds one korbit.* method: parse+validate synchronously (a
// malformed call throws a TypeError before any network), then dispatch the
// operation to the worker pool and return a Promise. The operation owns the
// behavior — there is no per-method behavior switch here.
func (r *Runtime) makeMethod(vm *goja.Runtime, op ops.Operation) func(goja.FunctionCall) goja.Value {
	m := op.Meta()
	name := jsMethodName(m.ID)
	isPlace := m.Key() == "order place"
	return func(call goja.FunctionCall) goja.Value {
		r.gateAPI(vm, name, m.Auth != nil)

		parsed, err := r.parseArgs(vm, op, call)
		if err != nil {
			panic(vm.NewTypeError("%s: %s", name, err.Error()))
		}

		// order.place mints the clientOrderId on the loop (before any dispatch), not
		// on a worker, so every resend the ops reconcile protocol makes reuses it.
		if isPlace {
			if _, ok := parsed.params["clientOrderId"]; !ok {
				parsed.params["clientOrderId"] = ops.MintClientOrderID()
			}
		}
		in := ops.RunInput{
			Values:   parsed.params,
			Controls: ops.Controls{Surface: r.opts.Surface},
			KeyName:  r.opts.KeyName,
			APIKeyID: r.opts.APIKeyID,
		}
		work := func() (any, error) {
			return op.Run(r.opCtx(), r.opts.API, in)
		}
		if isPlace {
			// A stateful runtime wraps the place so the order's local balance
			// hold is registered before it is sent and released when the place
			// fails — see withPlaceHold.
			work = r.withPlaceHold(work, parsed.params)
		}
		return r.dispatch(vm, work, func(vm *goja.Runtime, res any) (goja.Value, error) {
			return r.resolveResult(vm, res.(ops.Result))
		})
	}
}

// withPlaceHold wraps a place operation with local balance-hold accounting
// (state.AddLocalHold — see the local-hold contract there), keyed by the
// clientOrderId the params already carry, so a bot sizing follow-up orders
// against state.balances() sees Available net of its own in-flight orders.
// Must be called on the loop goroutine; the returned func runs on a worker:
// it resolves the quote-fee headroom rate (quoteFeeRate — a blocking, cached
// read), registers the hold ON the loop and waits so the hold is in place
// before the order is sent, runs the place, and on failure releases the hold
// (the order is not known to be in flight, so its hold must not stand). The
// wrap is a no-op passthrough when there is nothing to hold: without
// --stateful there is no store, and an inestimable order (a shape with no
// bounded notional) just skips its hold — that order degrades to the
// no-holds status quo. A bot that never subscribes myOrder still converges:
// the TTL backstop releases.
func (r *Runtime) withPlaceHold(work func() (any, error), params map[string]string) func() (any, error) {
	if !r.stateful || r.store == nil {
		return work
	}
	base, quote, found := strings.Cut(params["symbol"], "_")
	if !found {
		return work
	}
	intent := state.HoldIntent{
		Side:  params["side"],
		Price: params["price"], Qty: params["qty"], Amt: params["amt"],
		Base: base, Quote: quote,
	}
	// Shape check without the fee rate (headroom only scales the amount, it
	// never turns an inestimable shape estimable).
	if _, _, ok := intent.EstimateHold(); !ok {
		return work
	}
	seq := accountseq.Main
	if n, err := strconv.Atoi(params["accountSeq"]); err == nil && n > 0 {
		seq = n
	}
	cid := params["clientOrderId"]
	return func() (any, error) {
		if intent.Side == "buy" {
			// A quote-fee buy reserves notional*(1+maxFeeRate); the first
			// place per {account, symbol} pays one blocking read for the rate.
			intent.QuoteFeeRate = r.quoteFeeRate(seq, params["symbol"], quote)
		}
		registered := false
		if cur, amt, ok := intent.EstimateHold(); ok {
			// Register on the loop (the store's goroutine) and WAIT, so the
			// hold is visible before the order leaves. RunOnLoop only fails
			// when the loop is shutting down — then nothing is registered.
			done := make(chan struct{})
			if r.loop.RunOnLoop(func(*goja.Runtime) {
				r.store.AddLocalHold(seq, cid, cur, amt)
				close(done)
			}) {
				<-done
				registered = true
			}
		}
		res, err := work()
		if err != nil && registered {
			r.loop.RunOnLoop(func(*goja.Runtime) { r.store.ReleaseLocalHold(seq, cid) })
		}
		return res, err
	}
}

// feeRateKey keys the cached quote-fee headroom rate by sub-account and
// symbol (the fee tier is per sub-account).
type feeRateKey struct {
	accountSeq int
	symbol     string
}

// quoteFeeRate returns the fee rate a quote-funded buy on symbol must
// reserve as headroom for accountSeq: maxFeeRate when the account pays its
// buy fee in the quote currency, "" otherwise (a coin-fee buy reserves plain
// notional). The first call per {account, symbol} fetches the trading-fee
// policy through the fees operation, blocking the calling worker (never the
// loop); the answer is cached for the session and never invalidated — a
// mid-session fee-tier change only shifts how conservative the hold is. A
// failed fetch degrades to "" (a plain-notional hold) and is NOT cached, so
// a later place retries.
func (r *Runtime) quoteFeeRate(accountSeq int, symbol, quote string) string {
	key := feeRateKey{accountSeq: accountSeq, symbol: symbol}
	r.feeMu.Lock()
	rate, ok := r.feeRates[key]
	r.feeMu.Unlock()
	if ok {
		return rate
	}
	res, err := ops.Find("fees").Run(r.opCtx(), r.opts.API, ops.RunInput{
		Values:   map[string]string{"symbol": symbol, "accountSeq": strconv.Itoa(accountSeq)},
		Controls: ops.Controls{Surface: r.opts.Surface},
		KeyName:  r.opts.KeyName,
		APIKeyID: r.opts.APIKeyID,
	})
	if err != nil {
		return ""
	}
	var rows []struct {
		Symbol         string `json:"symbol"`
		BuyFeeCurrency string `json:"buyFeeCurrency"`
		MaxFeeRate     string `json:"maxFeeRate"`
	}
	if json.Unmarshal(res.Data, &rows) != nil {
		return ""
	}
	found := false
	for _, f := range rows {
		if f.Symbol == symbol {
			found = true
			if strings.EqualFold(f.BuyFeeCurrency, quote) {
				rate = f.MaxFeeRate
			}
		}
	}
	if !found {
		return ""
	}
	r.feeMu.Lock()
	if r.feeRates == nil {
		r.feeRates = map[feeRateKey]string{}
	}
	r.feeRates[key] = rate
	r.feeMu.Unlock()
	return rate
}

// resolveResult converts an ops.Result on the loop into a JS value. The verbatim
// data is parsed by the engine's JSON.parse (decimal strings stay strings), then
// the CLI-owned out-of-band signals are surfaced as MACHINE-visible properties:
//   - a truncated paged result carries truncated:true + note (resolveList's role)
//   - an eventual-consistency place ack carries acknowledgmentOnly:true + note
//
// ops already printed the human note to its Stderr (wired to the monitor's
// stderr) — here we only attach the script-visible properties so the cap/ack is
// never silent to bot logic.
func (r *Runtime) resolveResult(vm *goja.Runtime, res ops.Result) (goja.Value, error) {
	v, err := parseJSON(vm, res.Data)
	if err != nil {
		return nil, err
	}
	if res.Note == "" {
		return v, nil
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		return v, nil
	}
	_ = obj.Set("note", res.Note)
	if res.Truncated {
		_ = obj.Set("truncated", true)
	} else {
		// A non-truncated result carrying a note is the place protocol's
		// eventual-consistency fallback: the order landed but the full order could
		// not be read back, so the data is the accept ack, not the full order. A
		// list operation only ever sets Note together with Truncated, so it never
		// reaches this branch — acknowledgmentOnly is a place-only signal.
		_ = obj.Set("acknowledgmentOnly", true)
	}
	return v, nil
}

// gateAPI enforces the access rules every korbit.* call shares. It panics
// with a TypeError (the goja way to throw) when the call is not allowed here.
func (r *Runtime) gateAPI(vm *goja.Runtime, name string, auth bool) {
	if r.inWhere {
		panic(vm.NewTypeError("%s is not available inside --where — the filter must stay synchronous; do REST work in --init or --on", name))
	}
	if r.opts.API == nil {
		panic(vm.NewTypeError("%s is not available here", name))
	}
	if auth && r.opts.CredsErr != "" {
		panic(vm.NewTypeError("%s needs a signing key: %s", name, r.opts.CredsErr))
	}
}

// parsedArgs is a parsed+validated method invocation: the values keyed by API
// name, for the cross-field rules and the operation.
type parsedArgs struct {
	params map[string]string
}

// parseArgs maps a JS invocation onto the operation's params:
// method(positional?, options?) where options is keyed on wire names.
func (r *Runtime) parseArgs(vm *goja.Runtime, op ops.Operation, call goja.FunctionCall) (*parsedArgs, error) {
	m := op.Meta()
	params, positionals := m.Params, m.Positionals

	var posVal goja.Value
	var optsObj *goja.Object
	argIdx := 0
	if v := call.Argument(argIdx); !goja.IsUndefined(v) && !goja.IsNull(v) {
		if isPlainObject(v) {
			optsObj = v.ToObject(vm)
			argIdx++
		} else {
			posVal = v
			argIdx++
			if v2 := call.Argument(argIdx); !goja.IsUndefined(v2) && !goja.IsNull(v2) {
				if !isPlainObject(v2) {
					return nil, fmt.Errorf("the second argument must be an options object")
				}
				optsObj = v2.ToObject(vm)
				argIdx++
			}
		}
	}
	if v := call.Argument(argIdx); !goja.IsUndefined(v) && !goja.IsNull(v) {
		return nil, fmt.Errorf("too many arguments — expected (value?, options?)")
	}
	if posVal != nil && len(positionals) == 0 {
		return nil, fmt.Errorf("this method takes only an options object")
	}

	out := &parsedArgs{params: map[string]string{}}

	// The leading positional, when given.
	if posVal != nil {
		ps := positionals[0]
		raw, present, err := jsRawValue(cmdmeta.Param{Kind: ps.Kind}, posVal, ps.API)
		if err != nil {
			return nil, err
		}
		if present {
			v, err := cmdmeta.NormalizeValue(cmdmeta.Param{Kind: ps.Kind}, raw, ps.API)
			if err != nil {
				return nil, usageMessage(err)
			}
			out.params[ps.API] = v
		}
	}

	// Options object: every key must name a known wire param.
	if optsObj != nil {
		valid := validOptionKeys(params, positionals)
		for _, k := range optsObj.Keys() {
			target, ok := valid[k]
			if !ok {
				return nil, fmt.Errorf("unknown option %q — valid options: %s", k, optionKeyList(params, positionals))
			}
			if _, dup := out.params[k]; dup {
				return nil, fmt.Errorf("%q was given both positionally and as an option", k)
			}
			raw, present, err := jsRawValue(target, optsObj.Get(k), k)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			v, err := cmdmeta.NormalizeValue(target, raw, k)
			if err != nil {
				return nil, usageMessage(err)
			}
			out.params[k] = v
		}
	}

	// Apply defaults and enforce required positionals/params.
	for _, ps := range positionals {
		if _, ok := out.params[ps.API]; !ok && ps.Required {
			return nil, fmt.Errorf("%s is required", ps.API)
		}
	}
	for _, p := range params {
		if _, ok := out.params[p.API]; !ok {
			if p.Default != "" {
				v, err := cmdmeta.NormalizeValue(p, p.Default, p.API)
				if err != nil {
					return nil, usageMessage(err)
				}
				out.params[p.API] = v
			} else if p.Required {
				return nil, fmt.Errorf("%s is required", p.API)
			}
		}
	}
	acctSeqDefault := ""
	if r.opts.KeyManager != nil && r.opts.KeyName != "" && !r.opts.Inline {
		acctSeqDefault = r.opts.KeyManager.MetaDefaultAccountSeq(r.opts.KeyName)
	}
	if _, err := accountseq.Ensure(params, out.params, acctSeqDefault); err != nil {
		return nil, usageMessage(err)
	}

	if cv := op.Meta().CrossValidate; cv != nil {
		if err := cv(out.params); err != nil {
			return nil, usageMessage(err)
		}
	}
	return out, nil
}

func validOptionKeys(params []cmdmeta.Param, positionals []cmdmeta.Positional) map[string]cmdmeta.Param {
	valid := map[string]cmdmeta.Param{}
	for _, p := range params {
		valid[p.API] = p
	}
	for _, ps := range positionals {
		if _, ok := valid[ps.API]; !ok {
			valid[ps.API] = cmdmeta.Param{API: ps.API, Kind: ps.Kind}
		}
	}
	return valid
}

func optionKeyList(params []cmdmeta.Param, positionals []cmdmeta.Positional) string {
	var b bytes.Buffer
	seen := map[string]bool{}
	add := func(api string) {
		if seen[api] {
			return
		}
		seen[api] = true
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(api)
	}
	for _, ps := range positionals {
		add(ps.API)
	}
	for _, p := range params {
		add(p.API)
	}
	return b.String()
}

// jsRawValue converts one JS value to the raw string NormalizeValue expects,
// enforcing the per-kind type rules. present=false means "treat as omitted"
// (undefined/null, or a false boolean flag). The goja-specific undefined/null
// markers are handled here; the per-kind type rules (incl. the money-must-be-a-
// string guard) live in the shared cmdmeta.CoerceValue, so the bot API and the MCP
// server can never diverge on them.
func jsRawValue(p cmdmeta.Param, v goja.Value, label string) (raw string, present bool, err error) {
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return "", false, nil
	}
	return cmdmeta.CoerceValue(p, v.Export(), label)
}

// isPlainObject reports whether v is a plain options object (not an array,
// not a primitive).
func isPlainObject(v goja.Value) bool {
	_, ok := v.Export().(map[string]interface{})
	return ok
}

// usageMessage strips the validation error down to its message (those errors are
// CLI usage errors; in JS they surface as TypeErrors).
func usageMessage(err error) error { return fmt.Errorf("%s", err.Error()) }

// dispatch is the generic Promise bridge: work runs on the worker pool;
// convert turns its result into a JS value back on the loop. Must be called
// on the loop goroutine (it creates the Promise in the runtime).
func (r *Runtime) dispatch(vm *goja.Runtime, work func() (any, error), convert func(vm *goja.Runtime, res any) (goja.Value, error)) goja.Value {
	promise, resolve, reject := vm.NewPromise()
	r.pool.submit(func() {
		res, err := work()
		r.loop.RunOnLoop(func(vm *goja.Runtime) {
			if err != nil {
				_ = reject(jsError(vm, err))
				return
			}
			v, cerr := convert(vm, res)
			if cerr != nil {
				_ = reject(jsError(vm, cerr))
				return
			}
			_ = resolve(v)
		})
	})
	return vm.ToValue(promise)
}
