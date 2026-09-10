// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/cli"
	"github.com/digitalx-official/digitalx-cli/internal/journal"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
	"github.com/digitalx-official/digitalx-cli/internal/tui"
)

// runTUICLI is runCLI plus the WebSocket dial and TUI runner seams.
func runTUICLI(args []string, env map[string]string, doer apiclient.Doer, dial stream.Dialer, tuiRun func(tui.Config) error) (string, string, int) {
	merged := map[string]string{"DIGITALX_CLI_HOME": sharedTestHome()}
	for k, v := range env {
		merged[k] = v
	}
	var out, errb bytes.Buffer
	code := cli.Execute(args, cli.Deps{
		Getenv: func(k string) string { return merged[k] },
		Stdout: &out,
		Stderr: &errb,
		Doer:   doer,
		Now:    func() int64 { return 1700000000000 },
		Sleep:  func(time.Duration) {},
		WSDial: dial,
		TUIRun: tuiRun,
	})
	return out.String(), errb.String(), code
}

func TestTUIUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"positional rejected", []string{"tui", "btc_krw"}, "no positional arguments"},
		{"bad symbol", []string{"tui", "--symbols", "BTC KRW"}, "--symbols"},
		{"json rejected", []string{"tui", "--symbols", "btc_krw", "--json"}, "monitor"},
		{"compact rejected", []string{"tui", "--symbols", "btc_krw", "--compact"}, "monitor"},
		{"account-seq with public rejected", []string{"tui", "--symbols", "btc_krw", "--public", "--account-seq", "1,2"}, "--account-seq"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, code := runTUICLI(tc.args, nil, nil, nil, nil)
			if code != 2 {
				t.Fatalf("exit=%d, want 2 (stderr %q)", code, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Fatalf("stderr %q missing %q", errb, tc.want)
			}
		})
	}
}

func TestTUIRequiresTTY(t *testing.T) {
	// No TUIRun seam injected: the real runner path demands a terminal, and
	// the test stdout is a buffer.
	_, errb, code := runTUICLI([]string{"tui", "--symbols", "btc_krw", "--public", "--base-url", "http://127.0.0.1:9999"}, nil, nil, nil, nil)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr %q)", code, errb)
	}
	if !strings.Contains(errb, "TTY") {
		t.Fatalf("stderr %q should explain the TTY requirement", errb)
	}
}

func TestTUIDryRunPlans(t *testing.T) {
	type plan struct {
		DryRun        bool   `json:"dryRun"`
		PublicURL     string `json:"wsPublicUrl"`
		PrivateURL    string `json:"wsPrivateUrl"`
		Auth          bool   `json:"auth"`
		Backfill      bool   `json:"backfill"`
		Subscriptions []struct {
			Channel string   `json:"channel"`
			Symbols []string `json:"symbols"`
		} `json:"subscriptions"`
	}
	parse := func(t *testing.T, out string) plan {
		t.Helper()
		var p plan
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("plan json: %v (%q)", err, out)
		}
		return p
	}

	out, _, code := runTUICLI([]string{"tui", "--symbols", "btc_krw,eth_krw", "--public", "--base-url", "http://127.0.0.1:9999", "--dry-run", "--compact"}, nil, nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	p := parse(t, out)
	if !p.DryRun || p.Auth || !p.Backfill || p.PrivateURL != "" {
		t.Errorf("public plan flags wrong: %+v", p)
	}
	if len(p.Subscriptions) != 3 {
		t.Fatalf("public plan must have the three market channels: %+v", p.Subscriptions)
	}
	// ticker covers all symbols (it powers the sidebar); orderbook/trade cover
	// only the active symbol (btc_krw), moved dynamically on a switch.
	if p.Subscriptions[0].Channel != "ticker" || len(p.Subscriptions[0].Symbols) != 2 {
		t.Errorf("ticker should cover all symbols: %+v", p.Subscriptions[0])
	}
	for _, i := range []int{1, 2} {
		if len(p.Subscriptions[i].Symbols) != 1 || p.Subscriptions[i].Symbols[0] != "btc_krw" {
			t.Errorf("%s should cover only the active symbol: %+v", p.Subscriptions[i].Channel, p.Subscriptions[i])
		}
	}

	out, _, code = runTUICLI([]string{"tui", "--symbols", "btc_krw", "--base-url", "http://127.0.0.1:9999", "--dry-run", "--compact"}, nil, nil, nil, nil)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	p = parse(t, out)
	if !p.Auth || p.PrivateURL != "ws://127.0.0.1:9999/v2/private" {
		t.Errorf("private plan should sign and derive the private WS URL: %+v", p)
	}
	channels := make([]string, 0, len(p.Subscriptions))
	for _, s := range p.Subscriptions {
		channels = append(channels, s.Channel)
	}
	if strings.Join(channels, ",") != "ticker,orderbook,trade,myOrder,myTrade,myAsset" {
		t.Errorf("private plan channels wrong: %v", channels)
	}
}

