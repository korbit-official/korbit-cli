// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package rawapi

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
)

// captureDoer records the last outgoing request and replies with a fixed
// success envelope wrapping data.
type captureDoer struct {
	last *http.Request
	body string // captured request body (POST form), read before replying
	data string // the `data` payload to wrap in a success envelope
}

func (d *captureDoer) Do(req *http.Request) (*http.Response, error) {
	d.last = req
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		d.body = string(b)
	}
	envelope := `{"success":true,"data":` + d.data + `}`
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(envelope)),
	}, nil
}

// sentParams returns the encoded parameter string actually sent: the query
// string for GET/DELETE, the form body for POST.
func (d *captureDoer) sentParams(method string) string {
	if method == "POST" {
		return d.body
	}
	if d.last.URL.RawQuery != "" {
		return d.last.URL.RawQuery
	}
	return ""
}

// newSignedClient builds a typed Client whose wire client signs with a fresh
// keypair at a fixed timestamp and no recvWindow, against the capture doer.
func newSignedClient(t *testing.T, d *captureDoer) (*Client, ed25519.PublicKey) {
	t.Helper()
	kp, err := apiclient.GenerateKeypair()
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	priv, err := apiclient.ParsePrivatePEM(kp.PrivatePEM)
	if err != nil {
		t.Fatalf("parse priv: %v", err)
	}
	wire := &apiclient.Client{
		BaseURL: "https://api.example",
		Doer:    d,
		Creds:   &apiclient.Credentials{APIKeyID: "KID", Signer: apiclient.NewEd25519Signer(priv)},
		Clock:   fixedClock(1700000000000),
	}
	return New(wire, nil), publicKeyFromPEM(t, kp.PublicPEM)
}

// fixedClock is a deterministic apiclient.Clock for signing tests (no widening,
// zero offset).
type fixedClock int64

func (f fixedClock) SignNow() int64     { return int64(f) }
func (fixedClock) RecvWindowMs() int    { return 0 }
func (fixedClock) Offset() int64        { return 0 }
func (fixedClock) Measured() bool       { return true }
func (f fixedClock) ServerNowMs() int64 { return int64(f) }

func publicKeyFromPEM(t *testing.T, pemStr string) ed25519.PublicKey {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse public: %v", err)
	}
	return parsed.(ed25519.PublicKey)
}

// assertSignedLast verifies the sent string ends with timestamp then signature
// (signature LAST) and that the ED25519 signature verifies over the exact sent
// string minus the trailing &signature=... segment — the way the server checks.
func assertSignedLast(t *testing.T, sent string, pub ed25519.PublicKey) {
	t.Helper()
	idx := strings.LastIndex(sent, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature segment in %q", sent)
	}
	signedOver := sent[:idx]
	sigEnc := sent[idx+len("&signature="):]
	sig, err := url.QueryUnescape(sigEnc)
	if err != nil {
		t.Fatalf("unescape signature: %v", err)
	}
	if !strings.Contains(signedOver, "timestamp=1700000000000") {
		t.Errorf("timestamp not in signed portion: %q", signedOver)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pub, []byte(signedOver), sigBytes) {
		t.Fatalf("signature did not verify over the sent bytes %q", signedOver)
	}
}

