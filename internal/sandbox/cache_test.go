// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestWriteCacheReadme(t *testing.T) {
	dir := t.TempDir()
	writeCacheReadme(dir)
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("README not written: %v", err)
	}
	s := string(readme)
	if !strings.Contains(s, "docs.korbit.co.kr") {
		t.Error("README should point at the Official Source")
	}
	// Fork-safety pointer: the README must flag that the bundle is separately
	// licensed and where to read those terms.
	if !strings.Contains(s, "sandbox license") {
		t.Error("README must point at `sandbox license` for the bundle's terms")
	}
	// Leak/license contract: a short pointer is allowed (required above), but the
	// disclaimer BODY must never be reproduced in the CLI's open-source tree.
	low := bytes.ToLower(readme)
	for _, body := range []string{"as is", "warranty", "happy path", "no real money", "not the real exchange"} {
		if bytes.Contains(low, []byte(body)) {
			t.Errorf("README must NOT reproduce disclaimer body text (found %q)", body)
		}
	}
}

func TestLocalPathClassification(t *testing.T) {
	cases := map[string]string{
		"https://docs.korbit.co.kr/x.mjs": "",
		"http://localhost/x.mjs":          "",
		"file:///tmp/x.mjs":               "/tmp/x.mjs",
		"./rel/x.mjs":                     "./rel/x.mjs",
		"/abs/x.mjs":                      "/abs/x.mjs",
	}
	for src, want := range cases {
		if got := localPath(src); got != want {
			t.Errorf("localPath(%q) = %q, want %q", src, got, want)
		}
	}
}
