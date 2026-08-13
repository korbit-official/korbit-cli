// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keymgmtcmd

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli/probe"
	"github.com/korbit-official/korbit-cli/internal/keys"
	"github.com/korbit-official/korbit-cli/internal/korbit"
)

func TestRegistrationLink(t *testing.T) {
	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	link := registrationLink(probe.PortalURL, kp.PublicPEM, "trading-bot", []string{"203.0.113.7", "2001:db8:1:2::/64"}, permTrading)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link is not a valid URL: %v (%s)", err, link)
	}
	if u.Scheme != "https" || u.Host != "developers.korbit.co.kr" || u.Path != portalCreatePath {
		t.Fatalf("unexpected link base: %s", link)
	}
	q := u.Query()
	if got := q.Get("permissions"); got != strconv.Itoa(permTrading) {
		t.Fatalf("permissions = %q, want %d", got, permTrading)
	}
	if got := q.Get("label"); got != "trading-bot" {
		t.Fatalf("label = %q", got)
	}
	if got := q.Get("whitelist"); got != "203.0.113.7,2001:db8:1:2::/64" {
		t.Fatalf("whitelist = %q", got)
	}
	pk := q.Get("public_key")
	if strings.ContainsAny(pk, "+/=") {
		t.Fatalf("public_key must be base64url (no +,/,=): %q", pk)
	}
	der, err := base64.RawURLEncoding.DecodeString(pk)
	if err != nil {
		t.Fatalf("public_key not base64url: %v", err)
	}
	block, _ := pem.Decode([]byte(kp.PublicPEM))
	if block == nil || !bytes.Equal(der, block.Bytes) {
		t.Fatalf("public_key SPKI does not match the generated PEM")
	}
}

// TestNewKeyGuidanceLabelsLink pins the default key label prefilled into the
// registration deep link: "korbit-cli: <key name>", so a CLI-issued key is
// recognizable in the developers portal.
func TestNewKeyGuidanceLabelsLink(t *testing.T) {
	kp, err := korbit.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	link, _ := registrationLinkAndSteps(probe.PortalURL, "trading-bot", kp.PublicPEM, nil, permTrading)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link is not a valid URL: %v (%s)", err, link)
	}
	if got := u.Query().Get("label"); got != "korbit-cli: trading-bot" {
		t.Fatalf("label = %q, want %q", got, "korbit-cli: trading-bot")
	}
}

func TestRegistrationLinkOmitsEmptyWhitelist(t *testing.T) {
	kp, _ := korbit.GenerateKeypair()
	u, err := url.Parse(registrationLink(probe.PortalURL, kp.PublicPEM, "bot", nil, permTrading))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Query()["whitelist"]; ok {
		t.Fatal("whitelist must be absent when no allowlist is known")
	}
	if u.Query().Get("permissions") != strconv.Itoa(permTrading) {
		t.Fatal("permissions should still be prefilled")
	}
}

func TestRegistrationLinkEmptyOnBadPEM(t *testing.T) {
	if link := registrationLink(probe.PortalURL, "not a pem", "bot", nil, permTrading); link != "" {
		t.Fatalf("expected empty link for invalid PEM, got %q", link)
	}
}

// TestPermTradingScopes pins the security-relevant intent of the prefilled
// permission bitmask, not just its numeric value: all read scopes + writeOrders,
// within the portal's 1..127 range, and crucially WITHOUT the deposit/withdrawal
// write bits — this CLI must never ask the user to grant those.
func TestPermTradingScopes(t *testing.T) {
	const (
		readBalances     = 1
		readOrders       = 2
		writeOrders      = 4
		readDeposits     = 8
		writeDeposits    = 16
		readWithdrawals  = 32
		writeWithdrawals = 64
	)
	if permTrading != readBalances|readOrders|writeOrders|readDeposits|readWithdrawals {
		t.Fatalf("permTrading = %d, want all-reads|writeOrders (47)", permTrading)
	}
	if permTrading < 1 || permTrading > 127 {
		t.Fatalf("permTrading = %d is outside the portal's 1..127 range", permTrading)
	}
	if permTrading&writeDeposits != 0 || permTrading&writeWithdrawals != 0 {
		t.Fatalf("permTrading = %d must not include deposit/withdrawal write bits (use --with-transfers / PermAll)", permTrading)
	}
	// permTransfers is exactly the two write bits; PermAll is the default plus them
	// (the bitmask `--with-transfers` opts into), still within the portal's range.
	if permTransfers != writeDeposits|writeWithdrawals {
		t.Fatalf("permTransfers = %d, want writeDeposits|writeWithdrawals (80)", permTransfers)
	}
	if PermAll != 127 {
		t.Fatalf("PermAll = %d, want permTrading|permTransfers (127)", PermAll)
	}
	if PermAll&writeDeposits == 0 || PermAll&writeWithdrawals == 0 {
		t.Fatalf("PermAll = %d must include both deposit/withdrawal write bits", PermAll)
	}
}

// TestSetupDoneResultPointsToRecheckWhenReportMissing: a bound setup result with no
// attached health-check report — the interactive prompt exited (finish-later) while
// the auto-claim lane's check was still in flight, so `final` was snapshotted before
// its Doctor was set — must still point the user at an explicit re-check, never fall
// silent on a key that could carry a blocking issue.
func TestSetupDoneResultPointsToRecheckWhenReportMissing(t *testing.T) {
	var b bytes.Buffer
	id := "KEYID"
	setupDoneResult{Name: "default", Bound: true, Status: "configured", APIKeyID: &id}.FormatText(&b)
	out := b.String()
	if !strings.Contains(out, "Health check not completed") {
		t.Fatalf("a bound result without a report must warn the check did not complete: %s", out)
	}
	if !strings.Contains(out, "doctor --key default") {
		t.Fatalf("expected a key-scoped re-check command: %s", out)
	}
}

// TestSetupDoneResultInlineRecheckHasNoKeyFlag: an inline credential has no stored
// key to name, so the missing-report re-check pointer is a plain `doctor`.
func TestSetupDoneResultInlineRecheckHasNoKeyFlag(t *testing.T) {
	var b bytes.Buffer
	setupDoneResult{Name: keys.InlineDisplayName, Bound: true, Status: "configuredViaEnvironment"}.FormatText(&b)
	out := b.String()
	if !strings.Contains(out, "Health check not completed") {
		t.Fatalf("a bound result without a report must warn the check did not complete: %s", out)
	}
	if strings.Contains(out, "--key") {
		t.Fatalf("an inline re-check must not carry --key: %s", out)
	}
}
