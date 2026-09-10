// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/callrec"
	"github.com/korbit-official/korbit-cli/internal/clock"
	"github.com/korbit-official/korbit-cli/internal/journal"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func mcpFixedNow() int64 { return 1700000000000 }

// mcpHTTPStub records every request and returns a routed response.
type mcpHTTPStub struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []string
	route  func(r *http.Request) (*http.Response, error)
}

func (d *mcpHTTPStub) Do(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, r)
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	d.bodies = append(d.bodies, body)
	d.mu.Unlock()
	return d.route(r)
}

func (d *mcpHTTPStub) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

func okJSON(body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
}

// testMCPCmd builds a cobra command carrying the flags runMCP/resolveBaseURL
// consult, so handlers run without the full tree.
func testMCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mcp"}
	f := cmd.Flags()
	f.Bool("read-only", false, "")
	f.Bool("multi-key", false, "")
	f.String("base-url", "", "")
	f.String("timeout", "", "")
	f.String("retry-timeout", "", "")
	return cmd
}

// buildTestMCP assembles an mcpServer against a temp home and a stub Doer,
// targeting a loopback base URL (http, so signed calls are allowed). When
// key != "" a bound key of that name is seeded. retryBudget is 0 so the place
// protocol takes no budgeted sleeps under test.
func buildTestMCP(t *testing.T, doer apiclient.Doer, key string, multiKey bool) (*mcpServer, func()) {
	t.Helper()
	home := t.TempDir()
	if key != "" {
		m := keys.NewManager(home, "file", mcpFixedNow, nil)
		if _, err := m.Add(key, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := m.Bind(key, "KEYID-1"); err != nil {
			t.Fatal(err)
		}
	}
	rt := &runtime{
		deps: resolveDeps(Deps{
			Getenv: func(k string) string {
				switch k {
				case "DIGITALX_CLI_HOME":
					return home
				case "DIGITALX_CLI_BASE_URL":
					return "http://127.0.0.1:9999"
				}
				return ""
			},
			Doer:  doer,
			Now:   mcpFixedNow,
			Sleep: func(time.Duration) {},
			// Keep the setup tool's allowlist probe off the network in tests; an
			// error makes probe.IPs return empty, so setup just omits the allowlist.
			IPProbe: func(context.Context, string, string, string, int) (string, error) {
				return "", fmt.Errorf("no probe in tests")
			},
		}),
		io:  output.IO{Out: io.Discard, Err: io.Discard},
		key: key,
	}
	cmd := testMCPCmd()
	home2, cfg, err := rt.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	km := rt.KeyManager(home2, cfg)
	baseURL, _ := rt.resolveBaseURL(cmd, cfg, "")
	keyNames, _ := km.Names()
	rec := callrec.New(journal.DefaultPath(home2), journal.Disabled(rt.deps.Getenv), false,
		callrec.DefaultPolicy(false), rt.deps.Now, func(callrec.FailMode, error) {})
	srv := &mcpServer{
		rt: rt, cmd: cmd, cfg: cfg, km: km, rec: rec, clk: clock.New(rt.deps.Now),
		errW:          &syncWriter{w: io.Discard},
		home:          home2,
		publicBaseURL: baseURL, launchKey: key, multiKey: multiKey,
		timeoutMs: 15000, retryBudgetMs: 0, keyNames: keyNames,
		skillFS: testSkillFS(), apis: map[string]*keyAPI{},
	}
	return srv, func() { rec.Close() }
}

// testSkillFS is a minimal stand-in for the embedded Agent Skill: a SKILL.md
// (with frontmatter, to exercise the strip) plus one reference, so the
// korbit_guide tool registers with a discoverable topic.
func testSkillFS() fstest.MapFS {
	return fstest.MapFS{
		"SKILL.md":                 {Data: []byte("---\nname: korbit\n---\n\n# Operating Korbit\n\nbody\n")},
		"references/monitoring.md": {Data: []byte("# Monitoring\n\nplaybook\n")},
	}
}

func cmdByKey(t *testing.T, key string) surfaceCmd {
	t.Helper()
	c := findSurface(strings.Fields(key))
	if c == nil {
		t.Fatalf("command %q not found", key)
	}
	return *c
}

// endpointSurface returns the endpoint commands in the unified surface, for the
// generation guards that iterate every tool-backed command.
func endpointSurface() []surfaceCmd {
	var out []surfaceCmd
	for _, c := range commandSurface() {
		if c.IsEndpoint() {
			out = append(out, c)
		}
	}
	return out
}

// callTool drives one tool handler directly and asserts no protocol-level error.
func callTool(t *testing.T, srv *mcpServer, c surfaceCmd, multiKey bool, argsJSON string) *mcp.CallToolResult {
	t.Helper()
	h := srv.makeToolHandler(c, multiKey)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: toolName(c), Arguments: json.RawMessage(argsJSON)}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: unexpected protocol error: %v", toolName(c), err)
	}
	return res
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// --- generation tests -------------------------------------------------------

