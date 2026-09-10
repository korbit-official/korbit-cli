// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/version"
)

// Doer performs an HTTP request. *http.Client satisfies it; tests inject a stub.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// KV is one ordered API parameter. Order is preserved through signing.
type KV struct {
	Key   string
	Value string
}

// Credentials sign a private request: the api-key id sent as X-KAPI-KEY and the
// Signer that produces the `signature` value (ED25519 or HMAC-SHA256).
type Credentials struct {
	APIKeyID string
	Signer   Signer
}

// Request is a logical API call before signing.
type Request struct {
	Method string
	Path   string
	Params []KV
	Auth   bool
}

// Options configures request building and execution.
type Options struct {
	BaseURL    string
	RecvWindow int // 0 = omit
	TimeoutMs  int
	Creds      *Credentials
	Doer       Doer
	Now        func() int64
	// UserAgent overrides the User-Agent header. Empty string keeps the default
	// ("digitalx-cli/<version>"). Composing a richer string (program version, OS,
	// Origin) is the CALLER's job at wiring time. This package gathers no OS
	// info; it only carries the seam.
	UserAgent string
}

// defaultUserAgent is the User-Agent used when none is supplied: the bare
// product/version token from internal/version, with no environment detail.
// Composing the richer value is the caller's job (see Options.UserAgent).
func defaultUserAgent() string { return version.Token() }

// Built is a fully-formed HTTP request, ready to send or display (dry-run).
type Built struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

func now(opts Options) int64 {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now().UnixMilli()
}

// Build forms the HTTP request, signing private calls the way Digital X verifies
// them: the ED25519 signature is computed over the exact url-encoded parameter
// string that is sent, with `signature` appended last. GET/DELETE carry params
// in the query string; POST carries them in a form-encoded body.
func Build(r Request, opts Options) Built {
	p := &orderedParams{}
	for _, kv := range r.Params {
		p.add(kv.Key, kv.Value)
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent()
	}
	headers := map[string]string{
		"accept":     "application/json",
		"user-agent": ua,
	}
	if r.Auth {
		signAuthParams(p, opts.Creds.Signer, now(opts), opts.RecvWindow)
		// A signed call with an empty api-key id is the keyless proof-of-possession
		// form (auto-claim's GET /v2/keys/claim): sign the query, but send no
		// X-KAPI-KEY. Every keyed call carries a non-empty id, so omitting the header
		// exactly when the id is empty selects the keyless case without a new flag.
		if opts.Creds.APIKeyID != "" {
			headers["x-kapi-key"] = opts.Creds.APIKeyID
		}
	}
	encoded := p.encode()
	if r.Method == "POST" {
		headers["content-type"] = "application/x-www-form-urlencoded"
		return Built{Method: r.Method, URL: opts.BaseURL + r.Path, Headers: headers, Body: encoded}
	}
	url := opts.BaseURL + r.Path
	if encoded != "" {
		url += "?" + encoded
	}
	return Built{Method: r.Method, URL: url, Headers: headers, Body: ""}
}

// signAuthParams appends the authentication parameters the server verifies, in
// the exact order it verifies them: the timestamp, the recvWindow (only when
// > 0 — omit-below-5s leaves the server's 5s default), then `signature` LAST,
// computed over the encoding of everything appended so far. It is the one
// signing core shared by REST request signing (Build) and the WebSocket upgrade
// (Client.SignHandshake), so the two can never drift in order or encoding.
func signAuthParams(p *orderedParams, signer Signer, nowMs int64, recvWindowMs int) {
	p.add("timestamp", strconv.FormatInt(nowMs, 10))
	if recvWindowMs > 0 {
		p.add("recvWindow", strconv.Itoa(recvWindowMs))
	}
	p.add("signature", signer.Sign(p.encode()))
}

// digitsRE matches an all-digits string (here: the Retry-After header value).
// The same trivial `^\d+$` appears in the validate and cli packages for their
// own value rules; they are deliberately not consolidated — a shared regex
// package would couple three unrelated packages over a one-line pattern.
var digitsRE = regexp.MustCompile(`^\d+$`)

// defaultTimeoutMs is the per-attempt HTTP timeout applied when none is set.
const defaultTimeoutMs = 15000

// Execute sends the request and unwraps the {success, data} envelope, returning
// the raw data bytes (field order preserved). A non-2xx or success:false
// response becomes an ApiError. It applies opts.TimeoutMs (default 15s) as a
// standalone deadline on a background context — see executeCtx for the variant
// that layers the per-attempt timeout onto a caller-supplied context.
func Execute(r Request, opts Options) (json.RawMessage, error) {
	return executeCtx(context.Background(), r, opts)
}

