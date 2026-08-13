// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Default (no --json/--compact) output is human-readable.

func TestTickerHumanDefault(t *testing.T) {
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":[{"symbol":"btc_krw","close":"77136000","priceChangePercent":"0.1","high":"79650000","low":"76550000","volume":"48.73"}]}`, nil)}
	out, _, code := runCLI([]string{"ticker", "btc_krw"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	// Not JSON: no braces, and the price is grouped with thousand separators.
	if strings.Contains(out, "{") {
		t.Fatalf("human default must not emit JSON: %s", out)
	}
	if !strings.Contains(out, "btc_krw") || !strings.Contains(out, "77,136,000") {
		t.Fatalf("expected human ticker table with grouped price: %s", out)
	}
	if !strings.Contains(out, "symbol") || !strings.Contains(out, "last") {
		t.Fatalf("expected ticker headers: %s", out)
	}
}

func TestOrderbookHumanDefault(t *testing.T) {
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":{"timestamp":1708057740895,"bids":[{"price":"73303000","qty":"0.0089"}],"asks":[{"price":"73304000","qty":"0.0098"}]}}`, nil)}
	out, _, code := runCLI([]string{"orderbook", "btc_krw"}, nil, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("human orderbook must not emit JSON: %s", out)
	}
	if !strings.Contains(out, "Bids") || !strings.Contains(out, "Asks") {
		t.Fatalf("expected bid/ask ladder: %s", out)
	}
	if !strings.Contains(out, "73,303,000") {
		t.Fatalf("expected grouped bid price: %s", out)
	}
}

func TestBalanceHumanDefault(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":[{"currency":"krw","balance":"1000000","available":"700000","tradeInUse":"300000","withdrawalInUse":"0"}]}`, nil)}
	out, _, code := runCLI([]string{"balance", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("human balance must not emit JSON: %s", out)
	}
	if !strings.Contains(out, "krw") || !strings.Contains(out, "1,000,000") || !strings.Contains(out, "available") {
		t.Fatalf("expected human balance table: %s", out)
	}
}

func TestOrderGetHumanDefault(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":{"orderId":1234,"symbol":"btc_krw","side":"buy","orderType":"limit","status":"partiallyFilled","price":"5000","qty":"10","filledQty":"1"}}`, nil)}
	out, _, code := runCLI([]string{"order", "get", "--symbol", "btc_krw", "--order-id", "1234", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("human order get must not emit JSON: %s", out)
	}
	if !strings.Contains(out, "orderId") || !strings.Contains(out, "1234") || !strings.Contains(out, "partiallyFilled") {
		t.Fatalf("expected human order key-value block: %s", out)
	}
}

// --json yields exactly today's bytes (pretty), --compact is single-line JSON
// and implies --json.

func TestJSONFlagRoundTrip(t *testing.T) {
	body := `{"success":true,"data":{"last":"100","symbol":"btc_krw"}}`
	// --json (pretty)
	out, _, code := runCLI([]string{"ticker", "btc_krw", "--json"}, nil, &stubDoer{resp: resp(200, body, nil)})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json must emit JSON: %v (%s)", err, out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatalf("--json (no --compact) should be pretty-printed (multi-line): %q", out)
	}
	// --compact: single line, implies JSON.
	out2, _, code2 := runCLI([]string{"ticker", "btc_krw", "--compact"}, nil, &stubDoer{resp: resp(200, body, nil)})
	if code2 != 0 {
		t.Fatalf("exit=%d", code2)
	}
	if strings.TrimSpace(out2) != `{"last":"100","symbol":"btc_krw"}` {
		t.Fatalf("--compact should be single-line JSON: %q", out2)
	}
}

// The `commands` catalog is human-readable by default (a grouped listing) and
// emits the machine catalog only under --json — it must NOT print JSON without
// the flag.
func TestCommandsHumanByDefault(t *testing.T) {
	humanOut, _, code := runCLI([]string{"commands"}, nil, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.HasPrefix(strings.TrimSpace(humanOut), "{") {
		t.Fatalf("`commands` without --json must be human text, not JSON: %q", humanOut)
	}
	if !strings.Contains(humanOut, "commands") || !strings.Contains(humanOut, "ticker") {
		t.Fatalf("expected a human command listing: %q", humanOut)
	}
	jsonOut, _, _ := runCLI([]string{"commands", "--json"}, nil, &stubDoer{})
	var v any
	if err := json.Unmarshal([]byte(jsonOut), &v); err != nil {
		t.Fatalf("`commands --json` must be valid JSON: %v", err)
	}
	if humanOut == jsonOut {
		t.Fatalf("human and --json output should differ")
	}
}

// The defensive pretty-JSON fallback in emitMode is reached when a formatter
// can't recognize the payload shape (here: a ticker response that isn't the
// expected array). Human mode then emits the same bytes as --json rather than a
// misleading table.
func TestDefensiveJSONFallback(t *testing.T) {
	body := `{"success":true,"data":{"unexpected":"shape"}}`
	humanOut, _, code := runCLI([]string{"ticker", "btc_krw"}, nil, &stubDoer{resp: resp(200, body, nil)})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	jsonOut, _, _ := runCLI([]string{"ticker", "btc_krw", "--json"}, nil, &stubDoer{resp: resp(200, body, nil)})
	if humanOut != jsonOut {
		t.Fatalf("an unrecognized payload should fall back to --json bytes:\nhuman=%q\njson=%q", humanOut, jsonOut)
	}
	var v any
	if err := json.Unmarshal([]byte(humanOut), &v); err != nil {
		t.Fatalf("fallback must be valid JSON: %v", err)
	}
}

// The local/meta commands render human text by default (never JSON without
// --json). One assertion per family guards against a result-struct JSON-tag drift
// silently dropping a command back to the pretty-JSON fallback.
func TestLocalCommandsHumanByDefault(t *testing.T) {
	notJSON := func(t *testing.T, out string) {
		t.Helper()
		s := strings.TrimSpace(out)
		if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			t.Fatalf("human mode must not emit JSON: %q", out)
		}
	}

	t.Run("key list", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, _, code := runCLI([]string{"key", "list"}, map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "bot") || !strings.Contains(out, "new-key keystore") {
			t.Fatalf("expected key listing: %q", out)
		}
	})

	t.Run("key show", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, _, code := runCLI([]string{"key", "show", "bot"}, map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "publicKey") {
			t.Fatalf("expected key detail with publicKey: %q", out)
		}
	})

	t.Run("setup", func(t *testing.T) {
		home := t.TempDir()
		out, _, code := runWithDeps([]string{"setup", "--name", "fresh"},
			map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		// The whole registration guidance (headline, numbered next steps with the
		// link, public key) is the result and renders on stdout.
		if !strings.Contains(out, "Generated ED25519 key") || !strings.Contains(out, "Next steps:") || !strings.Contains(out, "manage/create") {
			t.Fatalf("expected setup guidance on stdout: %q", out)
		}
	})

	t.Run("ip", func(t *testing.T) {
		out, _, code := runWithDeps([]string{"ip"},
			map[string]string{"KORBIT_CLI_HOME": t.TempDir()}, nil, fakeProbe("203.0.113.7", ""))
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "203.0.113.7") {
			t.Fatalf("expected IP report: %q", out)
		}
	})

	t.Run("order place dry-run", func(t *testing.T) {
		// order place dry-run makes a best-effort PUBLIC orderbook probe for its
		// safety checks; give it a book so the path is exercised (a resting
		// below-market limit buy, so no warnings are expected).
		book := resp(200, `{"success":true,"data":{"timestamp":1,"bids":[{"price":"99000000","qty":"1"}],"asks":[{"price":"100000000","qty":"1"}]}}`, nil)
		out, _, code := runCLI([]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit", "--price", "90000000", "--qty", "0.01", "--dry-run"}, nil, &stubDoer{resp: book})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "DRY RUN") {
			t.Fatalf("expected dry-run plan: %q", out)
		}
	})

	t.Run("monitor dry-run", func(t *testing.T) {
		out, _, code := runCLI([]string{"monitor", "--symbols", "btc_krw", "--ticker", "--dry-run"}, nil, &stubDoer{})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "monitor plan") {
			t.Fatalf("expected monitor plan: %q", out)
		}
	})

	t.Run("mcp dry-run", func(t *testing.T) {
		out, _, code := runCLI([]string{"mcp", "serve", "--read-only", "--dry-run"}, nil, &stubDoer{})
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		notJSON(t, out)
		if !strings.Contains(out, "mcp serve plan") {
			t.Fatalf("expected mcp plan: %q", out)
		}
	})
}

