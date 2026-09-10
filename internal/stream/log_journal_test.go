// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/apiclient"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// syncBuf is a concurrency-safe io.Writer for capturing log output written from
// the session's goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestBackfillLogsWireRequests pins that every backfill REST request reaches the
// wire-layer per-request log — the guarantee that broke when the backfill client
// was built without its Log field (so a --log-level trace run showed no
// open-orders/balance request). The wire log now lives on the session's
// Config.Client (its Log field); the wire layer logs "http send" at Debug, so a
// Trace-level logger (one below Debug) captures it.
func TestBackfillLogsWireRequests(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/balance" {
			return ok(`[{"currency":"krw","available":"1000"}]`)
		}
		return 404, `{}`
	}}
	buf := &syncBuf{}
	// The wire log lives on the Client (its Log field), not on Config.Log — wire
	// it to the trace-level logger so the backfill REST request is captured.
	client := testClient("http://example.test", doer, auth)
	client.Log = logging.New(buf, logging.LevelTrace)
	tp, _ := startSession(t, Config{
		PrivateURL:    "ws://example.test/v2/private",
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        client,
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone)

	out := buf.String()
	if !strings.Contains(out, "http send") || !strings.Contains(out, "path=/v2/balance") {
		t.Fatalf("backfill REST request was not logged at the wire layer; log was:\n%s", out)
	}
}

// fakeRecorder is a spy apiclient.Recorder capturing one Ready invocation.
type fakeRecorder struct {
	onReady func(apiclient.CallInfo)
}

func (f *fakeRecorder) Ready(info apiclient.CallInfo) error {
	if f.onReady != nil {
		f.onReady(info)
	}
	return nil
}

func (f *fakeRecorder) Record(apiclient.CallInfo, apiclient.Outcome) int64 { return 0 }

// TestBackfillRoutesThroughRecorder pins that backfill reads consult the
// journaling policy through the Client's own per-call recorder (Client.NewRecorder,
// a FRESH Recorder per Do) rather than deciding at the call site. The recovery
// read carries the "stream-backfill" surface and the authenticated flag, the
// inputs the policy reasons over (and exempts).
func TestBackfillRoutesThroughRecorder(t *testing.T) {
	auth, _ := testAuth(t)
	conn := newFakeConn()
	dialer := &fakeDialer{script: connScript(conn)}
	doer := &stubDoer{handle: func(path string, _ url.Values) (int, string) {
		if path == "/v2/balance" {
			return ok(`[{"currency":"krw","available":"1000"}]`)
		}
		return 404, `{}`
	}}

	var mu sync.Mutex
	var factoryCalls int
	var readyInfos []apiclient.CallInfo
	client := testClient("http://example.test", doer, auth)
	client.NewRecorder = func(_ context.Context, _ apiclient.Call) apiclient.Recorder {
		mu.Lock()
		factoryCalls++
		mu.Unlock()
		return &fakeRecorder{onReady: func(info apiclient.CallInfo) {
			mu.Lock()
			readyInfos = append(readyInfos, info)
			mu.Unlock()
		}}
	}

	tp, _ := startSession(t, Config{
		PrivateURL:    "ws://example.test/v2/private",
		Subscriptions: []Subscription{{Channel: ChannelMyAsset}},
		Client:        client,
		Dial:          dialer.dial,
		Tunables:      testTunables(),
	})

	tp.waitNotice(Connected)
	tp.waitNotice(BackfillDone)

	mu.Lock()
	defer mu.Unlock()
	if factoryCalls == 0 {
		t.Fatal("the client's NewRecorder was never consulted for a backfill read")
	}
	var sawBalance bool
	for _, info := range readyInfos {
		if info.Path == "/v2/balance" {
			sawBalance = true
			if !info.Auth {
				t.Errorf("balance backfill should be authenticated, got Auth=false")
			}
			if info.Origin.Surface != "stream-backfill" {
				t.Errorf("backfill surface = %q, want %q", info.Origin.Surface, "stream-backfill")
			}
		}
	}
	if !sawBalance {
		t.Fatalf("recorder was not asked to gate the /v2/balance backfill read; saw %d Ready calls", len(readyInfos))
	}
}