// executeCtx is Execute with a caller-supplied parent context: it derives a
// per-attempt deadline (opts.TimeoutMs, default 15s) layered on parent, so the
// effective deadline is the TIGHTER of the two and a parent cancellation aborts
// the in-flight request promptly. Execute passes context.Background() to
// reproduce its original standalone-timeout behavior byte-for-byte.
func executeCtx(parent context.Context, r Request, opts Options) (json.RawMessage, error) {
	return executeCtxLog(parent, r, opts, nil)
}

// executeCtxLog is executeCtx with an optional operational logger. It logs at
// Debug when a non-empty response body cannot be parsed as the {success,data}
// envelope (the common "server/proxy returned HTML or a truncated body"
// failure) — recording the body LENGTH only, never the body itself. log nil =
// silent.
func executeCtxLog(parent context.Context, r Request, opts Options, log *slog.Logger) (json.RawMessage, error) {
	lg := logging.Or(log)
	built := Build(r, opts)
	timeout := opts.TimeoutMs
	if timeout <= 0 {
		timeout = defaultTimeoutMs
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	var bodyReader io.Reader
	if built.Body != "" {
		bodyReader = strings.NewReader(built.Body)
	}
	req, err := http.NewRequestWithContext(ctx, built.Method, built.URL, bodyReader)
	if err != nil {
		return nil, err
	}
	for k, v := range built.Headers {
		req.Header.Set(k, v)
	}

	doer := opts.Doer
	if doer == nil {
		doer = http.DefaultClient
	}
	res, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s failed before a response was received: %v", r.Method, r.Path, err)
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)

	var top map[string]json.RawMessage
	if len(text) > 0 {
		if err := json.Unmarshal(text, &top); err != nil || top == nil {
			// The body is not the expected JSON envelope (HTML proxy/error page,
			// truncated body, …). Log the LENGTH and status — never the body — so a
			// --debug run can tell a "server returned garbage" failure apart from a
			// real API rejection.
			lg.Debug("response not a JSON envelope",
				"method", r.Method, "path", r.Path,
				"status", res.StatusCode, "bodyLen", len(text))
		}
	}

	success := true
	if raw, ok := top["success"]; ok {
		_ = json.Unmarshal(raw, &success)
	}
	httpOK := res.StatusCode >= 200 && res.StatusCode < 300

	if !httpOK || top == nil || !success {
		return nil, apiError(r, res, text, top)
	}

	data, ok := top["data"]
	if !ok || string(data) == "null" {
		return json.RawMessage(`{"success":true}`), nil
	}
	return data, nil
}

func apiError(r Request, res *http.Response, text []byte, top map[string]json.RawMessage) error {
	var code, description string
	if errRaw, ok := top["error"]; ok {
		var errObj struct {
			Message     string `json:"message"`
			Description string `json:"description"`
		}
		if json.Unmarshal(errRaw, &errObj) == nil {
			code = errObj.Message
			description = errObj.Description
		}
	}

	var retryAfter *int
	if h := res.Header.Get("Retry-After"); digitsRE.MatchString(h) {
		if n, err := strconv.Atoi(h); err == nil {
			retryAfter = &n
		}
	}

	message := description
	if message == "" {
		message = code
	}
	if message == "" {
		message = fmt.Sprintf("HTTP %d from %s %s", res.StatusCode, r.Method, r.Path)
	}

	// Cap the diagnostic body so a hostile or oversized response can't flood the
	// agent's stderr/context. A small JSON body is kept verbatim (preserving
	// shape); anything else (non-JSON, or an oversized body) becomes a truncated
	// JSON string.
	const maxBody = 2048
	var body json.RawMessage
	if top != nil && len(text) <= maxBody {
		body = json.RawMessage(text)
	} else {
		b, _ := json.Marshal(truncate(string(text), maxBody))
		body = b
	}

	return &output.ApiError{
		Message:       message,
		HTTPStatus:    res.StatusCode,
		Code:          code,
		RetryAfterSec: retryAfter,
		Body:          body,
	}
}

// hostOf returns the host[:port] of a base URL for logging, or the raw string
// when it can't be parsed. It carries no query/userinfo, so it never leaks a
// signed query string or credential.
func hostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	return u.Host
}

// truncate caps s at n bytes without splitting a multi-byte rune (so the result
// stays valid UTF-8 and json.Marshal never has to emit U+FFFD).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back off to the start of the rune that straddles the cut point.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