func TestTUIPrivateRequiresKey(t *testing.T) {
	called := false
	_, errb, code := runTUICLI([]string{"tui", "--symbols", "btc_krw"},
		map[string]string{"DIGITALX_CLI_HOME": t.TempDir()},
		nil, nil, func(tui.Config) error { called = true; return nil })
	if code != 4 {
		t.Fatalf("exit=%d, want 4 (stderr %q)", code, errb)
	}
	if called {
		t.Fatal("the TUI must not start without a usable key")
	}
}

func TestTUISeamConfigPublicMode(t *testing.T) {
	var got tui.Config
	_, errb, code := runTUICLI(
		[]string{"tui", "--symbols", "btc_krw,eth_krw", "--public", "--base-url", "http://127.0.0.1:9999"},
		nil, failingDoer(), dialFrames(),
		func(cfg tui.Config) error { got = cfg; return nil })
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.Join(got.Symbols, ",") != "btc_krw,eth_krw" {
		t.Errorf("symbols wrong: %v", got.Symbols)
	}
	if got.Private || got.Trader != nil || got.KeyName != "" {
		t.Errorf("public mode must not carry account state: %+v", got)
	}
	if got.BaseURL != "http://127.0.0.1:9999" {
		t.Errorf("base url wrong: %q", got.BaseURL)
	}
	if got.Events == nil || got.StopSession == nil {
		t.Error("session wiring missing")
	}
	if got.SetActiveMarket == nil {
		t.Error("SetActiveMarket must be wired so the sidebar can switch the active market")
	}
}

// TestTUICandlesSeamSendsSymbol pins the candle-chart fetch seam: the symbol is
// a positional on the candles op, so it must be normalized and injected into the
// request (validateParams alone drops it, sending an empty symbol → "invalid
// currency pair").
func TestTUICandlesSeamSendsSymbol(t *testing.T) {
	var gotURL string
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/v2/candles") {
			gotURL = r.URL.String()
			return resp(200, `{"success":true,"data":[`+
				`{"timestamp":0,"open":"100","high":"110","low":"90","close":"105","volume":"1"}]}`, nil), nil
		}
		return resp(500, `{}`, nil), nil
	})
	var got tui.Config
	_, errb, code := runTUICLI(
		[]string{"tui", "--symbols", "btc_krw", "--public", "--base-url", "http://127.0.0.1:9999"},
		nil, doer, dialFrames(),
		func(cfg tui.Config) error { got = cfg; return nil })
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if got.Candles == nil {
		t.Fatal("Candles seam must be wired")
	}
	bars, err := got.Candles("btc_krw", "60", 100, 0)
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}
	if len(bars) != 1 || bars[0].Close != "105" {
		t.Errorf("bars wrong: %+v", bars)
	}
	if !strings.Contains(gotURL, "symbol=btc_krw") {
		t.Errorf("candles request must carry the symbol, got %q", gotURL)
	}
	if strings.Contains(gotURL, "end=") {
		t.Errorf("endMs 0 must omit the end bound, got %q", gotURL)
	}
	// A non-zero endMs pages back into history: the bound must reach the request
	// (the candles endpoint names it "end", not "endTime").
	if _, err := got.Candles("btc_krw", "60", 100, 1_700_000_000_000); err != nil {
		t.Fatalf("Candles (older page): %v", err)
	}
	if !strings.Contains(gotURL, "end=1700000000000") {
		t.Errorf("backfill request must carry the end bound, got %q", gotURL)
	}
}

