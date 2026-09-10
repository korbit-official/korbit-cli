// Copyright (c) 2026 Digital X Co., Ltd.
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

// setUserCacheBase points os.UserCacheDir at a fresh temp directory and returns
// it, so the ResolveCacheDir branches that probe the user cache never see the
// developer's real one.
func setUserCacheBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	return base
}

func cacheEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestResolveCacheDir(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		setUserCacheBase(t)
		got, err := ResolveCacheDir(cacheEnv(map[string]string{"DIGITALX_CLI_SANDBOX_CACHE": "/tmp/custom"}))
		if err != nil || got != "/tmp/custom" {
			t.Fatalf("ResolveCacheDir = %q, %v", got, err)
		}
	})

	t.Run("legacy env accepted", func(t *testing.T) {
		setUserCacheBase(t)
		got, err := ResolveCacheDir(cacheEnv(map[string]string{"KORBIT_CLI_SANDBOX_CACHE": "/tmp/legacy"}))
		if err != nil || got != "/tmp/legacy" {
			t.Fatalf("ResolveCacheDir = %q, %v", got, err)
		}
	})

	t.Run("current env wins over legacy", func(t *testing.T) {
		setUserCacheBase(t)
		got, err := ResolveCacheDir(cacheEnv(map[string]string{
			"DIGITALX_CLI_SANDBOX_CACHE": "/tmp/custom",
			"KORBIT_CLI_SANDBOX_CACHE":   "/tmp/legacy",
		}))
		if err != nil || got != "/tmp/custom" {
			t.Fatalf("ResolveCacheDir = %q, %v", got, err)
		}
	})

	t.Run("existing current directory", func(t *testing.T) {
		base := setUserCacheBase(t)
		if err := os.MkdirAll(filepath.Join(base, CacheDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, LegacyCacheDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveCacheDir(cacheEnv(nil))
		if want := filepath.Join(base, CacheDirName); err != nil || got != want {
			t.Fatalf("ResolveCacheDir = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("existing legacy directory", func(t *testing.T) {
		base := setUserCacheBase(t)
		if err := os.MkdirAll(filepath.Join(base, LegacyCacheDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveCacheDir(cacheEnv(nil))
		if want := filepath.Join(base, LegacyCacheDirName); err != nil || got != want {
			t.Fatalf("ResolveCacheDir = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("neither directory exists", func(t *testing.T) {
		base := setUserCacheBase(t)
		got, err := ResolveCacheDir(cacheEnv(nil))
		if want := filepath.Join(base, CacheDirName); err != nil || got != want {
			t.Fatalf("ResolveCacheDir = %q, %v; want %q", got, err, want)
		}
	})
}