func TestMCPToolNames(t *testing.T) {
	cases := map[string]string{
		"ticker": "ticker", "order place": "order_place", "order cancel": "order_cancel",
		"krw deposit history": "krw_deposit_history", "krw withdraw request": "krw_withdraw_request",
		"withdraw request": "withdraw_request", "fills": "fills",
	}
	for key, want := range cases {
		c := cmdByKey(t, key)
		if got := toolName(c); got != want {
			t.Errorf("toolName(%q) = %q, want %q", key, got, want)
		}
	}
	// Names must be unique across all endpoint commands.
	seen := map[string]string{}
	for _, c := range endpointSurface() {
		n := toolName(c)
		if prev, dup := seen[n]; dup {
			t.Errorf("duplicate tool name %q (%s and %s)", n, prev, c.Key())
		}
		seen[n] = c.Key()
	}
}

func TestMCPInputSchemaValidAndCoversParams(t *testing.T) {
	for _, c := range endpointSurface() {
		var schema struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties bool                       `json:"additionalProperties"`
			Required             []string                   `json:"required"`
		}
		if err := json.Unmarshal(inputSchema(c, false, nil), &schema); err != nil {
			t.Fatalf("%s: invalid schema json: %v", c.Key(), err)
		}
		if schema.Type != "object" {
			t.Errorf("%s: schema type = %q", c.Key(), schema.Type)
		}
		if schema.AdditionalProperties {
			t.Errorf("%s: additionalProperties must be false", c.Key())
		}
		for _, p := range c.Params {
			if _, ok := schema.Properties[p.API]; !ok {
				t.Errorf("%s: schema missing property %q", c.Key(), p.API)
			}
		}
		for _, ps := range c.Positionals {
			if _, ok := schema.Properties[ps.API]; !ok {
				t.Errorf("%s: schema missing positional property %q", c.Key(), ps.API)
			}
		}
	}
}

func TestMCPMultiKeyInjectsKeyArg(t *testing.T) {
	hasKeyArg := func(raw json.RawMessage) bool {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		_, ok := schema.Properties[mcpKeyArg]
		return ok
	}
	bal := cmdByKey(t, "balance")
	if !hasKeyArg(inputSchema(bal, true, []string{"a", "b"})) {
		t.Errorf("multi-key authenticated tool must expose %q", mcpKeyArg)
	}
	if hasKeyArg(inputSchema(bal, false, []string{"a"})) {
		t.Errorf("single-key tool must not expose %q", mcpKeyArg)
	}
	// A public tool never gets the key arg, even under multi-key.
	if hasKeyArg(inputSchema(cmdByKey(t, "ticker"), true, []string{"a"})) {
		t.Errorf("public tool must not expose %q", mcpKeyArg)
	}
}

func TestMCPAnnotations(t *testing.T) {
	get := toolAnnotations(cmdByKey(t, "ticker"))
	if !get.ReadOnlyHint {
		t.Error("ticker must be read-only")
	}
	place := toolAnnotations(cmdByKey(t, "order place"))
	if place.ReadOnlyHint {
		t.Error("order place must not be read-only")
	}
	if place.DestructiveHint == nil || !*place.DestructiveHint {
		t.Error("order place must be destructive")
	}
	cancel := toolAnnotations(cmdByKey(t, "order cancel"))
	if !cancel.IdempotentHint {
		t.Error("order cancel is idempotent (Safety idempotent)")
	}
	if cancel.DestructiveHint == nil || !*cancel.DestructiveHint {
		t.Error("order cancel must be destructive")
	}
}