// TestTUIDefaultsToLaunchedPairs: with no --symbols, the TUI watches every
// launched pair from /v2/currencyPairs (stopped pairs filtered out, sorted).
func TestTUIDefaultsToLaunchedPairs(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/v2/currencyPairs") {
			return resp(200, `{"success":true,"data":[`+
				`{"symbol":"eth_krw","status":"launched"},`+
				`{"symbol":"xrp_krw","status":"stopped"},`+
				`{"symbol":"btc_krw","status":"launched"}]}`, nil), nil
		}
		return resp(500, `{}`, nil), nil // other (clock/seed) calls — tolerated
	})
	var got tui.Config
	_, errb, code := runTUICLI(
		[]string{"tui", "--public", "--base-url", "http://127.0.0.1:9999"},
		nil, doer, dialFrames(),
		func(cfg tui.Config) error { got = cfg; return nil })
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.Join(got.Symbols, ",") != "btc_krw,eth_krw" {
		t.Errorf("want the sorted launched pairs, got %v", got.Symbols)
	}
}

// TestTUIPublicReadsJournaling pins that the TUI's public market-data reads —
// the launched-pairs list and the candle chart — obey the central journaling
// policy rather than a call-site decision: journaled in --debug (like every
// other surface's public reads), and the DB never even opened in a normal run.
func TestTUIPublicReadsJournaling(t *testing.T) {
	publicDoer := func() apiclient.Doer {
		return doerFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(r.URL.Path, "/v2/currencyPairs"):
				return resp(200, `{"success":true,"data":[{"symbol":"btc_krw","status":"launched"}]}`, nil), nil
			case strings.Contains(r.URL.Path, "/v2/candles"):
				return resp(200, `{"success":true,"data":[`+
					`{"timestamp":0,"open":"100","high":"110","low":"90","close":"105","volume":"1"}]}`, nil), nil
			}
			return resp(500, `{}`, nil), nil
		})
	}

	t.Run("journaled in debug", func(t *testing.T) {
		home := t.TempDir()
		// No --symbols, so launchedPairs runs; the candle fetch goes through the
		// captured seam INSIDE the runner, while the recorder is still open.
		_, errb, code := runTUICLI(
			[]string{"tui", "--public", "--debug", "--base-url", "http://127.0.0.1:9999"},
			map[string]string{"DIGITALX_CLI_HOME": home},
			publicDoer(), dialFrames(),
			func(cfg tui.Config) error {
				if cfg.Candles == nil {
					t.Fatal("Candles seam must be wired")
				}
				if _, err := cfg.Candles("btc_krw", "60", 100, 0); err != nil {
					t.Fatalf("Candles: %v", err)
				}
				return nil
			})
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb)
		}
		jl := openJournal(t, home)
		calls, _ := jl.RecentCalls(20)
		var sawPairs, sawCandles bool
		for _, c := range calls {
			if c.Auth {
				t.Errorf("public read journaled with auth=true: %+v", c)
			}
			switch {
			case strings.Contains(c.Path, "/v2/currencyPairs"):
				sawPairs = true
			case strings.Contains(c.Path, "/v2/candles"):
				sawCandles = true
			}
		}
		if !sawPairs || !sawCandles {
			t.Fatalf("debug run must journal both public reads (pairs=%v candles=%v): %+v", sawPairs, sawCandles, calls)
		}
	})

	t.Run("not opened in a normal run", func(t *testing.T) {
		home := t.TempDir()
		_, errb, code := runTUICLI(
			[]string{"tui", "--public", "--base-url", "http://127.0.0.1:9999"},
			map[string]string{"DIGITALX_CLI_HOME": home},
			publicDoer(), dialFrames(),
			func(cfg tui.Config) error {
				if _, err := cfg.Candles("btc_krw", "60", 100, 0); err != nil {
					t.Fatalf("Candles: %v", err)
				}
				return nil
			})
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb)
		}
		if _, err := os.Stat(journal.DefaultPath(home)); !os.IsNotExist(err) {
			t.Fatalf("a normal (non-debug) public-only run must not create the journal DB, stat err=%v", err)
		}
	})
}

