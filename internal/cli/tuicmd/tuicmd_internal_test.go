// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tuicmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
	"github.com/digitalx-official/digitalx-cli/internal/stream"
)

// The subscription set: ticker + account channels cover EVERY symbol, while the
// heavier orderbook/trade channels cover only the active symbol (the TUI moves
// those dynamically on a switch). Only the trade channel seeds REST history.
func TestTUISubscriptionsShape(t *testing.T) {
	syms := []string{"btc_krw", "eth_krw"}
	const active = "btc_krw"

	check := func(t *testing.T, subs []stream.Subscription) {
		t.Helper()
		for _, s := range subs {
			switch s.Channel {
			case stream.ChannelTicker, stream.ChannelMyOrder, stream.ChannelMyTrade:
				if strings.Join(s.Symbols, ",") != strings.Join(syms, ",") {
					t.Errorf("%s should cover every symbol, got %v", s.Channel, s.Symbols)
				}
			case stream.ChannelOrderbook, stream.ChannelTrade:
				if len(s.Symbols) != 1 || s.Symbols[0] != active {
					t.Errorf("%s should cover only the active symbol, got %v", s.Channel, s.Symbols)
				}
			}
			if s.Channel == stream.ChannelTrade {
				if s.TradeHistory != tuiTradeHistory {
					t.Errorf("trade channel TradeHistory = %d, want %d", s.TradeHistory, tuiTradeHistory)
				}
			} else if s.TradeHistory != 0 {
				t.Errorf("%s channel must not request trade history, got %d", s.Channel, s.TradeHistory)
			}
		}
	}

	t.Run("public", func(t *testing.T) {
		subs := tuiSubscriptions(syms, active, false, nil)
		if len(subs) != 3 {
			t.Fatalf("public mode: want 3 market channels, got %d", len(subs))
		}
		check(t, subs)
	})

	t.Run("private", func(t *testing.T) {
		subs := tuiSubscriptions(syms, active, true, []int{0})
		if len(subs) != 6 {
			t.Fatalf("private mode: want 6 channels, got %d", len(subs))
		}
		check(t, subs)
		// No --account-seq: private channels are pinned to the main account so
		// live frames carry accountSeq.
		for _, s := range subs {
			private := s.Channel == stream.ChannelMyOrder || s.Channel == stream.ChannelMyTrade || s.Channel == stream.ChannelMyAsset
			if private {
				if len(s.AccountSeqs) != 1 || s.AccountSeqs[0] != 1 {
					t.Errorf("%s: want AccountSeqs [1] without --account-seq, got %v", s.Channel, s.AccountSeqs)
				}
			} else if len(s.AccountSeqs) != 0 {
				t.Errorf("%s: public channel must not carry AccountSeqs, got %v", s.Channel, s.AccountSeqs)
			}
		}
	})

	t.Run("private with several account-seqs", func(t *testing.T) {
		subs := tuiSubscriptions(syms, active, true, []int{1, 2})
		check(t, subs)
		// A multi-account session subscribes every private channel to ALL its
		// sub-accounts up front — switching accounts later is a tracking/UI
		// change, never a WS resubscribe.
		for _, s := range subs {
			private := s.Channel == stream.ChannelMyOrder || s.Channel == stream.ChannelMyTrade || s.Channel == stream.ChannelMyAsset
			if private {
				if len(s.AccountSeqs) != 2 || s.AccountSeqs[0] != 1 || s.AccountSeqs[1] != 2 {
					t.Errorf("%s: want AccountSeqs [1 2], got %v", s.Channel, s.AccountSeqs)
				}
			} else if len(s.AccountSeqs) != 0 {
				t.Errorf("%s: public channel must not carry AccountSeqs, got %v", s.Channel, s.AccountSeqs)
			}
		}
	})

	t.Run("private with account-seq", func(t *testing.T) {
		subs := tuiSubscriptions(syms, active, true, []int{2})
		check(t, subs)
		// Every private channel is pinned to the chosen sub-account; the public
		// market channels never carry it.
		for _, s := range subs {
			private := s.Channel == stream.ChannelMyOrder || s.Channel == stream.ChannelMyTrade || s.Channel == stream.ChannelMyAsset
			if private {
				if len(s.AccountSeqs) != 1 || s.AccountSeqs[0] != 2 {
					t.Errorf("%s: want AccountSeqs [2], got %v", s.Channel, s.AccountSeqs)
				}
			} else if len(s.AccountSeqs) != 0 {
				t.Errorf("%s: public channel must not carry AccountSeqs, got %v", s.Channel, s.AccountSeqs)
			}
		}
	})
}

