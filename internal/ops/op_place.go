// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/korbit-official/korbit-cli/internal/ids"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/rawapi"
)

// order place — the reconcile protocol over the typed rawapi layer.

const (
	// The order read path is eventually consistent: a just-accepted order can be
	// briefly invisible to a lookup. So a placement we have positive proof of (a
	// 2xx accept, or a DUPLICATE_CLIENT_ORDER_ID answer) is fetched with retries
	// spanning ~1s of waiting before we give up reading it back. ~1s is the window
	// past which a still-absent order is treated as a lagging read, not a missing
	// order — so we NEVER read an empty result as "not placed".
	// lookupAttempts sleeps lookupRetryMs between attempts: 5 attempts => 4 waits
	// => ~1000ms total (plus the lookups' own round-trips).
	lookupAttempts = 5
	lookupRetryMs  = 250
)

type placeOp struct{ meta OpMeta }

func (op placeOp) Meta() OpMeta { return op.meta }

func (op placeOp) Run(ctx context.Context, a *API, in RunInput) (Result, error) {
	args := bindPlace(in)
	ctx, h, err := a.beginOp(ctx, op.meta, in)
	if err != nil {
		return Result{}, err
	}
	if args.skipReconcile {
		return op.runAck(ctx, a, h, args)
	}
	return op.runReconcile(ctx, a, h, args)
}

// placeArgs is the typed, bound input for the place operation: the wire request
// plus the reconcile/journal fields the protocol reads. The clientOrderId is
// always set (minted by the bind when the caller omitted it) so every resend
// reuses the same idempotency key.
type placeArgs struct {
	req           rawapi.OrderPlaceRequest
	clientOrderID string // == *req.ClientOrderID; the idempotency key
	symbol        string // == string(req.Symbol); used by the lookup + journal
	skipReconcile bool
}

// bindPlace converts the validated RunInput into the typed place args once
// (minting the clientOrderId when absent), so the protocol body reads typed
// fields instead of the Values map.
func bindPlace(in RunInput) placeArgs {
	values := ensureClientOrderIDValues(in.Values)
	req := placeRequest(values)
	return placeArgs{
		req:           req,
		clientOrderID: values["clientOrderId"],
		symbol:        string(req.Symbol),
		skipReconcile: in.Controls.SkipReconcile,
	}
}

// MintClientOrderID returns a fresh clientOrderId. The caller mints it (on its
// own goroutine, before any dispatch) so the same id is reused across every
// resend the reconcile protocol makes. Exposed so a frontend that wants the id
// minted on a specific goroutine (e.g. botapi's JS event loop, where the param
// must be appended before the work is handed to the worker pool) controls the
// ordering; the place operation also mints one itself when the caller did not.
func MintClientOrderID() string { return ids.UUIDv7() }

// ensureClientOrderIDValues returns a copy of values that carries a
// clientOrderId, minting one when the caller did not supply it so every resend
// reuses the same id (the idempotency key).
func ensureClientOrderIDValues(values map[string]string) map[string]string {
	next := make(map[string]string, len(values)+1)
	for k, v := range values {
		next[k] = v
	}
	if _, ok := next["clientOrderId"]; !ok {
		next["clientOrderId"] = MintClientOrderID()
	}
	return next
}

// placeRequest builds the typed place request from validated values.
func placeRequest(values map[string]string) rawapi.OrderPlaceRequest {
	seq := reqAccountSeq(values)
	req := rawapi.OrderPlaceRequest{
		Symbol:        rawapi.Symbol(reqStr(values, "symbol")),
		Side:          rawapi.Side(reqStr(values, "side")),
		OrderType:     rawapi.OrderType(reqStr(values, "orderType")),
		Price:         optStr(values, "price"),
		Qty:           optStr(values, "qty"),
		Amt:           optStr(values, "amt"),
		BestNth:       optInt(values, "bestNth"),
		ClientOrderID: optStr(values, "clientOrderId"),
		PP:            optBool(values, "pp"),
		PPPercent:     optInt(values, "ppPercent"),
		AccountSeq:    &seq,
	}
	if v, ok := values["timeInForce"]; ok {
		tif := rawapi.TimeInForce(v)
		req.TimeInForce = &tif
	}
	return req
}