// An empty launched set is a usage error pointing at --symbols (exit 2); a
// fetch failure is a network/API error (NOT usage), so it keeps a non-2 exit.
func TestTUILaunchedPairsEdgeCases(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		doer := doerFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/v2/currencyPairs") {
				return resp(200, `{"success":true,"data":[]}`, nil), nil
			}
			return resp(500, `{}`, nil), nil
		})
		_, errb, code := runTUICLI([]string{"tui", "--public", "--base-url", "http://127.0.0.1:9999"}, nil, doer, dialFrames(), func(tui.Config) error { return nil })
		if code != 2 || !strings.Contains(errb, "--symbols") {
			t.Fatalf("empty list: exit=%d stderr=%q, want usage error mentioning --symbols", code, errb)
		}
	})
	t.Run("network failure", func(t *testing.T) {
		doer := doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})
		ran := false
		_, errb, code := runTUICLI([]string{"tui", "--public", "--base-url", "http://127.0.0.1:9999"}, nil, doer, dialFrames(), func(tui.Config) error { ran = true; return nil })
		if ran {
			t.Fatal("the TUI must not start when the pairs fetch failed")
		}
		// A transport failure is classified as network (exit 1), not usage (2),
		// and the wrapped message keeps the --symbols pointer.
		if code != 1 {
			t.Fatalf("network failure: exit=%d, want 1; stderr=%q", code, errb)
		}
		if !strings.Contains(errb, "launched trading pairs") {
			t.Fatalf("network failure stderr %q should explain the failed pairs lookup", errb)
		}
	})
}

// tradeDoer routes the REST calls a private TUI session makes: the startup key
// pre-flight, clock probes, private backfill, and the trader's place/cancel.
func tradeDoer(t *testing.T) (apiclient.Doer, *struct {
	sync.Mutex
	placeBody  string
	cancelSeen string
}) {
	rec := &struct {
		sync.Mutex
		placeBody  string
		cancelSeen string
	}{}
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/v2/currentKeyInfo":
			// Healthy key pre-flight: activated, main account allowed, no expiry.
			return resp(200, `{"success":true,"data":{"apiKey":"k","type":"ed25519","status":"activated","permissions":["readOrders","writeOrders","readBalances"],"allowedAccountSeqs":[1]}}`, nil), nil
		case r.URL.Path == "/v2/time":
			return resp(200, `{"success":true,"data":{"time":1700000000000}}`, nil), nil
		case r.URL.Path == "/v2/balance":
			return resp(200, `{"success":true,"data":[]}`, nil), nil
		case r.URL.Path == "/v2/openOrders":
			return resp(200, `{"success":true,"data":[]}`, nil), nil
		case r.Method == "POST" && r.URL.Path == "/v2/orders":
			body, _ := io.ReadAll(r.Body)
			rec.Lock()
			rec.placeBody = string(body)
			rec.Unlock()
			return resp(200, `{"success":true,"data":{"orderId":424242}}`, nil), nil
		case r.Method == "DELETE" && r.URL.Path == "/v2/orders":
			rec.Lock()
			rec.cancelSeen = r.URL.RawQuery
			rec.Unlock()
			return resp(200, `{"success":true}`, nil), nil
		}
		return resp(500, `{}`, nil), nil
	})
	return doer, rec
}

// keyInfoDoer answers the startup /v2/currentKeyInfo pre-flight with the given
// status/body (and a stock clock probe); every other path 500s, since a failed
// pre-flight must return before the session dials anything.
func keyInfoDoer(status int, body string) apiclient.Doer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v2/currentKeyInfo":
			return resp(status, body, nil), nil
		case "/v2/time":
			return resp(200, `{"success":true,"data":{"time":1700000000000}}`, nil), nil
		}
		return resp(500, `{}`, nil), nil
	})
}