func TestMCPBuildToolSetAndReadOnly(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{"success":true,"data":{}}`)}, "", false)
	defer done()
	srv.build(false, false)
	full := srv.toolCount
	srv.build(true, false)
	ro := srv.toolCount
	if ro >= full {
		t.Fatalf("read-only (%d) should expose fewer tools than full (%d)", ro, full)
	}
	// full = every endpoint command + the five hand-registered non-endpoint tools
	// (list_keys, bot_runtime_reference, korbit_guide, setup, doctor).
	// The onboarding tools (setup/doctor) and the read-only doc tools are exposed in
	// both modes — they act on the local keystore or return text, not the exchange —
	// so read-only only drops non-GET endpoints.
	endpoints := len(endpointSurface())
	if full != endpoints+5 {
		t.Fatalf("full tool count = %d, want %d (endpoints + list_keys + bot_runtime_reference + korbit_guide + setup + doctor)", full, endpoints+5)
	}
}

// --- handler behavior -------------------------------------------------------

func TestMCPPublicReadDoesNotSign(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"last":"100","symbol":"btc_krw"}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "ticker"), false, `{"symbol":"btc_krw"}`)
	if res.IsError {
		t.Fatalf("ticker errored: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), `"last":"100"`) {
		t.Fatalf("unexpected result: %s", resultText(res))
	}
	if doer.reqs[0].Header.Get("x-kapi-key") != "" {
		t.Fatal("public ticker must not be signed")
	}
}

func TestMCPSignedReadSigns(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"krw":{"available":"1000"}}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "balance"), false, `{}`)
	if res.IsError {
		t.Fatalf("balance errored: %s", resultText(res))
	}
	if doer.reqs[0].Header.Get("x-kapi-key") != "KEYID-1" {
		t.Fatal("balance must be signed with the bound key")
	}
	// A JSON object response is mirrored into structuredContent.
	if res.StructuredContent == nil {
		t.Fatal("object response should populate structuredContent")
	}
}

func TestMCPInlineCredentialDoesNotInheritStoredDefaultAccountSeq(t *testing.T) {
	home := t.TempDir()
	km := keys.NewManager(home, "file", mcpFixedNow, nil)
	if _, err := km.Add("bot", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := km.Bind("bot", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	if err := km.SetDefaultAccountSeq("bot", 7); err != nil {
		t.Fatal(err)
	}
	bal := cmdByKey(t, "balance")

	inlineSrv := &mcpServer{km: km, sel: keys.Selection{Inline: true}}
	params, err := parseToolArgs(bal, map[string]json.RawMessage{}, inlineSrv.accountSeqDefault(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := params["accountSeq"]; got != "1" {
		t.Fatalf("inline launch credential must default accountSeq to 1, got %q", got)
	}

	storedSrv := &mcpServer{km: km, sel: keys.Selection{}}
	params, err = parseToolArgs(bal, map[string]json.RawMessage{}, storedSrv.accountSeqDefault(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := params["accountSeq"]; got != "7" {
		t.Fatalf("stored launch key must inherit its default accountSeq, got %q", got)
	}
}

func TestMCPOrderPlaceMintsAndSendsClientOrderID(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"orderId":123,"status":"open"}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "order place"), false,
		`{"symbol":"btc_krw","side":"buy","orderType":"limit","price":"100000000","qty":"0.001"}`)
	if res.IsError {
		t.Fatalf("order place errored: %s", resultText(res))
	}
	// The POST body must carry an auto-minted clientOrderId and an explicit
	// accountSeq default.
	sentClientOrderID := false
	sentAccountSeq := false
	doer.mu.Lock()
	for i, r := range doer.reqs {
		if r.Method == "POST" && strings.Contains(doer.bodies[i], "clientOrderId") {
			sentClientOrderID = true
		}
		if r.Method == "POST" && strings.Contains(doer.bodies[i], "accountSeq=1") {
			sentAccountSeq = true
		}
	}
	doer.mu.Unlock()
	if !sentClientOrderID {
		t.Fatal("order place must mint and send a clientOrderId")
	}
	if !sentAccountSeq {
		t.Fatal("order place must send the default accountSeq")
	}
}

func TestMCPOrderPlaceDryRunPreviewsWithoutPlacing(t *testing.T) {
	// A thin book: a market buy sweeps it to a far-worse average -> HIGH_SLIPPAGE.
	book := `{"success":true,"data":{"timestamp":1,"bids":[{"price":"9900","qty":"5"}],` +
		`"asks":[{"price":"10000","qty":"1"},{"price":"20000","qty":"5"}]}}`
	doer := &mcpHTTPStub{route: okJSON(book)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "order place"), false,
		`{"symbol":"btc_krw","side":"buy","orderType":"market","amt":"15000","dryRun":true}`)
	if res.IsError {
		t.Fatalf("dry-run errored: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, `"dryRun":true`) || !strings.Contains(out, "simulation") {
		t.Fatalf("dry-run should return a simulation: %s", out)
	}
	if !strings.Contains(out, "HIGH_SLIPPAGE") {
		t.Fatalf("dry-run should warn on slippage: %s", out)
	}
	// It must place nothing: only the public market-data GET(s), never an order POST.
	doer.mu.Lock()
	defer doer.mu.Unlock()
	for _, r := range doer.reqs {
		if r.Method == "POST" {
			t.Fatalf("dry-run must not place an order (saw POST %s)", r.URL.Path)
		}
	}
}

func TestMCPOrderPlaceDryRunWithInlineCredentialSkipsStoredDefaultBaseURL(t *testing.T) {
	home := t.TempDir()
	m := keys.NewManager(home, "file", mcpFixedNow, nil)
	if _, err := m.Add("stored-default", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind("stored-default", "KEYID-1"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBaseURL("stored-default", "http://127.0.0.1:7777", ""); err != nil {
		t.Fatal(err)
	}

	book := `{"success":true,"data":{"timestamp":1,"bids":[{"price":"9900","qty":"5"}],` +
		`"asks":[{"price":"10000","qty":"1"},{"price":"20000","qty":"5"}]}}`
	doer := &mcpHTTPStub{route: okJSON(book)}
	rt := &runtime{
		deps: resolveDeps(Deps{
			Getenv: func(k string) string {
				switch k {
				case "DIGITALX_CLI_HOME":
					return home
				case "DIGITALX_CLI_BASE_URL":
					return "http://127.0.0.1:9999"
				case keys.EnvAPIKeyID:
					return "INLINE-KEY"
				case keys.EnvAPIKeySecret:
					return "secret"
				case keys.EnvAPIKeyType:
					return keys.TypeHMACSHA256
				}
				return ""
			},
			Doer:  doer,
			Now:   mcpFixedNow,
			Sleep: func(time.Duration) {},
		}),
		io: output.IO{Out: io.Discard, Err: io.Discard},
	}
	cmd := testMCPCmd()
	home2, cfg, err := rt.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	km := rt.KeyManager(home2, cfg)
	baseURL, _ := rt.resolveBaseURL(cmd, cfg, "")
	rec := callrec.New(journal.DefaultPath(home2), journal.Disabled(rt.deps.Getenv), false,
		callrec.DefaultPolicy(false), rt.deps.Now, func(callrec.FailMode, error) {})
	defer rec.Close()
	srv := &mcpServer{
		rt: rt, cmd: cmd, cfg: cfg, km: km, rec: rec, clk: clock.New(rt.deps.Now),
		errW:          &syncWriter{w: io.Discard},
		home:          home2,
		publicBaseURL: baseURL, sel: keys.Selection{Inline: true}, multiKey: false,
		timeoutMs: 15000, retryBudgetMs: 0, apis: map[string]*keyAPI{},
	}

	res := callTool(t, srv, cmdByKey(t, "order place"), false,
		`{"symbol":"btc_krw","side":"buy","orderType":"market","amt":"15000","dryRun":true}`)
	if res.IsError {
		t.Fatalf("dry-run errored: %s", resultText(res))
	}
	doer.mu.Lock()
	defer doer.mu.Unlock()
	if len(doer.reqs) == 0 {
		t.Fatal("dry-run should fetch public market data")
	}
	for _, r := range doer.reqs {
		if r.URL.Host != "127.0.0.1:9999" {
			t.Fatalf("inline dry-run used host %q; want 127.0.0.1:9999", r.URL.Host)
		}
	}
}

func TestMCPDryRunArgOnlyOnOrderPlace(t *testing.T) {
	if !strings.Contains(string(inputSchema(cmdByKey(t, "order place"), false, nil)), `"dryRun"`) {
		t.Fatal("the order_place tool schema must expose the dryRun preview flag")
	}
	for _, c := range endpointSurface() {
		if c.Key() == cmdPlace {
			continue
		}
		if strings.Contains(string(inputSchema(c, false, nil)), `"dryRun"`) {
			t.Fatalf("%s must not expose dryRun (it bloats the schema for no benefit)", toolName(c))
		}
	}
}

func TestMCPDecimalAsNumberRejected(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	// price as a JSON number, not a string.
	res := callTool(t, srv, cmdByKey(t, "order place"), false,
		`{"symbol":"btc_krw","side":"buy","orderType":"limit","price":100000000,"qty":"0.001"}`)
	if !res.IsError {
		t.Fatal("a JSON-number price must be rejected")
	}
	if !strings.Contains(resultText(res), "decimal") {
		t.Fatalf("error should mention decimal strings: %s", resultText(res))
	}
	if doer.count() != 0 {
		t.Fatal("nothing should be sent when validation fails")
	}
}

func TestMCPUnknownArgumentRejected(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{}`)}, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "ticker"), false, `{"bogus":"x"}`)
	if !res.IsError || !strings.Contains(resultText(res), "unknown argument") {
		t.Fatalf("expected unknown-argument error, got: %s", resultText(res))
	}
}