// TestSigningWireBytes covers a representative signed endpoint per method:
// order place (POST), order get (GET), order cancel (DELETE). It asserts the
// param sequence is <spec-ordered params>&timestamp=...&signature=... with
// signature last and verifies the signature the way the server does.
func TestSigningWireBytes(t *testing.T) {
	ctx := context.Background()

	t.Run("place POST", func(t *testing.T) {
		d := &captureDoer{data: `{"orderId":42}`}
		c, pub := newSignedClient(t, d)
		price, qty := "100000000", "0.001"
		_, _, _, err := c.OrderPlace(ctx, OrderPlaceRequest{
			Symbol: "btc_krw", Side: SideBuy, OrderType: OrderTypeLimit,
			Price: &price, Qty: &qty,
		}, apiclient.Policy{})
		if err != nil {
			t.Fatalf("place: %v", err)
		}
		if d.last.Method != "POST" {
			t.Fatalf("method = %s, want POST", d.last.Method)
		}
		if d.last.URL.RawQuery != "" {
			t.Fatalf("POST must carry no query params, got %q", d.last.URL.RawQuery)
		}
		sent := d.sentParams("POST")
		const wantPrefix = "symbol=btc_krw&side=buy&orderType=limit&price=100000000&qty=0.001&timestamp=1700000000000&signature="
		if !strings.HasPrefix(sent, wantPrefix) {
			t.Fatalf("sent = %q\nwant prefix %q", sent, wantPrefix)
		}
		assertSignedLast(t, sent, pub)
	})

	t.Run("get GET", func(t *testing.T) {
		d := &captureDoer{data: `{"orderId":42}`}
		c, pub := newSignedClient(t, d)
		oid := 123456
		_, _, _, err := c.OrderGet(ctx, OrderGetRequest{Symbol: "btc_krw", OrderID: &oid}, apiclient.Policy{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if d.last.Method != "GET" {
			t.Fatalf("method = %s, want GET", d.last.Method)
		}
		sent := d.sentParams("GET")
		const wantPrefix = "symbol=btc_krw&orderId=123456&timestamp=1700000000000&signature="
		if !strings.HasPrefix(sent, wantPrefix) {
			t.Fatalf("sent = %q\nwant prefix %q", sent, wantPrefix)
		}
		assertSignedLast(t, sent, pub)
	})

	t.Run("cancel DELETE", func(t *testing.T) {
		d := &captureDoer{data: `null`}
		c, pub := newSignedClient(t, d)
		oid := 123456
		_, _, _, err := c.OrderCancel(ctx, OrderCancelRequest{Symbol: "btc_krw", OrderID: &oid}, apiclient.Policy{})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if d.last.Method != "DELETE" {
			t.Fatalf("method = %s, want DELETE", d.last.Method)
		}
		sent := d.sentParams("DELETE")
		const wantPrefix = "symbol=btc_krw&orderId=123456&timestamp=1700000000000&signature="
		if !strings.HasPrefix(sent, wantPrefix) {
			t.Fatalf("sent = %q\nwant prefix %q", sent, wantPrefix)
		}
		assertSignedLast(t, sent, pub)
	})
}

// TestOrderedParamsDeclarationOrder asserts a multi-param place request emits
// its KVs in spec declaration order and omits absent optionals.
func TestOrderedParamsDeclarationOrder(t *testing.T) {
	d := &captureDoer{data: `{"orderId":1}`}
	c, _ := newSignedClient(t, d)
	tif := TimeInForcePO
	bestNth := 1
	coid := "my-id-1"
	pp := true
	ppPct := 5
	seq := 2
	qty := "0.001"
	_, _, _, err := c.OrderPlace(context.Background(), OrderPlaceRequest{
		Symbol: "btc_krw", Side: SideSell, OrderType: OrderTypeBest,
		// Price omitted (absent optional) — must not appear.
		Qty: &qty, TimeInForce: &tif, BestNth: &bestNth,
		ClientOrderID: &coid, PP: &pp, PPPercent: &ppPct, AccountSeq: &seq,
	}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	sent := d.sentParams("POST")
	// Spec order: symbol, side, orderType, [price], qty, [amt], timeInForce,
	// bestNth, clientOrderId, pp, ppPercent, accountSeq — price/amt absent.
	const want = "symbol=btc_krw&side=sell&orderType=best&qty=0.001&timeInForce=po&bestNth=1&clientOrderId=my-id-1&pp=true&ppPercent=5&accountSeq=2&timestamp=1700000000000"
	idx := strings.LastIndex(sent, "&signature=")
	if idx < 0 {
		t.Fatalf("no signature in %q", sent)
	}
	if got := sent[:idx]; got != want {
		t.Fatalf("ordered params mismatch:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(sent, "price=") || strings.Contains(sent, "amt=") {
		t.Fatalf("absent optionals must be omitted, got %q", sent)
	}
}

// TestDecodeMarket feeds a representative public-array payload and asserts both
// the typed decode and the verbatim raw bytes.
func TestDecodeMarket(t *testing.T) {
	const data = `[{"symbol":"btc_krw","open":"1","high":"2","low":"0.5","close":"1.5","prevClose":"1","priceChange":"0.5","priceChangePercent":"50","volume":"10","quoteVolume":"15","bestBidPrice":"1.4","bestAskPrice":"1.6","lastTradedAt":1700000000000}]`
	d := &captureDoer{data: data}
	c := New(&apiclient.Client{BaseURL: "https://api.example", Doer: d}, nil)
	got, raw, _, err := c.Ticker(context.Background(), TickerRequest{}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("ticker: %v", err)
	}
	if len(got) != 1 || got[0].Symbol != "btc_krw" || got[0].Close != "1.5" || got[0].LastTradedAt != 1700000000000 {
		t.Fatalf("typed decode mismatch: %+v", got)
	}
	if string(raw) != data {
		t.Fatalf("raw bytes not verbatim:\n got %q\nwant %q", string(raw), data)
	}
}

// TestCandlesTimeBoundParamNames pins the candles endpoint's query-param names:
// it uses start/end (unlike the history endpoints' startTime/endTime). Sending
// the wrong names makes the server ignore the bound and always return the newest
// page, silently capping auto-paging and scroll-back backfill at one page.
func TestCandlesTimeBoundParamNames(t *testing.T) {
	d := &captureDoer{data: `[]`}
	c := New(&apiclient.Client{BaseURL: "https://api.example", Doer: d}, nil)
	start, end := 1_600_000_000_000, 1_700_000_000_000
	if _, _, _, err := c.Candles(context.Background(),
		CandlesRequest{Symbol: "btc_krw", Interval: "60", Limit: 200, StartTime: &start, EndTime: &end},
		apiclient.Policy{}); err != nil {
		t.Fatalf("candles: %v", err)
	}
	q := d.sentParams("GET")
	if !strings.Contains(q, "start=1600000000000") || !strings.Contains(q, "end=1700000000000") {
		t.Fatalf("candles must send start/end, got %q", q)
	}
	if strings.Contains(q, "startTime") || strings.Contains(q, "endTime") {
		t.Fatalf("candles must NOT send startTime/endTime (those are history params), got %q", q)
	}
	// limit is required (the endpoint rejects a missing/out-of-range limit with
	// BAD_REQUEST "limit out of range"), so every candles call must carry one.
	if !strings.Contains(q, "limit=200") {
		t.Fatalf("candles must always send limit, got %q", q)
	}
}

// TestDecodeTickSize pins that the tick-size endpoint's data payload is an
// ARRAY (one element per symbol), so the typed value is a slice. Decoding it
// into a single struct would silently fail (array into struct), leaving the
// typed view at its zero value while the verbatim bytes stay correct.
func TestDecodeTickSize(t *testing.T) {
	const data = `[{"symbol":"xrp_krw","tickSizePolicy":[{"priceGte":"0","tickSize":"0.0001"},{"priceGte":"1","tickSize":"0.001"}],"orderbookLevels":["0.1","1","10"]}]`
	d := &captureDoer{data: data}
	c := New(&apiclient.Client{BaseURL: "https://api.example", Doer: d}, nil)
	got, raw, _, err := c.TickSize(context.Background(), TickSizeRequest{Symbol: "xrp_krw"}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("ticksize: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 policy element, got %d (%+v)", len(got), got)
	}
	if got[0].Symbol != "xrp_krw" || len(got[0].TickSizePolicy) != 2 ||
		got[0].TickSizePolicy[1].TickSize != "0.001" || len(got[0].OrderbookLevels) != 3 {
		t.Fatalf("typed decode mismatch: %+v", got[0])
	}
	if string(raw) != data {
		t.Fatalf("raw bytes not verbatim:\n got %q\nwant %q", string(raw), data)
	}
}

// TestDecodeOrders covers the order-domain decode + verbatim raw.
func TestDecodeOrders(t *testing.T) {
	const data = `{"orderId":42,"clientOrderId":"abc","symbol":"btc_krw","orderType":"limit","side":"buy","timeInForce":"gtc","price":"100","qty":"0.001","filledQty":"0","filledAmt":"0","createdAt":1700000000000,"lastFilledAt":0,"status":"open"}`
	d := &captureDoer{data: data}
	c, _ := newSignedClient(t, d)
	oid := 42
	got, raw, _, err := c.OrderGet(context.Background(), OrderGetRequest{Symbol: "btc_krw", OrderID: &oid}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("order get: %v", err)
	}
	if got.OrderID != 42 || got.Status != "open" || got.Price != "100" || got.ClientOrderID != "abc" {
		t.Fatalf("typed decode mismatch: %+v", got)
	}
	if string(raw) != data {
		t.Fatalf("raw bytes not verbatim:\n got %q\nwant %q", string(raw), data)
	}
}

// TestDecodeAccount covers the account-domain decode + verbatim raw.
func TestDecodeAccount(t *testing.T) {
	const data = `[{"currency":"krw","balance":"1000","available":"800","tradeInUse":"200","withdrawalInUse":"0","avgPrice":"0"}]`
	d := &captureDoer{data: data}
	c, _ := newSignedClient(t, d)
	got, raw, _, err := c.Balance(context.Background(), BalanceRequest{}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if len(got) != 1 || got[0].Currency != "krw" || got[0].Available != "800" {
		t.Fatalf("typed decode mismatch: %+v", got)
	}
	if string(raw) != data {
		t.Fatalf("raw bytes not verbatim:\n got %q\nwant %q", string(raw), data)
	}
}

// TestDecodeFunding covers the funding-domain decode + verbatim raw, including a
// nested networkList in the currencies response (a judgment-call shape).
func TestDecodeFunding(t *testing.T) {
	const data = `[{"currency":"btc","withdrawableAmount":"1.5","withdrawalInUseAmount":"0.2"}]`
	d := &captureDoer{data: data}
	c, _ := newSignedClient(t, d)
	got, raw, _, err := c.WithdrawAmount(context.Background(), WithdrawAmountRequest{}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("withdraw amount: %v", err)
	}
	if len(got) != 1 || got[0].Currency != "btc" || got[0].WithdrawableAmount != "1.5" {
		t.Fatalf("typed decode mismatch: %+v", got)
	}
	if string(raw) != data {
		t.Fatalf("raw bytes not verbatim:\n got %q\nwant %q", string(raw), data)
	}
}

// TestCurrenciesNetworkListVerbatim asserts the per-network verbatim object is
// preserved alongside the common fields.
func TestCurrenciesNetworkListVerbatim(t *testing.T) {
	const data = `[{"name":"usdt","fullName":"Tether","withdrawalMaxAmountPerRequest":"100","withdrawalMinAmount":"1","defaultNetwork":"ETH","networkList":[{"name":"ETH","withdrawalStatus":"open","depositStatus":"open","confirmCount":12}]}]`
	d := &captureDoer{data: data}
	c := New(&apiclient.Client{BaseURL: "https://api.example", Doer: d}, nil)
	got, _, _, err := c.Currencies(context.Background(), CurrenciesRequest{}, apiclient.Policy{})
	if err != nil {
		t.Fatalf("currencies: %v", err)
	}
	if len(got) != 1 || len(got[0].NetworkList) != 1 {
		t.Fatalf("decode mismatch: %+v", got)
	}
	n := got[0].NetworkList[0]
	if n.Name != "ETH" || n.WithdrawalStatus != "open" {
		t.Fatalf("network common fields: %+v", n)
	}
	// The verbatim object retains fields not in the typed struct.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(n.Raw, &m); err != nil {
		t.Fatalf("raw network unmarshal: %v", err)
	}
	if _, ok := m["confirmCount"]; !ok {
		t.Fatalf("verbatim network object lost confirmCount: %s", n.Raw)
	}
}