// preflightVerdict is the start/don't-start gate the TUI runs against
// /v2/currentKeyInfo before opening the alt-screen. A usable key/account returns
// nil (start); a DEFINITIVE key/config problem (apiclient.ClassFatal, or an unusable
// payload) is a ConfigError (exit 4) so the diagnostic lands on the terminal; a
// transient failure (network, 5xx, 429) and an unconverged clock resync
// (EXCEED_TIME_WINDOW) are non-fatal — the session starts and the stream layer
// recovers. Per-endpoint permissions are intentionally NOT gated here.
func TestPreflightVerdict(t *testing.T) {
	const nowMs = 1_700_000_000_000
	const seq = 2

	tests := []struct {
		name       string
		data       string
		err        error
		accountSeq int
		wantErr    bool
		wantSub    string
	}{
		{name: "healthy key starts", data: `{"status":"activated","allowedAccountSeqs":[1,2],"expiration":1800000000000}`, accountSeq: seq},
		{name: "no expiry and no allowed list starts", data: `{"status":"activated"}`, accountSeq: seq},
		// Non-fatal error classes: the config may be fine, so start anyway.
		{name: "network error is non-fatal", err: errors.New("dial tcp: connection refused"), accountSeq: seq},
		{name: "http 5xx is non-fatal", err: &output.ApiError{HTTPStatus: 503, Message: "bad gateway"}, accountSeq: seq},
		{name: "http 429 is non-fatal", err: &output.ApiError{HTTPStatus: 429, Message: "slow down"}, accountSeq: seq},
		{name: "clock resync unconverged is non-fatal", err: &output.ApiError{Code: "EXCEED_TIME_WINDOW", HTTPStatus: 400, Message: "bad ts"}, accountSeq: seq},
		// Definitive rejections and unusable payloads block the start (exit 4).
		{
			name:    "ip not allowlisted",
			err:     &output.ApiError{Code: "IP_NOT_ALLOWED", HTTPStatus: 403, Message: "forbidden"},
			wantErr: true, wantSub: "allowlisted",
		},
		{
			name:    "generic auth rejection",
			err:     &output.ApiError{Code: "KEY_NOT_FOUND", HTTPStatus: 404, Message: "no such key"},
			wantErr: true, wantSub: "KEY_NOT_FOUND",
		},
		{
			name:    "deactivated key",
			data:    `{"status":"deactivated","allowedAccountSeqs":[2]}`,
			wantErr: true, wantSub: "not activated",
		},
		{
			name:    "expired key",
			data:    `{"status":"activated","expiration":1699999999999,"allowedAccountSeqs":[2]}`,
			wantErr: true, wantSub: "expired",
		},
		{
			name:       "session account-seq not allowed",
			data:       `{"status":"activated","allowedAccountSeqs":[1,3]}`,
			accountSeq: seq, wantErr: true, wantSub: "not in this key's allowed accounts",
		},
		{
			name:       "empty allowed list is rejected",
			data:       `{"status":"activated","allowedAccountSeqs":[]}`,
			accountSeq: seq, wantErr: true, wantSub: "not in this key's allowed accounts",
		},
	}

	// A multi-account session must have EVERY sub-account allowed; the error
	// names exactly the missing ones.
	t.Run("multi account-seq partially allowed", func(t *testing.T) {
		data := json.RawMessage(`{"status":"activated","allowedAccountSeqs":[1,3]}`)
		if err := preflightVerdict(data, nil, []int{1, 3}, nowMs, nil); err != nil {
			t.Fatalf("all-allowed seqs must start: %v", err)
		}
		err := preflightVerdict(data, nil, []int{1, 2}, nowMs, nil)
		if err == nil || !strings.Contains(err.Error(), "[2]") {
			t.Fatalf("want a verdict naming the missing seq [2], got %v", err)
		}
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightVerdict(json.RawMessage(tc.data), tc.err, []int{tc.accountSeq}, nowMs, nil)
			if tc.wantErr != (err != nil) {
				t.Fatalf("preflightVerdict err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			// Every blocking verdict is a ConfigError (exit 4) so the terminal shows
			// a "fix your key/config" diagnostic.
			var cfgErr *output.ConfigError
			if !errors.As(err, &cfgErr) {
				t.Errorf("want ConfigError (exit 4), got %T: %v", err, err)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestAllowedAccountSeqs(t *testing.T) {
	if got := allowedAccountSeqs(nil); got != nil {
		t.Fatalf("nil data = %v, want nil", got)
	}
	if got := allowedAccountSeqs(json.RawMessage(`{"status":"activated"}`)); got != nil {
		t.Fatalf("absent field = %v, want nil (distinct from empty)", got)
	}
	if got := allowedAccountSeqs(json.RawMessage(`{"allowedAccountSeqs":[]}`)); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %v, want a present-but-empty slice", got)
	}
	got := allowedAccountSeqs(json.RawMessage(`{"allowedAccountSeqs":[1,3,2]}`))
	if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 2 {
		t.Fatalf("got %v, want [1 3 2] (verbatim)", got)
	}
}

// resolveTUIAccounts turns the --account-seq selection + the key's default and
// allowedAccountSeqs into the subscribe set and the active account.
func TestResolveTUIAccounts(t *testing.T) {
	t.Run("explicit list: verbatim, active on the first", func(t *testing.T) {
		active, subs, err := resolveTUIAccounts([]int{2, 1, 3}, "1", []int{1, 2, 3}, nil)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if active != 2 || len(subs) != 3 || subs[0] != 2 {
			t.Fatalf("active=%d subs=%v, want active 2 and the list verbatim", active, subs)
		}
	})
	t.Run("omitted: whole allowed set, active on the key default", func(t *testing.T) {
		active, subs, err := resolveTUIAccounts(nil, "2", []int{3, 1, 2}, nil)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if active != 2 {
			t.Fatalf("active=%d, want the key default 2", active)
		}
		if len(subs) != 3 || subs[0] != 1 || subs[1] != 2 || subs[2] != 3 {
			t.Fatalf("subs=%v, want the allowed set sorted ascending", subs)
		}
	})
	t.Run("omitted, no default: active main when 1 is allowed", func(t *testing.T) {
		active, subs, err := resolveTUIAccounts(nil, "", []int{1, 2}, nil)
		if err != nil || active != 1 || len(subs) != 2 {
			t.Fatalf("active=%d subs=%v err=%v, want active 1 over [1 2]", active, subs, err)
		}
	})
	t.Run("omitted, default resolves to 1 but 1 not allowed: error", func(t *testing.T) {
		_, _, err := resolveTUIAccounts(nil, "", []int{2, 3}, nil)
		if err == nil {
			t.Fatalf("want an error when the default sub-account is not allowed")
		}
		var cfgErr *output.ConfigError
		if !errors.As(err, &cfgErr) {
			t.Fatalf("want ConfigError (exit 4), got %T: %v", err, err)
		}
	})
	t.Run("omitted, transient key-info failure: refuses (can't expand omission)", func(t *testing.T) {
		// A network/5xx/429 failure classifies non-fatal, so the omission ("all
		// accounts") is unknowable and must not silently narrow to one account.
		_, _, err := resolveTUIAccounts(nil, "2", nil, errors.New("network down"))
		if err == nil {
			t.Fatal("want an error when the allowed set can't be resolved and --account-seq was omitted")
		}
	})
	t.Run("omitted, ClassFatal key-info failure: falls back for preflightVerdict", func(t *testing.T) {
		// A definitive rejection (4xx w/ code) falls through to the single-account
		// fallback so preflightVerdict reports the specific cause on the terminal.
		fatal := &output.ApiError{Code: "INVALID_KEY", HTTPStatus: 401}
		active, subs, err := resolveTUIAccounts(nil, "2", nil, fatal)
		if err != nil || active != 2 || len(subs) != 1 || subs[0] != 2 {
			t.Fatalf("active=%d subs=%v err=%v, want the single-account fallback [2]", active, subs, err)
		}
	})
	t.Run("omitted, server didn't report the field: single active fallback", func(t *testing.T) {
		// No error, but the server (older) didn't list allowedAccountSeqs — start on
		// the single active account rather than refuse.
		active, subs, err := resolveTUIAccounts(nil, "2", nil, nil)
		if err != nil || active != 2 || len(subs) != 1 || subs[0] != 2 {
			t.Fatalf("active=%d subs=%v err=%v, want the single-account fallback [2]", active, subs, err)
		}
	})
}

// A failed pair-listing read must still hand the panel the KRW market's
// documented bounds, so the TUI's ⚠ <min / ⚠ >max appear exactly where
// `order place --dry-run` raises them against the same failure. Returning the
// zero value here — which this seam used to do — silently removed a warning the
// CLI still gave, on the only market that exists today.
func TestResolveTUIBoundsKeepsTheFallbackOnAFailedRead(t *testing.T) {
	boom := errors.New("network down")

	got, err := resolveTUIBounds(nil, boom, "btc_krw")
	if !errors.Is(err, boom) {
		t.Fatalf("the error must ride along so the panel retries, got %v", err)
	}
	if got.Min != "5000" || got.Max != "1000000000" || got.QuoteCurrency != "krw" {
		t.Fatalf("a failed read must still yield the KRW fallback, got %+v", got)
	}

	// Any other market stays unbounded — nothing is invented for a market whose
	// figures this code has never known.
	if got, _ := resolveTUIBounds(nil, boom, "btc_xaut"); got.Min != "" || got.Max != "" {
		t.Fatalf("a failed read must not invent bounds off the KRW market: %+v", got)
	}

	// A listing that DID arrive is used verbatim, bounds and all.
	ok, err := resolveTUIBounds([]rawapi.Pair{
		{Symbol: "btc_krw", QuoteCurrency: "krw", MinOrderValue: "7000"},
	}, nil, "btc_krw")
	if err != nil || ok.Min != "7000" {
		t.Fatalf("a landed listing must win: %+v err=%v", ok, err)
	}
}
