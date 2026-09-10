// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package tuicmd

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/ops"
	"github.com/digitalx-official/digitalx-cli/internal/rawapi"
	"github.com/digitalx-official/digitalx-cli/internal/tui"
)

// scriptedWire is a canned rawapi Doer: it returns per-"METHOD path" payloads
// and records every call, so the tests can assert which endpoint a seam method
// hit and with which wire parameters.
type scriptedWire struct {
	replies map[string]json.RawMessage
	calls   []apiclient.Call
}

func (s *scriptedWire) Do(_ context.Context, call apiclient.Call, _ apiclient.Policy) (json.RawMessage, apiclient.Meta, error) {
	s.calls = append(s.calls, call)
	key := call.Method + " " + call.Path
	data, ok := s.replies[key]
	if !ok {
		return nil, apiclient.Meta{}, fmt.Errorf("unexpected call %s", key)
	}
	return data, apiclient.Meta{Attempts: 1}, nil
}

func (s *scriptedWire) param(i int, name string) string {
	for _, kv := range s.calls[i].Params {
		if kv.Key == name {
			return kv.Value
		}
	}
	return ""
}

func testFunding(replies map[string]json.RawMessage) (*tuiFunding, *scriptedWire) {
	wire := &scriptedWire{replies: replies}
	return &tuiFunding{api: &ops.API{Raw: rawapi.New(wire, nil)}}, wire
}

func TestFundingCurrenciesMapping(t *testing.T) {
	tf, _ := testFunding(map[string]json.RawMessage{
		"GET /v2/currencies": json.RawMessage(`[
			{"name":"btc","fullName":"Bitcoin","defaultNetwork":"BTC","networkList":[
				{"name":"BTC","withdrawalStatus":"launched","depositStatus":"stopped",
				 "withdrawalTxFee":"0.0009","withdrawalMinAmount":"0.001","withdrawalPrecision":8,"hasSecondaryAddr":false}]},
			{"name":"xrp","fullName":"Ripple","defaultNetwork":"XRP","networkList":[
				{"name":"XRP","hasSecondaryAddr":true}]},
			{"name":"krw","fullName":"Korean won"}]`),
	})
	got, err := tf.Currencies()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("currencies = %d, want 3", len(got))
	}
	btc := got[0].Networks[0]
	if btc.WithdrawalFee != "0.0009" || btc.WithdrawalMin != "0.001" || btc.WithdrawalPrecision != 8 {
		t.Fatalf("btc network constraints not mapped: %+v", btc)
	}
	if btc.DepositLaunched || !btc.WithdrawalLaunched {
		t.Fatalf("btc statuses: deposit stopped, withdrawal launched; got %+v", btc)
	}
	xrp := got[1].Networks[0]
	if !xrp.HasSecondaryAddress || xrp.WithdrawalPrecision != -1 {
		t.Fatalf("xrp: memo flag mapped, absent precision = -1; got %+v", xrp)
	}
	// Absent statuses are permissive: only an explicit "stopped" suspends.
	if !xrp.DepositLaunched || !xrp.WithdrawalLaunched {
		t.Fatalf("absent statuses must read as launched: %+v", xrp)
	}
	if len(got[2].Networks) != 0 {
		t.Fatalf("fiat carries no networks: %+v", got[2])
	}
}

func TestFundingHistoryRouting(t *testing.T) {
	tf, wire := testFunding(map[string]json.RawMessage{
		"GET /v2/krw/recentDeposits": json.RawMessage(`[{"id":7,"status":"done","quantity":"50000","createdAt":123}]`),
		"GET /v2/coin/recentWithdrawals": json.RawMessage(`[
			{"id":9,"currency":"btc","quantity":"0.01","fee":"0.0009","status":"reviewing",
			 "network":"BTC","address":"bc1q","transactionHash":"0xabc","createdAt":456}]`),
	})

	krw, err := tf.DepositHistory(1, "krw", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(krw) != 1 || krw[0].Currency != "krw" || krw[0].Amount != "50000" || krw[0].ID != 7 {
		t.Fatalf("krw rows mapped wrong: %+v", krw)
	}
	if wire.calls[0].Path != "/v2/krw/recentDeposits" {
		t.Fatalf("krw history must route to the KRW endpoint, hit %s", wire.calls[0].Path)
	}

	wd, err := tf.WithdrawHistory(1, "btc", 50)
	if err != nil {
		t.Fatal(err)
	}
	r := wd[0]
	if r.Amount != "0.01" || r.Fee != "0.0009" || r.TxHash != "0xabc" || r.Network != "BTC" {
		t.Fatalf("coin rows mapped wrong: %+v", r)
	}
	if got := wire.param(1, "currency"); got != "btc" {
		t.Fatalf("coin history must carry the currency, got %q", got)
	}
	if got := wire.param(1, "limit"); got != "50" {
		t.Fatalf("limit param = %q, want 50", got)
	}
}

func TestFundingRequestWithdrawalWire(t *testing.T) {
	tf, wire := testFunding(map[string]json.RawMessage{
		"POST /v2/coin/withdrawal": json.RawMessage(`{"status":"reviewing","coinWithdrawalId":9812}`),
	})
	rec, err := tf.RequestWithdrawal(2, tui.FundingWithdrawOrder{
		Currency: "btc", Network: "BTC", Amount: "0.01",
		Address: "bc1qexample", SecondaryAddress: "1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != 9812 || rec.Status != "reviewing" {
		t.Fatalf("receipt = %+v", rec)
	}
	for name, want := range map[string]string{
		"currency": "btc", "amount": "0.01", "address": "bc1qexample",
		"network": "BTC", "secondaryAddress": "1234",
		// The seam sends the accountSeq it was given (here 2) — never defaulting
		// to main; the server is what enforces funding's main-only rule.
		"accountSeq": "2",
	} {
		if got := wire.param(0, name); got != want {
			t.Fatalf("wire param %s = %q, want %q", name, got, want)
		}
	}
	if !wire.calls[0].Auth {
		t.Fatal("a withdrawal request must be signed")
	}
}

func TestFundingWithdrawablePicksCurrency(t *testing.T) {
	tf, _ := testFunding(map[string]json.RawMessage{
		"GET /v2/coin/withdrawableAmount": json.RawMessage(`[
			{"currency":"eth","withdrawableAmount":"1","withdrawalInUseAmount":"0"},
			{"currency":"btc","withdrawableAmount":"0.5","withdrawalInUseAmount":"0.1"}]`),
	})
	w, err := tf.Withdrawable(1, "btc")
	if err != nil {
		t.Fatal(err)
	}
	if w.Amount != "0.5" || w.InUse != "0.1" {
		t.Fatalf("withdrawable = %+v", w)
	}
}
