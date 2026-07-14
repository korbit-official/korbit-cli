// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// A funding history that saturates the 100-row ceiling is reported as truncated.
// In --json the array is wrapped in {"data":...,"truncated":true,"note":...} so a
// consumer reading only stdout sees the result is incomplete; in human mode the
// table carries a truncation marker.
func TestFundingHistoryTruncationInBand(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	rows := make([]string, 100)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"id":%d,"currency":"btc","status":"done","quantity":"1"}`, i)
	}
	body := `{"success":true,"data":[` + strings.Join(rows, ",") + `]}`

	out, stderr, code := runCLI(
		[]string{"deposit", "history", "btc", "--key", "bot", "--json"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{resp: resp(200, body, nil)})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, "cannot be reached") || strings.Contains(stderr, "result is incomplete") {
		t.Fatalf("truncation note must be carried in stdout, not duplicated on stderr: %s", stderr)
	}
	var env struct {
		Data      []json.RawMessage `json:"data"`
		Truncated bool              `json:"truncated"`
		Note      string            `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("expected a JSON truncation envelope, got %s (%v)", out, err)
	}
	if !env.Truncated || len(env.Data) != 100 || env.Note == "" {
		t.Fatalf("want truncated envelope with 100 rows and a note: truncated=%v rows=%d note=%q", env.Truncated, len(env.Data), env.Note)
	}

	human, _, code2 := runCLI(
		[]string{"deposit", "history", "btc", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{resp: resp(200, body, nil)})
	if code2 != 0 {
		t.Fatalf("exit=%d", code2)
	}
	if strings.HasPrefix(strings.TrimSpace(human), "{") {
		t.Fatalf("human mode must not emit JSON: %s", human)
	}
	if !strings.Contains(human, "⚠") || !strings.Contains(human, "cannot be reached") {
		t.Fatalf("human mode should show a truncation note: %s", human)
	}
}

// A signed GET funding command sends the right method/path/query and is signed.
func TestDepositHistorySignsAndQueries(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true,"data":[{"id":1,"currency":"btc","status":"done","quantity":"1.5"}]}`, nil)}
	out, stderr, code := runCLI(
		[]string{"deposit", "history", "btc", "--limit", "50", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if doer.last.Method != "GET" || doer.last.URL.Path != "/v2/coin/recentDeposits" {
		t.Fatalf("request = %s %s", doer.last.Method, doer.last.URL.Path)
	}
	q := doer.last.URL.Query()
	if q.Get("currency") != "btc" || q.Get("limit") != "50" {
		t.Fatalf("query = %s", doer.last.URL.RawQuery)
	}
	if doer.last.Header.Get("x-kapi-key") != "KEYID-1" {
		t.Fatalf("funding read must be signed")
	}
	if !strings.Contains(out, `"currency":"btc"`) {
		t.Fatalf("stdout = %s", out)
	}
}

// A POST funding command (withdrawal request) builds a POST with the params in
// the body; --dry-run shows the shape without sending.
func TestWithdrawRequestDryRunIsPOST(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	out, _, code := runCLI(
		[]string{"withdraw", "request", "BTC", "--amount", "0.025",
			"--address", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "--network", "BTC",
			"--key", "bot", "--dry-run", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{
		`"method":"POST"`, `"path":"/v2/coin/withdrawal"`, `"auth":true`,
		`"currency":"btc"`, // KindCurrency lowercases the positional
		`"amount":"0.025"`, `"address":"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"`, `"network":"BTC"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run missing %s in: %s", want, out)
		}
	}
}

// withdraw cancel is a DELETE carrying coinWithdrawalId in the query.
func TestWithdrawCancelIsDELETE(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true}`, nil)}
	out, _, code := runCLI(
		[]string{"withdraw", "cancel", "--id", "1234", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if doer.last.Method != "DELETE" || doer.last.URL.Path != "/v2/coin/withdrawal" {
		t.Fatalf("request = %s %s", doer.last.Method, doer.last.URL.Path)
	}
	if doer.last.URL.Query().Get("coinWithdrawalId") != "1234" {
		t.Fatalf("query = %s", doer.last.URL.RawQuery)
	}
	if !strings.Contains(out, "withdrawal cancellation accepted") {
		t.Fatalf("expected human ack: %s", out)
	}
}

func TestWithdrawCancelRequiresID(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	_, _, code := runCLI(
		[]string{"withdraw", "cancel", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, &stubDoer{})
	if code != 2 {
		t.Fatalf("missing --id should be a usage error (exit 2), got %d", code)
	}
}

// KindCurrency rejects a non-currency token (e.g. a trading pair) before any call.
func TestFundingRejectsBadCurrency(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{}
	_, stderr, code := runCLI(
		[]string{"deposit", "history", "btc_krw", "--key", "bot", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 2 {
		t.Fatalf("invalid currency should be exit 2, got %d (%s)", code, stderr)
	}
	if doer.last != nil {
		t.Fatalf("must reject before sending a request")
	}
}

// permFromSetupOutput extracts the prefilled `permissions` from the
// registrationLink in a setup/key JSON result.
func permFromSetupOutput(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		RegistrationLink string `json:"registrationLink"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil {
		t.Fatalf("parse setup output: %v (%s)", err, out)
	}
	u, err := url.Parse(doc.RegistrationLink)
	if err != nil {
		t.Fatalf("registrationLink is not a URL: %v", err)
	}
	return u.Query().Get("permissions")
}

func TestSetupDefaultPermissionsExcludeTransfers(t *testing.T) {
	home := t.TempDir()
	out, _, code := runWithDeps([]string{"setup", "--name", "trade", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := permFromSetupOutput(t, out); got != "47" {
		t.Fatalf("default setup permissions = %s, want 47 (no transfer write bits)", got)
	}
}

func TestSetupWithTransfersWidensPermissions(t *testing.T) {
	home := t.TempDir()
	out, _, code := runWithDeps([]string{"setup", "--name", "xfer", "--with-transfers", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": home}, nil, fakeProbe("203.0.113.7", ""))
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if got := permFromSetupOutput(t, out); got != "127" {
		t.Fatalf("setup --with-transfers permissions = %s, want 127 (all scopes)", got)
	}
}

// KRW deposit is a POST push; its human output is the confirmation message.
func TestKrwDepositPushAndAck(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200, `{"success":true}`, nil)}
	out, _, code := runCLI(
		[]string{"krw", "deposit", "request", "50000", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if doer.last.Method != "POST" || doer.last.URL.Path != "/v2/krw/sendKrwDepositPush" {
		t.Fatalf("request = %s %s", doer.last.Method, doer.last.URL.Path)
	}
	if !strings.Contains(doer.body, "amount=50000") {
		t.Fatalf("POST body missing amount: %q", doer.body)
	}
	if !strings.Contains(out, "KRW deposit push sent") {
		t.Fatalf("expected human ack message: %s", out)
	}
}

// The human-readable (default) mode renders a funding list as a table with
// thousand-grouped amounts.
func TestKrwDepositsHumanTable(t *testing.T) {
	home := t.TempDir()
	seedBoundKey(t, home)
	doer := &stubDoer{resp: resp(200,
		`{"success":true,"data":[{"id":1234,"status":"done","quantity":"50000","createdAt":1700000000000}]}`, nil)}
	out, _, code := runCLI(
		[]string{"krw", "deposit", "history", "--key", "bot"},
		map[string]string{"KORBIT_CLI_HOME": home}, doer)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "status") || !strings.Contains(out, "50,000") {
		t.Fatalf("expected a human table with grouped amount: %s", out)
	}
}