// A key/config problem the pre-flight can see must abort BEFORE the alt-screen
// opens (exit 4, diagnostic on the terminal) — never inside the full-screen
// program, where it would be lost on exit. Per-endpoint permissions are not
// gated here (surfaced in-band on the action instead), so they are not tested.
func TestTUIPreflightBlocksBeforeAltScreen(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{
			name:    "ip not allowlisted",
			status:  403,
			body:    `{"success":false,"error":{"code":403,"message":"IP_NOT_ALLOWED","description":"nope"}}`,
			wantSub: "allowlisted",
		},
		{
			name:    "deactivated key",
			status:  200,
			body:    `{"success":true,"data":{"apiKey":"k","type":"ed25519","status":"deactivated","allowedAccountSeqs":[1]}}`,
			wantSub: "not activated",
		},
		{
			name:    "session account-seq not allowed",
			status:  200,
			body:    `{"success":true,"data":{"apiKey":"k","type":"ed25519","status":"activated","allowedAccountSeqs":[2]}}`,
			wantSub: "allowed accounts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			seedBoundKey(t, home)
			ran := false
			_, errb, code := runTUICLI(
				[]string{"tui", "--symbols", "btc_krw", "--key", "bot", "--base-url", "http://127.0.0.1:9999"},
				map[string]string{"DIGITALX_CLI_HOME": home},
				keyInfoDoer(tc.status, tc.body), dialFrames(),
				func(tui.Config) error { ran = true; return nil },
			)
			if code != 4 {
				t.Fatalf("exit=%d, want 4 (config) — stderr %q", code, errb)
			}
			if ran {
				t.Fatal("TUI must NOT start when the key pre-flight fails — the diagnostic would be lost on the alt-screen")
			}
			if !strings.Contains(errb, tc.wantSub) {
				t.Fatalf("stderr %q missing %q", errb, tc.wantSub)
			}
		})
	}
}

// multiAccountDoer answers the startup pre-flight with the given allowed
// sub-account set (a JSON array) plus the stock session reads, so the seam
// receives the resolved AccountSeq/AccountSeqs.
func multiAccountDoer(allowedJSON string) apiclient.Doer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v2/currentKeyInfo":
			return resp(200, `{"success":true,"data":{"apiKey":"k","type":"ed25519","status":"activated","permissions":["readOrders","writeOrders","readBalances"],"allowedAccountSeqs":`+allowedJSON+`}}`, nil), nil
		case "/v2/time":
			return resp(200, `{"success":true,"data":{"time":1700000000000}}`, nil), nil
		case "/v2/balance":
			return resp(200, `{"success":true,"data":[]}`, nil), nil
		case "/v2/openOrders":
			return resp(200, `{"success":true,"data":[]}`, nil), nil
		}
		return resp(500, `{}`, nil), nil
	})
}

// The multi-account wiring end to end: with --account-seq omitted the session
// subscribes the key's whole allowed set (active on the configured default, here
// main); with an explicit list it subscribes exactly that, active on the first.
func TestTUIMultiAccountResolvesSubscribeSet(t *testing.T) {
	t.Run("omitted subscribes the allowed set, active on main", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		var got tui.Config
		_, errb, code := runTUICLI(
			[]string{"tui", "--symbols", "btc_krw", "--key", "bot", "--base-url", "http://127.0.0.1:9999"},
			map[string]string{"DIGITALX_CLI_HOME": home},
			multiAccountDoer(`[3,1,2]`), dialFrames(),
			func(cfg tui.Config) error { got = cfg; return nil })
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb)
		}
		if got.AccountSeq != 1 {
			t.Fatalf("active=%d, want 1 (the key's default)", got.AccountSeq)
		}
		if len(got.AccountSeqs) != 3 || got.AccountSeqs[0] != 1 || got.AccountSeqs[1] != 2 || got.AccountSeqs[2] != 3 {
			t.Fatalf("AccountSeqs=%v, want the allowed set sorted [1 2 3]", got.AccountSeqs)
		}
	})
	t.Run("explicit list is used verbatim, active on the first", func(t *testing.T) {
		home := t.TempDir()
		seedBoundKey(t, home)
		var got tui.Config
		_, errb, code := runTUICLI(
			[]string{"tui", "--symbols", "btc_krw", "--key", "bot", "--account-seq", "2,3", "--base-url", "http://127.0.0.1:9999"},
			map[string]string{"DIGITALX_CLI_HOME": home},
			multiAccountDoer(`[1,2,3]`), dialFrames(),
			func(cfg tui.Config) error { got = cfg; return nil })
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb)
		}
		if got.AccountSeq != 2 {
			t.Fatalf("active=%d, want 2 (first listed)", got.AccountSeq)
		}
		if len(got.AccountSeqs) != 2 || got.AccountSeqs[0] != 2 || got.AccountSeqs[1] != 3 {
			t.Fatalf("AccountSeqs=%v, want [2 3] verbatim", got.AccountSeqs)
		}
	})
}