// startOrder journals the order's mint-time intent BEFORE anything is sent (the
// hard pre-send guarantee), through the operation handle's order seam.
func (a *API) startOrder(h OpHandle, args placeArgs) (OrderFinishFunc, error) {
	req := args.req
	return h.StartOrder(OrderIntent{
		ClientOrderID: args.clientOrderID,
		Symbol:        string(req.Symbol),
		Side:          string(req.Side),
		OrderType:     string(req.OrderType),
		Price:         deref(req.Price),
		Qty:           deref(req.Qty),
		Amt:           deref(req.Amt),
		Tif:           tifStr(req.TimeInForce),
		ParamsJSON:    placeParamsJSON(req),
	})
}

// deref returns the pointed-to string, or "" when nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// tifStr returns the time-in-force as a string, or "" when nil.
func tifStr(p *rawapi.TimeInForce) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// runReconcile runs the clientOrderId reconcile protocol: single-shot send,
// resend the SAME id only on the safe classes, resolve DUPLICATE by lookup, and
// the eventual-consistency endgame. The placement is issued via rawapi
// OrderPlace (single-shot) and the lookup via OrderGet.
func (op placeOp) runReconcile(ctx context.Context, a *API, h OpHandle, args placeArgs) (Result, error) {
	req := args.req
	clientOrderID := args.clientOrderID
	symbol := args.symbol
	accountSeq := *req.AccountSeq
	log := a.log()

	// The clientOrderId is the thread that ties the whole protocol's log lines
	// together (and the key a support reader uses to fetch the order). side/
	// orderType are non-sensitive request facts; price/qty are deliberately NOT
	// logged here (they ride the journal's params record, not the operational log).
	log.Debug("order place: start",
		"clientOrderId", clientOrderID,
		"symbol", symbol,
		"side", string(req.Side),
		"orderType", string(req.OrderType),
		"accountSeq", accountSeq)

	finish, jerr := a.startOrder(h, args)
	if jerr != nil {
		// The order intent could not be durably recorded, so nothing is sent; mark
		// the operation failed rather than leaving its ledger row at 'running'.
		h.Finish(outcomeFailed, "")
		return Result{}, jerr
	}

	send := func() (json.RawMessage, korbit.Meta, error) {
		_, b, meta, err := a.Raw.OrderPlace(ctx, req, korbit.Policy{})
		return b, meta, err
	}

	var (
		placedData json.RawMessage
		duplicate  bool
		dupErr     error
		sends      int
	)
	budget := int64(a.RetryBudgetMs)

	gov := korbit.NewRetryGovernor(true, true, a.Resync != nil, budget)
	var lastErr error
	for gov.Attempt() {
		data, _, err := send()
		sends++
		if err == nil {
			trace(log, "order place: send accepted", "clientOrderId", clientOrderID, "attempt", sends)
			placedData = data
			break
		}
		lastErr = err
		class := korbit.Classify(err)
		ae := apiErrOf(err)
		trace(log, "order place: send failed",
			"clientOrderId", clientOrderID, "attempt", sends,
			"class", class.String(), "code", codeOf(ae))
		if ae != nil && ae.Code == "DUPLICATE_CLIENT_ORDER_ID" {
			// Pre-existing order for this id: not a failure — the order IS on the
			// server; the endgame resolves it by lookup.
			log.Debug("order place: duplicate clientOrderId — order already exists, resolving by lookup",
				"clientOrderId", clientOrderID)
			duplicate = true
			dupErr = err
			break
		}

		dec := gov.Next(class, korbit.RetryAfterMs(err))
		// The retry-governor decision is the ops-owned reasoning the wire layer
		// can't see (it logs each HTTP attempt; this logs why ops resends or stops).
		log.Debug("order place: retry decision",
			"clientOrderId", clientOrderID, "attempt", sends,
			"class", class.String(), "action", dec.Action.String(),
			"waitMs", dec.Wait.Milliseconds(), "reason", dec.Reason)
		switch dec.Action {
		case korbit.RetryResync:
			if rerr := a.Resync(); rerr != nil {
				log.Debug("order place: clock resync failed — giving up",
					"clientOrderId", clientOrderID, "err", rerr.Error())
				finish("failed", "", codeOf(ae), sends)
				ferr := h.Finish(outcomeFailed, codeOf(ae))
				return Result{Attempts: sends, JournalErr: ferr}, err
			}

		case korbit.RetryWait:
			if serr := a.sleep(ctx, dec.Wait); serr != nil {
				if class == korbit.ClassTransient {
					return a.reconcileUnknownTyped(ctx, h, symbol, clientOrderID, accountSeq, err, finish, sends)
				}
				finish("failed", "", codeOf(ae), sends)
				ferr := h.Finish(outcomeFailed, codeOf(ae))
				return Result{Attempts: sends, JournalErr: ferr}, err
			}

		case korbit.RetryGiveUp:
			if class == korbit.ClassTransient {
				return a.reconcileUnknownTyped(ctx, h, symbol, clientOrderID, accountSeq, err, finish, sends)
			}
			finish("failed", "", codeOf(ae), sends)
			ferr := h.Finish(outcomeFailed, codeOf(ae))
			return Result{Attempts: sends, JournalErr: ferr}, err
		}
	}

	if placedData == nil && !duplicate {
		if lastErr == nil {
			lastErr = errors.New("order place retry loop reached its hard iteration ceiling")
		}
		log.Debug("order place: retry loop hit its hard iteration ceiling — treating placement as UNKNOWN",
			"clientOrderId", clientOrderID, "sends", sends)
		fmt.Fprintf(a.stderr(), "korbit-cli: order place: retry loop reached its hard iteration ceiling — treating the placement as UNKNOWN\n")
		return a.reconcileUnknownTyped(ctx, h, symbol, clientOrderID, accountSeq, lastErr, finish, sends)
	}

	doc, found, lerr := a.lookupOrderTyped(ctx, symbol, clientOrderID, accountSeq)
	if found {
		orderID := jsonNumberField(doc, "orderId")
		log.Debug("order place: accepted", "clientOrderId", clientOrderID, "orderId", orderID, "sends", sends)
		finish("accepted", orderID, "", sends)
		ferr := finishOK(a, h)
		return Result{Data: doc, Attempts: sends, JournalErr: ferr}, nil
	}

	detail := "could not be read back within the verify window (the order read may be lagging)"
	if lerr != nil {
		detail = fmt.Sprintf("could not be read back: %v (the verification lookup failed)", lerr)
	}
	res := Result{Attempts: sends}
	if duplicate {
		// The order IS placed (the server rejected a re-use of its id) but the
		// read-back lagged. The verdict reaches the user through program output
		// (res.Note + the returned error); this Debug line is the structured copy
		// for the combined --log-file trail, not a second terminal message.
		log.Debug("order place: duplicate accepted but order could not be read back — order IS placed",
			"clientOrderId", clientOrderID, "sends", sends)
		finish("accepted", "", "", sends)
		res.JournalErr = h.Finish(outcomeOK, "DUPLICATE_CLIENT_ORDER_ID")
		res.Note = fmt.Sprintf("clientOrderId %s already has an order on the server (DUPLICATE_CLIENT_ORDER_ID) but it %s — the order IS placed; do NOT re-place, fetch it with `%s order get --symbol %s --client-order-id %s`", clientOrderID, detail, progname.Name(), symbol, clientOrderID)
		fmt.Fprintf(a.stderr(), "korbit-cli: order place: %s\n", res.Note)
		return res, fmt.Errorf("%s: %w", res.Note, dupErr)
	}
	orderID := jsonNumberField(placedData, "orderId")
	// The send was accepted (2xx) but the full order couldn't be fetched back yet
	// (eventually-consistent read). The order IS placed; the ack-without-full-order
	// is surfaced to the user as program output (res.Note + the in-band
	// acknowledgmentOnly flag), so this Debug line only adds it to the trail.
	log.Debug("order place: accepted but full order not yet readable — returning the ack",
		"clientOrderId", clientOrderID, "orderId", orderID, "sends", sends)
	finish("accepted", orderID, "", sends)
	res.JournalErr = finishOK(a, h)
	res.Data = placedData
	if isEmptyDoc(placedData) {
		res.Data = json.RawMessage(fmt.Sprintf(`{"clientOrderId":%q}`, clientOrderID))
	}
	orderRef := "orderId " + orderID
	if orderID == "" {
		orderRef = "clientOrderId " + clientOrderID
	}
	res.Note = fmt.Sprintf("the order was placed (%s) but the full order %s — this is the placement acknowledgement, not the full order with fill state; fetch it with `%s order get --symbol %s --client-order-id %s`", orderRef, detail, progname.Name(), symbol, clientOrderID)
	fmt.Fprintf(a.stderr(), "korbit-cli: order place: %s\n", res.Note)
	return res, nil
}