func TestMCPKeyArgWithoutMultiKeyIsError(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false) // multiKey = false
	defer done()
	res := callTool(t, srv, cmdByKey(t, "balance"), false, `{"key":"bot"}`)
	if !res.IsError {
		t.Fatal("a key argument without --multi-key must error")
	}
	if !strings.Contains(resultText(res), "--multi-key") {
		t.Fatalf("error should point at --multi-key: %s", resultText(res))
	}
	if doer.count() != 0 {
		t.Fatal("nothing should be sent when the key arg is rejected")
	}
}

func TestMCPMultiKeyUnknownKeyIsError(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{"success":true,"data":{}}`)}, "bot", true)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "balance"), true, `{"key":"nope"}`)
	if !res.IsError || !strings.Contains(resultText(res), "unknown key") {
		t.Fatalf("expected unknown-key error, got: %s", resultText(res))
	}
}

// A public tool never accepts a key arg, even under --multi-key: it is not in
// the schema, so it must be rejected as an unknown argument (not silently used).
func TestMCPPublicToolRejectsKeyArgUnderMultiKey(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"last":"100"}}`)}
	srv, done := buildTestMCP(t, doer, "bot", true) // multiKey = true
	defer done()
	res := callTool(t, srv, cmdByKey(t, "ticker"), true, `{"symbol":"btc_krw","key":"bot"}`)
	if !res.IsError || !strings.Contains(resultText(res), "unknown argument") {
		t.Fatalf("public tool must reject a key arg as unknown, got: %s", resultText(res))
	}
	if doer.count() != 0 {
		t.Fatal("nothing should be sent when the arg is rejected")
	}
}