// The mcp serve --read-only / --multi-key toggles resolve from env vars
// (KORBIT_CLI_MCP_*) end to end through the real command — the path the .mcpb
// Desktop Extension uses to expose them as install-time checkboxes. Guards
// against a refactor that stops consulting the env in runMCP.
func TestMCPServeEnvTogglesWireThroughPlan(t *testing.T) {
	out, _, code := runCLI(
		[]string{"mcp", "serve", "--dry-run", "--json"},
		map[string]string{"KORBIT_CLI_MCP_READ_ONLY": "1", "KORBIT_CLI_MCP_MULTI_KEY": "true"},
		&stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d: %s", code, out)
	}
	var plan struct {
		ReadOnly bool `json:"readOnly"`
		MultiKey bool `json:"multiKey"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan JSON: %v\n%s", err, out)
	}
	if !plan.ReadOnly || !plan.MultiKey {
		t.Errorf("env toggles not honored: readOnly=%v multiKey=%v", plan.ReadOnly, plan.MultiKey)
	}
}

// order place in human mode still surfaces the echoed clientOrderId.
func TestOrderPlaceHumanEchoesClientOrderID(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":{"orderId":123,"status":"open"}}`, nil)}
	out, _, code := runCLI(
		[]string{"order", "place", "--symbol", "btc_krw", "--side", "buy", "--type", "limit",
			"--price", "100000000", "--qty", "0.001", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("human order place must not emit JSON: %s", out)
	}
	if !strings.Contains(out, "clientOrderId") {
		t.Fatalf("human order place must surface clientOrderId: %s", out)
	}
	// The same id must have been sent in the request body.
	if !strings.Contains(doer.body, "clientOrderId=") {
		t.Fatalf("clientOrderId not sent: %s", doer.body)
	}
}