// runAck is the single-shot, NO-RECONCILE placement (the --no-reconcile path):
// send exactly once, return the raw accept ack, classify honestly, never resend
// and never look up.
func (op placeOp) runAck(ctx context.Context, a *API, h OpHandle, args placeArgs) (Result, error) {
	clientOrderID := args.clientOrderID
	req := args.req
	log := a.log()
	log.Debug("order place: start (no-reconcile, single-shot)",
		"clientOrderId", clientOrderID, "symbol", args.symbol,
		"side", string(req.Side), "orderType", string(req.OrderType))

	finish, jerr := a.startOrder(h, args)
	if jerr != nil {
		// The order intent could not be durably recorded, so nothing is sent; mark
		// the operation failed rather than leaving its ledger row at 'running'.
		h.Finish(outcomeFailed, "")
		return Result{}, jerr
	}

	_, data, meta, err := a.Raw.OrderPlace(ctx, req, korbit.Policy{})
	res := Result{Attempts: meta.Attempts}
	if err == nil {
		orderID := jsonNumberField(data, "orderId")
		log.Debug("order place: accepted (no-reconcile)", "clientOrderId", clientOrderID, "orderId", orderID)
		finish("accepted", orderID, "", meta.Attempts)
		res.JournalErr = finishOK(a, h)
		res.Data = data
		return res, nil
	}
	ae := apiErrOf(err)
	if korbit.Classify(err) == korbit.ClassTransient {
		log.Debug("order place: state UNKNOWN (no-reconcile, ambiguous single-shot failure, not verified)",
			"clientOrderId", clientOrderID, "code", codeOf(ae))
		finish("unknown", "", codeOf(ae), meta.Attempts)
		res.JournalErr = h.Finish(outcomeUnknown, codeOf(ae))
		return res, fmt.Errorf("order placement state is UNKNOWN (--no-reconcile: the single-shot send failed ambiguously and was not verified) — the order may have landed; verify with `%s order get --symbol %s --client-order-id %s` before retrying: %w", progname.Name(), args.symbol, clientOrderID, err)
	}
	log.Debug("order place: failed (no-reconcile, clean pre-execution rejection)",
		"clientOrderId", clientOrderID, "code", codeOf(ae))
	finish("failed", "", codeOf(ae), meta.Attempts)
	res.JournalErr = h.Finish(outcomeFailed, codeOf(ae))
	return res, err
}