// With no configured keys, --multi-key must still emit valid JSON Schema: the
// injected key arg's enum is an empty array, never null.
func TestMCPMultiKeyEmptyKeyNamesSchemaValid(t *testing.T) {
	bal := cmdByKey(t, "balance")
	raw := inputSchema(bal, true, nil)
	if strings.Contains(string(raw), `"enum":null`) {
		t.Fatalf("enum must not be null: %s", raw)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema must remain valid JSON: %v", err)
	}
	if _, ok := schema.Properties[mcpKeyArg]; !ok {
		t.Fatal("multi-key authenticated tool must still expose the key arg")
	}
}

func TestMCPApiErrorBecomesIsErrorWithCode(t *testing.T) {
	doer := &mcpHTTPStub{route: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 422,
			Body:   io.NopCloser(strings.NewReader(`{"success":false,"error":{"code":422,"message":"NO_BALANCE"}}`)),
			Header: http.Header{}}, nil
	}}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callTool(t, srv, cmdByKey(t, "balance"), false, `{}`)
	if !res.IsError {
		t.Fatal("an API rejection must surface as IsError")
	}
	if !strings.Contains(resultText(res), "NO_BALANCE") {
		t.Fatalf("error should carry the symbolic code: %s", resultText(res))
	}
}

func TestMCPMissingKeyAuthErrorsButPublicWorks(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"last":"100"}}`)}
	srv, done := buildTestMCP(t, doer, "", false) // no key seeded
	defer done()
	// Authenticated tool: clear error pointing at doctor.
	res := callTool(t, srv, cmdByKey(t, "balance"), false, `{}`)
	if !res.IsError || !strings.Contains(resultText(res), "doctor") {
		t.Fatalf("missing-key authed tool should error and point at doctor: %s", resultText(res))
	}
	// Public tool still works without a key.
	pub := callTool(t, srv, cmdByKey(t, "ticker"), false, `{"symbol":"btc_krw"}`)
	if pub.IsError {
		t.Fatalf("public tool should work without a key: %s", resultText(pub))
	}
}

func TestMCPListKeysToolListsConfigured(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{}`)}, "bot", false)
	defer done()
	h := srv.makeListKeysHandler()
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_keys"}})
	if err != nil || res.IsError {
		t.Fatalf("list_keys failed: %v / %s", err, resultText(res))
	}
	if !strings.Contains(resultText(res), `"bot"`) {
		t.Fatalf("list_keys should list the configured key: %s", resultText(res))
	}
	if res.StructuredContent == nil {
		t.Fatal("list_keys should provide structuredContent")
	}
}