// Golden field-mapping tests: feed a canned API response and assert the
// DISTINCTIVE values render. A formatter that stops reading a field (its source
// field renamed or dropped) would show a blank — it returns ok=true on a missing
// field, so it never falls back to JSON — and these catch that silent blanking,
// which a header-only check misses. (They don't catch a swap of two same-typed
// fields, since the match is position-independent; silent blanking is the target.)
func TestHumanFormattersPinFieldMapping(t *testing.T) {
	cases := []struct {
		name string
		args []string
		body string
		want []string
	}{
		{
			"balance",
			[]string{"balance", "--key", "bot"},
			`{"success":true,"data":[{"currency":"btc","balance":"1.5","available":"1.0","tradeInUse":"0.5","withdrawalInUse":"0"}]}`,
			[]string{"btc", "1.5", "1.0", "0.5"},
		},
		{
			"order get",
			[]string{"order", "get", "--symbol", "btc_krw", "--order-id", "123", "--key", "bot"},
			`{"success":true,"data":{"orderId":123,"symbol":"btc_krw","side":"buy","orderType":"limit","status":"open","price":"100","qty":"0.5"}}`,
			[]string{"123", "buy", "limit", "open"},
		},
		{
			"fills",
			[]string{"fills", "--symbol", "btc_krw", "--key", "bot"},
			`{"success":true,"data":[{"tradeId":99,"orderId":123,"side":"sell","price":"100","qty":"0.5","amt":"50","feeQty":"0.1","feeCurrency":"krw","isTaker":true}]}`,
			[]string{"99", "123", "sell", "krw"},
		},
		{
			// The order value bounds are why an agent reads this endpoint before
			// sizing, and the `order place` note points at `pairs` for them — so
			// the TEXT table has to carry them, not just --json. Pinned because
			// the two-to-six column growth is a recorded consumer-visible change
			// with nothing else guarding it.
			"pairs",
			[]string{"pairs"},
			`{"success":true,"data":[{"symbol":"btc_krw","status":"launched","baseCurrency":"btc","quoteCurrency":"krw","minOrderValue":"5000","maxOrderValue":"1000000000"}]}`,
			[]string{"symbol", "status", "base", "quote", "minOrderValue", "maxOrderValue",
				"btc_krw", "launched", "btc", "krw", "5000", "1000000000"},
		},
		{
			"whoami",
			[]string{"whoami", "--key", "bot"},
			`{"success":true,"data":{"apiKey":"K1","type":"ed25519","status":"active","permissions":["readOrders"]}}`,
			[]string{"K1", "ed25519", "active", "readOrders"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			seedBoundKey(t, home)
			out, stderr, code := runCLI(tc.args, map[string]string{"KORBIT_CLI_HOME": home},
				&stubDoer{resp: resp(200, tc.body, nil)})
			if code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			if strings.HasPrefix(strings.TrimSpace(out), "{") || strings.HasPrefix(strings.TrimSpace(out), "[") {
				t.Fatalf("human mode must not emit JSON: %s", out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("human %s output missing %q (field-mapping drift?):\n%s", tc.name, w, out)
				}
			}
		})
	}
}

// whoami renders userUuid only when the API returns it; when absent the row is
// omitted entirely (no empty "userUuid:" line).
func TestWhoamiUserUUIDHumanDefault(t *testing.T) {
	const uuid = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	run := func(body string) string {
		home := t.TempDir()
		seedBoundKey(t, home)
		out, stderr, code := runCLI([]string{"whoami", "--key", "bot"},
			map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{resp: resp(200, body, nil)})
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr)
		}
		return out
	}

	present := run(`{"success":true,"data":{"apiKey":"K1","userUuid":"` + uuid + `","type":"ed25519"}}`)
	if !strings.Contains(present, uuid) {
		t.Fatalf("whoami output should show userUuid when present:\n%s", present)
	}

	absent := run(`{"success":true,"data":{"apiKey":"K1","type":"ed25519"}}`)
	if strings.Contains(absent, "userUuid") {
		t.Fatalf("whoami output should omit the userUuid row when absent:\n%s", absent)
	}
}