// reconcileUnknownTyped is the retry-budget-exhausted endgame after AMBIGUOUS
// failures: read the order back by clientOrderId and report it placed (found)
// or UNKNOWN (not found).
func (a *API) reconcileUnknownTyped(ctx context.Context, h OpHandle, symbol, clientOrderID string, accountSeq int, ambiguous error, finish OrderFinishFunc, sends int) (Result, error) {
	log := a.log()
	doc, found, lerr := a.lookupOrderTyped(ctx, symbol, clientOrderID, accountSeq)
	res := Result{Attempts: sends}
	if found {
		orderID := jsonNumberField(doc, "orderId")
		// The ambiguous send DID land — the lookup recovered it. A reassuring
		// outcome worth recording so the preceding transport Warn isn't read as a
		// lost order.
		log.Debug("order place: ambiguous send recovered by lookup — order IS placed",
			"clientOrderId", clientOrderID, "orderId", orderID, "sends", sends)
		finish("accepted", orderID, "", sends)
		res.JournalErr = finishOK(a, h)
		res.Data = doc
		return res, nil
	}
	// Genuinely unknown: an ambiguous send AND no order read back. The order may
	// or may not exist — the one outcome a caller must verify by hand.
	log.Debug("order place: state UNKNOWN — ambiguous send and order not visible within the verify window",
		"clientOrderId", clientOrderID, "sends", sends, "lookupErr", errText(lerr))
	finish("unknown", "", "", sends)
	res.JournalErr = h.Finish(outcomeUnknown, "")
	cause := "no matching order was visible within the verify window"
	if lerr != nil {
		cause = fmt.Sprintf("the verification lookup also failed: %v", lerr)
	}
	res.Note = fmt.Sprintf("order placement state is UNKNOWN (the send failed ambiguously and %s) — the order may have landed or may never have reached the server; verify with `%s order get --symbol %s --client-order-id %s` before retrying", cause, progname.Name(), symbol, clientOrderID)
	return res, fmt.Errorf("%s: %w", res.Note, ambiguous)
}