// With --account-seq omitted the session must subscribe EVERY account the key
// can access — a set only the /v2/currentKeyInfo read can supply. If that read
// fails the set is unknowable, so the session must refuse to start rather than
// silently narrow to the single active account and hide the key's other accounts
// in a money UI. The refusal preserves the failure's classification (a 5xx is an
// API error, exit 3; a transport failure is exit 1) — never exit 0. An explicit
// --account-seq needs no such read and stays resilient (covered by the preflight
// non-fatal tests).
func TestTUIOmittedAccountSeqRefusesWhenKeyInfoFails(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	// currentKeyInfo 500s (transient 5xx); the clock probe still answers.
	doer := keyInfoDoer(500, `{"success":false,"error":{"code":500,"message":"INTERNAL"}}`)
	ran := false
	_, errb, code := runTUICLI(
		[]string{"tui", "--symbols", "btc_krw", "--key", "bot", "--base-url", "http://127.0.0.1:9999"},
		map[string]string{"DIGITALX_CLI_HOME": home},
		doer, dialFrames(),
		func(tui.Config) error { ran = true; return nil })
	if ran {
		t.Fatal("TUI must NOT start when the omitted account set can't be resolved")
	}
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (API error, 5xx) — stderr %q", code, errb)
	}
	if !strings.Contains(errb, "--account-seq") {
		t.Fatalf("stderr %q should point at --account-seq as the way forward", errb)
	}
}

func TestTUITraderPlacesValidatesAndCancels(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer, rec := tradeDoer(t)

	ran := false
	_, errb, code := runTUICLI(
		[]string{"tui", "--symbols", "btc_krw", "--key", "bot", "--base-url", "http://127.0.0.1:9999"},
		map[string]string{"DIGITALX_CLI_HOME": home},
		doer, dialFrames(),
		func(cfg tui.Config) error {
			ran = true
			if !cfg.Private || cfg.Trader == nil || cfg.KeyName != "bot" {
				t.Fatalf("private config wrong: %+v", cfg)
			}

			// The sizing matrix runs before anything is sent.
			_, err := cfg.Trader.Place(tui.OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit", Price: "100", AccountSeq: 1})
			var ue *output.UsageError
			if err == nil || !errors.As(err, &ue) || !strings.Contains(ue.Message, "qty") {
				t.Fatalf("missing qty must be a usage error, got %v", err)
			}

			res, err := cfg.Trader.Place(tui.OrderForm{Symbol: "btc_krw", Side: "buy", Type: "limit", Price: "100", Qty: "1", AccountSeq: 1})
			if err != nil {
				t.Fatalf("place: %v", err)
			}
			if res.OrderID != "424242" || res.ClientOrderID == "" || res.Warning != "" {
				t.Fatalf("place result wrong: %+v", res)
			}

			warning, err := cfg.Trader.Cancel("btc_krw", 424242, 1)
			if err != nil || warning != "" {
				t.Fatalf("cancel: %v %q", err, warning)
			}
			return nil
		})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !ran {
		t.Fatal("TUI runner not invoked")
	}

	rec.Lock()
	placeBody, cancelSeen := rec.placeBody, rec.cancelSeen
	rec.Unlock()
	for _, want := range []string{"symbol=btc_krw", "side=buy", "orderType=limit", "price=100", "qty=1", "clientOrderId=", "signature="} {
		if !strings.Contains(placeBody, want) {
			t.Errorf("place body %q missing %q", placeBody, want)
		}
	}
	if !strings.Contains(cancelSeen, "orderId=424242") || !strings.Contains(cancelSeen, "signature=") {
		t.Errorf("cancel query wrong: %q", cancelSeen)
	}

	// The action journal recorded the placement (hard guarantee).
	out, _, code := runCLI([]string{"logs", "--orders", "--compact"},
		map[string]string{"DIGITALX_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("logs exit=%d", code)
	}
	if !strings.Contains(out, `"accepted"`) || !strings.Contains(out, "424242") {
		t.Fatalf("journal must hold the accepted order: %q", out)
	}
}
