// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package stream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestDefaultDialerAgainstRealServer exercises the coder/websocket adapter
// over a real HTTP upgrade: handshake, header passthrough, text round-trip,
// ping/pong, and close.
func TestDefaultDialerAgainstRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reject" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":{"code":401,"message":"KEY_NOT_FOUND"}}`))
			return
		}
		if got := r.Header.Get("X-Test-Header"); got != "hello" {
			t.Errorf("header not passed through: %q", got)
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		// Echo one message, then keep reading (so client pings are answered)
		// until the client goes away.
		_, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
			return
		}
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	header := http.Header{}
	header.Set("X-Test-Header", "hello")
	conn, err := DefaultDialer(ctx, wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.Write(ctx, []byte(`{"hello":"ws"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Ping requires a concurrent Read; run the echo read alongside it the way
	// connManager's serve/pingLoop do.
	echoed := make(chan []byte, 1)
	go func() {
		first := true
		for {
			p, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if first {
				first = false
				echoed <- p
			}
		}
	}()
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	select {
	case p := <-echoed:
		if string(p) != `{"hello":"ws"}` {
			t.Fatalf("echo mismatch: %s", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("echo never arrived")
	}
}

func TestDefaultDialerParsesUpgradeRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":401,"message":"KEY_NOT_FOUND"}}`))
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := DefaultDialer(ctx, wsURL, nil)
	var ue *UpgradeError
	if !errors.As(err, &ue) {
		t.Fatalf("want UpgradeError, got %v", err)
	}
	if ue.Status != 401 || ue.Code != "KEY_NOT_FOUND" {
		t.Fatalf("unexpected UpgradeError: %+v", ue)
	}
}