// lookupOrderTyped fetches one order by clientOrderId via OrderGet,
// retrying an empty/not-found answer across ~1s of waiting because the order
// read path is eventually consistent.
func (a *API) lookupOrderTyped(ctx context.Context, symbol, clientOrderID string, accountSeq int) (json.RawMessage, bool, error) {
	req := rawapi.OrderGetRequest{
		Symbol:        rawapi.Symbol(symbol),
		ClientOrderID: &clientOrderID,
		AccountSeq:    &accountSeq,
	}
	log := a.log()
	var lastErr error
	for attempt := 0; attempt < lookupAttempts; attempt++ {
		if attempt > 0 {
			if serr := a.sleep(ctx, lookupRetryMs*time.Millisecond); serr != nil {
				return nil, false, serr
			}
		}
		_, data, _, err := a.Raw.OrderGet(ctx, req, korbit.Policy{Idempotent: true, BudgetMs: a.RetryBudgetMs})
		if err != nil {
			if isOrderNotFound(err) {
				trace(log, "order place: lookup not-found (read may be lagging, retrying)",
					"clientOrderId", clientOrderID, "attempt", attempt+1)
				lastErr = nil
				continue
			}
			trace(log, "order place: lookup error (retrying)",
				"clientOrderId", clientOrderID, "attempt", attempt+1, "err", err.Error())
			lastErr = err
			continue
		}
		if isEmptyDoc(data) {
			trace(log, "order place: lookup empty (read may be lagging, retrying)",
				"clientOrderId", clientOrderID, "attempt", attempt+1)
			lastErr = nil
			continue
		}
		log.Debug("order place: lookup found", "clientOrderId", clientOrderID, "attempts", attempt+1)
		return data, true, nil
	}
	log.Debug("order place: lookup exhausted — order not read back within the verify window",
		"clientOrderId", clientOrderID, "attempts", lookupAttempts, "lastErr", errText(lastErr))
	return nil, false, lastErr
}

// errText renders an error for a log attribute, "" when nil — so a "no error"
// case logs an empty value rather than the literal "<nil>".
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isOrderNotFound reports a definitive "no such order" answer.
func isOrderNotFound(err error) bool {
	ae := apiErrOf(err)
	if ae == nil {
		return false
	}
	return ae.HTTPStatus == 404 || ae.Code == "ORDER_NOT_FOUND" || ae.Code == "NOT_FOUND"
}

// isEmptyDoc reports a success answer that carries no order (null, {}, []).
func isEmptyDoc(data json.RawMessage) bool {
	switch string(bytes.TrimSpace(data)) {
	case "", "null", "{}", "[]", `{"success":true}`:
		return true
	}
	return false
}

// placeParamsJSON renders the typed place request as a JSON object in wire
// declaration order — the pre-signing parameter record the order journal stores
// for an order's intent.
func placeParamsJSON(req rawapi.OrderPlaceRequest) string {
	var p paramsCapture
	p.str("symbol", string(req.Symbol))
	p.str("side", string(req.Side))
	p.str("orderType", string(req.OrderType))
	p.strPtr("price", req.Price)
	p.strPtr("qty", req.Qty)
	p.strPtr("amt", req.Amt)
	if req.TimeInForce != nil {
		v := string(*req.TimeInForce)
		p.strPtr("timeInForce", &v)
	}
	p.intPtr("bestNth", req.BestNth)
	p.strPtr("clientOrderId", req.ClientOrderID)
	p.boolFlag("pp", req.PP)
	p.intPtr("ppPercent", req.PPPercent)
	p.intPtr("accountSeq", req.AccountSeq)
	return orderedJSON(p.kv)
}

// paramsCapture mirrors the wire layer's ordered-param builder so the order
// journal records the same pre-signing parameter object the placement sends.
type paramsCapture struct{ kv []korbit.KV }

func (p *paramsCapture) str(key, value string) {
	p.kv = append(p.kv, korbit.KV{Key: key, Value: value})
}
func (p *paramsCapture) strPtr(key string, value *string) {
	if value != nil {
		p.kv = append(p.kv, korbit.KV{Key: key, Value: *value})
	}
}
func (p *paramsCapture) intPtr(key string, value *int) {
	if value != nil {
		p.kv = append(p.kv, korbit.KV{Key: key, Value: strconv.Itoa(*value)})
	}
}
func (p *paramsCapture) boolFlag(key string, value *bool) {
	if value != nil {
		p.kv = append(p.kv, korbit.KV{Key: key, Value: strconv.FormatBool(*value)})
	}
}
