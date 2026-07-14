// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/output"
)

type stub struct {
	resp *http.Response
	err  error
	got  *http.Request
	body string
}

func (s *stub) Do(r *http.Request) (*http.Response, error) {
	s.got = r
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.body = string(b)
	}
	return s.resp, s.err
}

func mkResp(status int, body string, h map[string]string) *http.Response {
	hd := http.Header{}
	for k, v := range h {
		hd.Set(k, v)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: hd}
}

func testKey(t *testing.T) Credentials {
	t.Helper()
	kp, _ := GenerateKeypair()
	priv, err := ParsePrivatePEM(kp.PrivatePEM)
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{APIKeyID: "KEYID", Signer: NewEd25519Signer(priv)}
}

func TestBuildPOSTBodyAndHeaders(t *testing.T) {
	creds := testKey(t)
	built := Build(
		Request{Method: "POST", Path: "/v2/orders", Params: []KV{{"symbol", "btc_krw"}, {"side", "buy"}}, Auth: true},
		Options{BaseURL: "https://api.example", Creds: &creds, Now: func() int64 { return 1700000000000 }},
	)
	if strings.Contains(built.URL, "?") {
		t.Fatalf("POST must not carry a query string: %s", built.URL)
	}
	if built.Headers["content-type"] != "application/x-www-form-urlencoded" {
		t.Fatalf("missing form content-type")
	}
	if built.Headers["x-kapi-key"] != "KEYID" {
		t.Fatalf("missing api key header")
	}
	if !strings.Contains(built.Body, "&signature=") || !strings.HasPrefix(built.Body, "symbol=btc_krw&side=buy&timestamp=") {
		t.Fatalf("body shape wrong / signature not last: %s", built.Body)
	}
}

func TestExecuteUnwrapsData(t *testing.T) {
	d := &stub{resp: mkResp(200, `{"success":true,"data":{"a":1}}`, nil)}
	got, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("got %s", got)
	}
}

func TestExecuteNullDataNormalizes(t *testing.T) {
	for _, body := range []string{`{"success":true,"data":null}`, `{"success":true}`} {
		d := &stub{resp: mkResp(200, body, nil)}
		got, err := Execute(Request{Method: "DELETE", Path: "/v2/orders"}, Options{Doer: d})
		if err != nil || string(got) != `{"success":true}` {
			t.Fatalf("body %q -> %s, %v", body, got, err)
		}
	}
}

func TestExecuteFalsyDataPassesThrough(t *testing.T) {
	d := &stub{resp: mkResp(200, `{"success":true,"data":0}`, nil)}
	got, _ := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	if string(got) != `0` {
		t.Fatalf("falsy data should pass through, got %s", got)
	}
}

func TestExecute200ButSuccessFalseIsAPIError(t *testing.T) {
	d := &stub{resp: mkResp(200, `{"success":false,"error":{"code":422,"message":"BAD"}}`, nil)}
	_, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	var ae *output.ApiError
	if !errors.As(err, &ae) || ae.Code != "BAD" {
		t.Fatalf("want ApiError BAD, got %v", err)
	}
}

func TestExecuteMessageFallbackChain(t *testing.T) {
	// description present -> message = description
	d := &stub{resp: mkResp(422, `{"success":false,"error":{"message":"DUP","description":"already placed"}}`, nil)}
	_, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	var ae *output.ApiError
	errors.As(err, &ae)
	if ae.Message != "already placed" || ae.Code != "DUP" {
		t.Fatalf("desc case: %+v", ae)
	}
	// no description -> message = code
	d = &stub{resp: mkResp(422, `{"success":false,"error":{"message":"DUP"}}`, nil)}
	_, err = Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	errors.As(err, &ae)
	if ae.Message != "DUP" {
		t.Fatalf("code-as-message case: %q", ae.Message)
	}
	// no error object at all -> synthetic message, code empty
	d = &stub{resp: mkResp(500, `{"success":false}`, nil)}
	_, err = Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	errors.As(err, &ae)
	if ae.Code != "" || !strings.Contains(ae.Message, "HTTP 500 from GET /v2/x") {
		t.Fatalf("synthetic case: %+v", ae)
	}
}

func TestExecuteRetryAfter(t *testing.T) {
	// numeric header -> set
	d := &stub{resp: mkResp(429, `{"success":false,"error":{"message":"RATE"}}`, map[string]string{"Retry-After": "30"})}
	_, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	var ae *output.ApiError
	errors.As(err, &ae)
	if ae.RetryAfterSec == nil || *ae.RetryAfterSec != 30 {
		t.Fatalf("numeric retry-after not set: %+v", ae.RetryAfterSec)
	}
	// non-numeric (HTTP-date) -> absent
	d = &stub{resp: mkResp(429, `{"success":false,"error":{"message":"RATE"}}`, map[string]string{"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"})}
	_, err = Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	errors.As(err, &ae)
	if ae.RetryAfterSec != nil {
		t.Fatalf("date retry-after should be nil")
	}
}

func TestExecuteOversizedBodyTruncated(t *testing.T) {
	big := `{"success":false,"error":{"message":"X","description":"` + strings.Repeat("y", 5000) + `"}}`
	d := &stub{resp: mkResp(500, big, nil)}
	_, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	var ae *output.ApiError
	errors.As(err, &ae)
	if len(ae.Body) > 2100 { // 2048 cap + JSON-string quoting overhead
		t.Fatalf("body not truncated: %d bytes", len(ae.Body))
	}
}

func TestExecuteTransportErrorIsPlain(t *testing.T) {
	d := &stub{err: errors.New("dial tcp: connection refused")}
	_, err := Execute(Request{Method: "GET", Path: "/v2/x"}, Options{Doer: d})
	var ae *output.ApiError
	if errors.As(err, &ae) {
		t.Fatalf("transport failure must NOT be an ApiError (so it maps to exit 1)")
	}
	if !strings.Contains(err.Error(), "failed before a response was received") {
		t.Fatalf("unexpected: %v", err)
	}
}