func TestMCPBotRuntimeReferenceTool(t *testing.T) {
	h := makeBotRuntimeReferenceHandler()
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: botRuntimeReferenceName}})
	if err != nil || res.IsError {
		t.Fatalf("bot_runtime_reference failed: %v / %s", err, resultText(res))
	}
	text := resultText(res)
	// The reference is rendered from the monitor spec entry: it must describe the
	// ta library and the JavaScript hooks, and must not leak the {prog} placeholder.
	for _, want := range []string{"ta.stream", "--where", "--on", "monitor"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bot_runtime_reference missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, progPlaceholder) {
		t.Fatalf("bot_runtime_reference leaked the %q placeholder", progPlaceholder)
	}
	// It makes no API call — purely local documentation, so no signing key needed.
}

func TestMCPKorbitGuideTool(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{}`)}, "", false)
	defer done()
	h := srv.makeGuideHandler()

	// No topic → the SKILL.md overview, with its YAML frontmatter stripped.
	res := callNamedTool(t, h, korbitGuideName, `{}`)
	if res.IsError {
		t.Fatalf("korbit_guide overview errored: %s", resultText(res))
	}
	overview := resultText(res)
	if !strings.Contains(overview, "# Operating Korbit") {
		t.Fatalf("overview missing body:\n%s", overview)
	}
	if strings.Contains(overview, "name: korbit") || strings.HasPrefix(overview, "---") {
		t.Fatalf("overview should have frontmatter stripped:\n%s", overview)
	}

	// A known topic → that reference verbatim.
	res = callNamedTool(t, h, korbitGuideName, `{"topic":"monitoring"}`)
	if res.IsError {
		t.Fatalf("korbit_guide topic errored: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "# Monitoring") {
		t.Fatalf("monitoring topic missing body:\n%s", resultText(res))
	}

	// An unknown topic → a recoverable error naming the valid topics (the handler
	// is the guard; the SDK does not validate against the schema enum).
	res = callNamedTool(t, h, korbitGuideName, `{"topic":"nope"}`)
	if !res.IsError {
		t.Fatalf("unknown topic should error, got: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "monitoring") {
		t.Fatalf("unknown-topic error should list valid topics:\n%s", resultText(res))
	}
}

func TestKorbitGuideBuildersZeroTopics(t *testing.T) {
	// With no topics (no skill embedded), the schema must still be valid JSON with
	// no `topic` property, and the description must omit the "Topics:" suffix.
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(korbitGuideSchema(nil), &schema); err != nil {
		t.Fatalf("zero-topic schema is invalid json: %v", err)
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatalf("unexpected schema: %+v", schema)
	}
	if _, ok := schema.Properties[mcpGuideTopicArg]; ok {
		t.Fatal("zero-topic schema should not advertise a topic property")
	}
	if strings.Contains(korbitGuideDesc(nil), "Topics:") {
		t.Fatal("zero-topic description should omit the Topics suffix")
	}
}

// --- onboarding & diagnostics tools (setup, doctor) -------------------------

func callNamedTool(t *testing.T, h mcp.ToolHandler, name, argsJSON string) *mcp.CallToolResult {
	t.Helper()
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(argsJSON)}}
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: unexpected protocol error: %v", name, err)
	}
	return res
}

func TestMCPSetupOnboarding(t *testing.T) {
	// No key seeded: the setup tool bootstraps one entirely in-chat.
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{}`)}, "", false)
	defer done()

	// setup → generates a local keypair, returns a registration link + next steps.
	res := callNamedTool(t, srv.makeSetupHandler(), "setup", `{"name":"trading"}`)
	if res.IsError {
		t.Fatalf("setup errored: %s", resultText(res))
	}
	var setupDoc struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		PublicKey        string `json:"publicKey"`
		RegistrationLink string `json:"registrationLink"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &setupDoc); err != nil {
		t.Fatalf("setup result json: %v\n%s", err, resultText(res))
	}
	if setupDoc.Name != "trading" || setupDoc.Status != "created" || setupDoc.RegistrationLink == "" {
		t.Fatalf("unexpected setup doc: %+v", setupDoc)
	}
	if !strings.Contains(setupDoc.PublicKey, "PUBLIC KEY") {
		t.Fatalf("setup should return the public key, got %q", setupDoc.PublicKey)
	}
	if res.StructuredContent == nil {
		t.Fatal("setup should provide structuredContent")
	}

	// The key now exists locally; re-running setup resumes (does not error).
	again := callNamedTool(t, srv.makeSetupHandler(), "setup", `{"name":"trading"}`)
	if again.IsError {
		t.Fatalf("re-running setup should resume, not error: %s", resultText(again))
	}

	// setup refreshed the in-memory registry: the new key (the server launched
	// with none) is now selectable, so --multi-key validation would accept it.
	if !srv.knownKey("trading") {
		t.Fatal("setup should refresh keyNames so the new key is selectable without a restart")
	}

	// Simulate a pre-bind authenticated call that cached a failed launch-slot
	// resolution; without the post-bind cache refresh this would stick forever.
	srv.mu.Lock()
	srv.apis[""] = &keyAPI{err: "stale pre-bind failure"}
	srv.mu.Unlock()

	// setup again, now with apiKey → binds the portal-issued id (the one-step
	// `setup --api-key` flow; no separate key_bind tool).
	bind := callNamedTool(t, srv.makeSetupHandler(), "setup", `{"name":"trading","apiKey":"KEYID-XYZ"}`)
	if bind.IsError {
		t.Fatalf("setup --api-key errored: %s", resultText(bind))
	}
	if !strings.Contains(resultText(bind), "KEYID-XYZ") {
		t.Fatalf("setup --api-key should echo the bound api-key id: %s", resultText(bind))
	}

	// No restart: the launch slot now resolves the just-bound default key, and
	// the stale cached failure is gone.
	ka := srv.apiForKey("")
	if ka.err != "" {
		t.Fatalf("launch slot should resolve the bound key in-session, got error: %s", ka.err)
	}
	if ka.apiKeyID != "KEYID-XYZ" {
		t.Fatalf("launch slot should resolve the just-bound key, got apiKeyID %q", ka.apiKeyID)
	}
}

// TestMCPSetupRejectsMalformedArgs: the SDK does not validate against InputSchema,
// so the setup handler must reject a wrong-typed or unknown argument rather than
// silently coerce it to absent (which would turn an intended bind into a resume).
func TestMCPSetupRejectsMalformedArgs(t *testing.T) {
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: okJSON(`{}`)}, "", false)
	defer done()
	for _, tc := range []struct{ name, args string }{
		{"apiKey wrong type", `{"apiKey":123}`},
		{"name wrong type", `{"name":true}`},
		{"withTransfers wrong type", `{"withTransfers":"yes"}`},
		{"unknown key (typo)", `{"api_key":"KEYID-XYZ"}`},
	} {
		res := callNamedTool(t, srv.makeSetupHandler(), "setup", tc.args)
		if !res.IsError {
			t.Errorf("%s: setup must reject %s, got: %s", tc.name, tc.args, resultText(res))
		}
	}
}

// TestMCPSetupRunsDoctorOnBoundKey: re-running the setup tool on an already-bound
// key runs the complementary doctor and embeds its report in the result, the same
// as the cli.
func TestMCPSetupRunsDoctorOnBoundKey(t *testing.T) {
	route := func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "currentKeyInfo"):
			return okJSON(`{"success":true,"data":{"type":"ed25519","status":"activated","permissions":["readOrders","writeOrders"]}}`)(r)
		case strings.Contains(r.URL.Path, "/v2/time"):
			return okJSON(`{"success":true,"data":{"serverTime":1700000000000}}`)(r)
		default:
			return okJSON(`{}`)(r)
		}
	}
	srv, done := buildTestMCP(t, &mcpHTTPStub{route: route}, "trading", false)
	defer done()
	srv.rt.deps.WSDial = nil // skip the WS reachability dial — keep the check hermetic

	res := callNamedTool(t, srv.makeSetupHandler(), "setup", `{"name":"trading"}`)
	if res.IsError {
		t.Fatalf("setup on a bound key errored: %s", resultText(res))
	}
	var doc struct {
		Status string          `json:"status"`
		Doctor json.RawMessage `json:"doctor"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &doc); err != nil {
		t.Fatalf("setup result json: %v\n%s", err, resultText(res))
	}
	if doc.Status != "alreadyConfigured" {
		t.Fatalf("expected alreadyConfigured, got %q", doc.Status)
	}
	if len(doc.Doctor) == 0 {
		t.Fatalf("mcp setup must embed the complementary doctor report for a bound key: %s", resultText(res))
	}
}

