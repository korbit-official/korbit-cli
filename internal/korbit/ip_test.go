// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package korbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeIPReadsPlaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ipPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		// Trailing whitespace must be trimmed by the prober.
		w.Write([]byte("203.0.113.7\n"))
	}))
	defer srv.Close()

	ip, err := ProbeIP(context.Background(), &http.Client{}, srv.URL, "")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if ip != "203.0.113.7" {
		t.Fatalf("ip = %q, want 203.0.113.7 (trimmed)", ip)
	}
}

func TestProbeIPNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := ProbeIP(context.Background(), &http.Client{}, srv.URL, ""); err == nil {
		t.Fatalf("non-2xx should be an error")
	}
}
