// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
)

// End-to-end tests against a live server (normally the Korbit API Sandbox,
// which implements both WebSocket endpoints with a real signature verifier).
// Skipped unless configured:
//
//	KORBIT_STREAM_E2E_BASE=http://127.0.0.1:9971   (REST base; ws:// is derived)
//	KORBIT_STREAM_E2E_KEY_ID=...                   (optional: enables the private test)
//	KORBIT_STREAM_E2E_PEM_FILE=/path/to/key.pem    (optional: enables the private test)
func e2eBase(t *testing.T) (rest, ws string) {
	t.Helper()
	base := os.Getenv("KORBIT_STREAM_E2E_BASE")
	if base == "" {
		t.Skip("KORBIT_STREAM_E2E_BASE not set")
	}
	return base, "ws" + strings.TrimPrefix(base, "http")
}

func TestE2EPublicStream(t *testing.T) {
	rest, ws := e2eBase(t)
	s, err := New(Config{
		PublicURL: ws + "/v2/public",
		Subscriptions: []Subscription{
			{Channel: ChannelTicker, Symbols: []string{"btc_krw"}},
			{Channel: ChannelOrderbook, Symbols: []string{"btc_krw"}},
			{Channel: ChannelTrade, Symbols: []string{"btc_krw"}},
		},
		Client: testClient(rest, nil, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	want := map[string]bool{ChannelTicker: false, ChannelOrderbook: false, ChannelTrade: false}
	deadline := time.After(15 * time.Second)
	for !(want[ChannelTicker] && want[ChannelOrderbook] && want[ChannelTrade]) {
		select {
		case ev := <-s.Events():
			if d, isData := ev.(Data); isData && d.Origin == OriginSnapshot {
				want[d.Channel] = true
			}
			if n, isNotice := ev.(Notice); isNotice {
				t.Logf("notice: %s %s", n.Code, n.Message)
				if n.Code == Fatal {
					t.Fatalf("fatal: %s", n.Message)
				}
			}
		case <-deadline:
			t.Fatalf("snapshots missing: %v", want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestE2EPublicSoak holds a public connection open across multiple ping
// intervals (both directions: our client pings and the server's heartbeat)
// and fails on any disconnect. Set KORBIT_STREAM_E2E_SOAK_MS (e.g. 40000).
func TestE2EPublicSoak(t *testing.T) {
	rest, ws := e2eBase(t)
	soakMs := os.Getenv("KORBIT_STREAM_E2E_SOAK_MS")
	if soakMs == "" {
		t.Skip("KORBIT_STREAM_E2E_SOAK_MS not set")
	}
	dur, err := time.ParseDuration(soakMs + "ms")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		PublicURL:     ws + "/v2/public",
		Subscriptions: []Subscription{{Channel: ChannelTicker, Symbols: []string{"btc_krw"}}},
		Client:        testClient(rest, nil, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	dataCount := 0
	deadline := time.After(dur)
	for {
		select {
		case ev := <-s.Events():
			switch v := ev.(type) {
			case Data:
				dataCount++
			case Notice:
				t.Logf("notice: %s %s", v.Code, v.Message)
				if v.Code == Disconnected || v.Code == Fatal || v.Code == ConnectionUnreliable {
					t.Fatalf("connection did not stay healthy: %s %s", v.Code, v.Message)
				}
			}
		case <-deadline:
			t.Logf("soak ok: %d data events over %s", dataCount, dur)
			cancel()
			<-done
			return
		}
	}
}

func TestE2EPrivateStream(t *testing.T) {
	rest, ws := e2eBase(t)
	keyID := os.Getenv("KORBIT_STREAM_E2E_KEY_ID")
	pemFile := os.Getenv("KORBIT_STREAM_E2E_PEM_FILE")
	if keyID == "" || pemFile == "" {
		t.Skip("KORBIT_STREAM_E2E_KEY_ID / KORBIT_STREAM_E2E_PEM_FILE not set")
	}
	pemBytes, err := os.ReadFile(pemFile)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := apiclient.ParsePrivatePEM(string(pemBytes))
	if err != nil {
		t.Fatal(err)
	}
	auth := &apiclient.Credentials{APIKeyID: keyID, Signer: apiclient.NewEd25519Signer(priv)}

	s, err := New(Config{
		PrivateURL: ws + "/v2/private",
		Subscriptions: []Subscription{
			{Channel: ChannelMyAsset},
			{Channel: ChannelMyOrder, Symbols: []string{"btc_krw"}},
			{Channel: ChannelMyTrade, Symbols: []string{"btc_krw"}},
		},
		Client: testClient(rest, nil, auth),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Phase 1: the initial backfill must deliver balances + open orders.
	sawBalance, sawOpenOrders, sawBackfillDone := false, false, false
	deadline := time.After(15 * time.Second)
	for !(sawBalance && sawOpenOrders && sawBackfillDone) {
		select {
		case ev := <-s.Events():
			switch v := ev.(type) {
			case Data:
				if v.Source == "/v2/balance" {
					sawBalance = true
				}
				if v.Source == "/v2/openOrders" {
					sawOpenOrders = true
				}
			case Notice:
				t.Logf("notice: %s %s", v.Code, v.Message)
				switch v.Code {
				case BackfillDone:
					sawBackfillDone = true
				case Fatal, BackfillFailed:
					t.Fatalf("%s: %s", v.Code, v.Message)
				}
			}
		case <-deadline:
			t.Fatalf("backfill incomplete: balance=%v openOrders=%v done=%v", sawBalance, sawOpenOrders, sawBackfillDone)
		}
	}

	// Phase 2: place a real signed order over REST and expect it live on the
	// myOrder channel.
	clientOrderID := fmt.Sprintf("stream-e2e-%d", time.Now().UnixMilli())
	_, err = apiclient.Execute(apiclient.Request{
		Method: "POST", Path: "/v2/orders", Auth: true,
		Params: []apiclient.KV{
			{Key: "symbol", Value: "btc_krw"},
			{Key: "side", Value: "buy"},
			{Key: "price", Value: "10000000"},
			{Key: "qty", Value: "0.001"},
			{Key: "orderType", Value: "limit"},
			{Key: "clientOrderId", Value: clientOrderID},
		},
	}, apiclient.Options{
		BaseURL: rest,
		Creds:   &apiclient.Credentials{APIKeyID: keyID, Signer: apiclient.NewEd25519Signer(priv)},
	})
	if err != nil {
		t.Fatalf("order place: %v", err)
	}

	deadline = time.After(15 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if d, isData := ev.(Data); isData && d.Channel == ChannelMyOrder && d.Origin == OriginRealtime {
				if strings.Contains(string(d.Payload), clientOrderID) {
					cancel()
					<-done
					return
				}
			}
		case <-deadline:
			t.Fatal("the placed order never arrived on the myOrder channel")
		}
	}
}
