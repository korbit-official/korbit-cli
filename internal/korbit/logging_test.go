// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/logging"
)

// signedClient builds a Client over the stub d with a real ED25519 signer, so
// its logs can be checked for a secret leak.
func signedClient(t *testing.T, d Doer, buf *strings.Builder) *Client {
	t.Helper()
	creds := testKey(t)
	return &Client{
		BaseURL:  "https://api.example.com",
		Doer:     d,
		Creds:    &creds,
		KeyName:  "trading",
		APIKeyID: "PUBKEYID",
		Clock:    fakeClock{now: func() int64 { return 1700000000000 }},
		Log:      logging.New(buf, slog.LevelDebug),
	}
}

func TestDoLogsSendAndResponse(t *testing.T) {
	var buf strings.Builder
	d := &stub{resp: mkResp(200, `{"success":true,"data":{"ok":1}}`, nil)}
	c := signedClient(t, d, &buf)

	_, _, err := c.Do(context.Background(), Call{Method: "POST", Path: "/v2/orders", Params: []KV{{"symbol", "btc_krw"}}, Auth: true}, Policy{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"http send",
		"method=POST",
		"path=/v2/orders",
		"host=api.example.com",
		"http ok",
		"scheme=ed25519",
		"apiKeyId=PUBKEYID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// TestDoLogNeverLeaksSecret asserts neither the signature nor any private-key
// material reaches the log, and that the signed query string is not logged.
func TestDoLogNeverLeaksSecret(t *testing.T) {
	var buf strings.Builder
	d := &stub{resp: mkResp(200, `{"success":true,"data":{}}`, nil)}
	c := signedClient(t, d, &buf)

	_, _, _ = c.Do(context.Background(), Call{Method: "GET", Path: "/v2/orders/open", Auth: true}, Policy{})

	out := buf.String()
	// The actually-sent signature value (from the stub's recorded request).
	sentURL := d.got.URL.String()
	if i := strings.Index(sentURL, "signature="); i >= 0 {
		sig := sentURL[i+len("signature="):]
		if sig != "" && strings.Contains(out, sig) {
			t.Fatalf("log leaked the signature value")
		}
	}
	for _, bad := range []string{"PRIVATE KEY", "signature=", "BEGIN"} {
		if strings.Contains(out, bad) {
			t.Fatalf("log leaked %q\n--- log ---\n%s", bad, out)
		}
	}
}

func TestDoLogsTransportErrorAtWarn(t *testing.T) {
	var buf strings.Builder
	d := &stub{err: errors.New("dial tcp: connection refused")}
	c := signedClient(t, d, &buf)

	_, _, err := c.Do(context.Background(), Call{Method: "GET", Path: "/v2/orders/open", Auth: true}, Policy{})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	out := buf.String()
	if !strings.Contains(out, "warn: http transport error") {
		t.Fatalf("missing Warn transport line\n--- log ---\n%s", out)
	}
	if !strings.Contains(out, "connection refused") || !strings.Contains(out, "host=api.example.com") {
		t.Fatalf("transport log lacks context\n--- log ---\n%s", out)
	}
}

func TestExecuteLogsUnparseableEnvelope(t *testing.T) {
	var buf strings.Builder
	log := logging.New(&buf, slog.LevelDebug)
	d := &stub{resp: mkResp(502, "<html>502 Bad Gateway</html>", nil)}

	_, err := executeCtxLog(context.Background(), Request{Method: "GET", Path: "/v2/time"}, Options{Doer: d}, log)
	if err == nil {
		t.Fatal("expected an error for a non-envelope body")
	}
	out := buf.String()
	if !strings.Contains(out, "response not a JSON envelope") || !strings.Contains(out, "bodyLen=") {
		t.Fatalf("missing envelope-parse Debug line\n--- log ---\n%s", out)
	}
	if strings.Contains(out, "Bad Gateway") {
		t.Fatalf("log must not contain the raw body\n--- log ---\n%s", out)
	}
}

func TestMeasureClockOffsetLogsProbes(t *testing.T) {
	var buf strings.Builder
	const srv = 1_000_000
	d := &clockStub{results: []clockProbe{{serverTime: srv}, {serverTime: srv}}}
	opts := Options{Doer: d, Now: seqNow(0, 100, 200, 220)}

	if _, err := MeasureClockOffset(opts, 2, logging.New(&buf, slog.LevelDebug)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"clock probe", "rttMs=", "clock offset measured", "offsetMs="} {
		if !strings.Contains(out, want) {
			t.Errorf("clock log missing %q\n--- log ---\n%s", want, out)
		}
	}
}