func TestMCPDoctorToolReportsHealth(t *testing.T) {
	// whoami succeeds → doctor can render a report. Stub returns a generic OK.
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"apiKeyAccess":{}}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	res := callNamedTool(t, srv.makeDoctorHandler(), "doctor", `{}`)
	if res.IsError {
		t.Fatalf("doctor tool errored: %s", resultText(res))
	}
	// The report is the doctor JSON document; it has a checks array and an ok flag.
	var rep struct {
		Checks []json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &rep); err != nil {
		t.Fatalf("doctor report json: %v\n%s", err, resultText(res))
	}
	if len(rep.Checks) == 0 {
		t.Fatalf("doctor report should carry checks: %s", resultText(res))
	}
	if res.StructuredContent == nil {
		t.Fatal("doctor should provide structuredContent")
	}
}

// --- end-to-end over the in-memory transport --------------------------------

func TestMCPServerOverInMemoryTransport(t *testing.T) {
	doer := &mcpHTTPStub{route: okJSON(`{"success":true,"data":{"last":"100","symbol":"btc_krw"}}`)}
	srv, done := buildTestMCP(t, doer, "bot", false)
	defer done()
	server := srv.build(false, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Run(ctx, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != srv.toolCount {
		t.Fatalf("listed %d tools, want %d", len(tools.Tools), srv.toolCount)
	}
	var hasTicker, hasListKeys, hasReference bool
	annos := map[string]*mcp.ToolAnnotations{}
	for _, tl := range tools.Tools {
		annos[tl.Name] = tl.Annotations
		switch tl.Name {
		case "ticker":
			hasTicker = true
		case "list_keys":
			hasListKeys = true
		case botRuntimeReferenceName:
			hasReference = true
		}
	}
	if !hasTicker || !hasListKeys || !hasReference {
		t.Fatalf("missing expected tools (ticker=%v list_keys=%v bot_runtime_reference=%v)", hasTicker, hasListKeys, hasReference)
	}

	// The onboarding/diagnostics tools are present with the right hints in both
	// modes: doctor is a pure read; setup changes local key state (not read-only)
	// but is non-destructive (resume/bind, nothing is destroyed).
	for _, name := range []string{"setup", "doctor"} {
		if annos[name] == nil {
			t.Fatalf("onboarding tool %q not registered", name)
		}
	}
	if !annos["doctor"].ReadOnlyHint {
		t.Error("doctor must be read-only")
	}
	if annos["setup"].ReadOnlyHint {
		t.Error("setup must not be read-only (it changes local key state)")
	}
	if annos["setup"].DestructiveHint == nil || *annos["setup"].DestructiveHint {
		t.Error("setup must be non-destructive")
	}

	call, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ticker", Arguments: map[string]any{"symbol": "btc_krw"}})
	if err != nil {
		t.Fatalf("call ticker: %v", err)
	}
	if call.IsError {
		t.Fatalf("ticker call errored: %s", resultText(call))
	}
	if !strings.Contains(resultText(call), `"last":"100"`) {
		t.Fatalf("unexpected ticker result: %s", resultText(call))
	}
}

// TestMCPToolStringsSubstituteProgName guards that the generated MCP tool
// descriptions and input-schema property descriptions never leak the {prog}
// placeholder: the spec stores command suggestions with {prog}, and the MCP
// frontend must substitute the program name like help and the catalog do. The
// other guards check structure, not this substitution, so a missed withProg
// would otherwise ship a literal "{prog}" to the model.
func TestMCPToolStringsSubstituteProgName(t *testing.T) {
	for _, c := range endpointSurface() {
		if d := toolDescription(c); strings.Contains(d, progPlaceholder) {
			t.Errorf("%s: tool description leaks %q: %s", c.Key(), progPlaceholder, d)
		}
		if s := string(inputSchema(c, false, nil)); strings.Contains(s, progPlaceholder) {
			t.Errorf("%s: input schema leaks %q: %s", c.Key(), progPlaceholder, s)
		}
	}
}

// --read-only / --multi-key resolve from either the flag or a truthy env var
// (the env form is how the .mcpb Desktop Extension toggles them). The flag, when
// set, wins; otherwise only "1"/"true"/"yes" enable it.
func TestMCPBoolFlagOrEnv(t *testing.T) {
	const env = "DIGITALX_CLI_MCP_READ_ONLY"
	tests := []struct {
		name    string
		flagSet bool
		envVal  string
		want    bool
	}{
		{"both unset", false, "", false},
		{"env 1", false, "1", true},
		{"env true", false, "true", true},
		{"env yes", false, "yes", true},
		{"env 0", false, "0", false},
		{"env false", false, "false", false}, // the literal an MCPB host sends for an unchecked toggle
		{"env junk", false, "nope", false},
		{"flag set, no env", true, "", true},
		{"flag set wins over falsey env", true, "0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &runtime{deps: resolveDeps(Deps{Getenv: func(k string) string {
				if k == env {
					return tc.envVal
				}
				return ""
			}})}
			cmd := testMCPCmd()
			if tc.flagSet {
				if err := cmd.Flags().Set("read-only", "true"); err != nil {
					t.Fatal(err)
				}
			}
			if got := rt.mcpBoolFlagOrEnv(cmd, "read-only", env); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
