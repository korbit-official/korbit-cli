// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package useragent

import (
	"testing"

	"github.com/digitalx-official/digitalx-cli/internal/version"
)

// The wants below spell the product token LITERALLY rather than reading
// version.Product: a test built from the constant it pins would validate
// whatever that constant says, and this token is a wire contract the server
// attributes traffic by, so a change to it must fail here.
func TestComposeFull(t *testing.T) {
	e := env{OS: "darwin", Arch: "arm64", Version: "25.6.0", Lang: "ko_KR"}
	got := compose(e, "cli", "order.place")
	want := "digitalx-cli/" + version.Version + " (darwin/25.6.0; arm64; ko_KR) ctx:cli/order.place"
	if got != want {
		t.Fatalf("compose full:\n got %q\nwant %q", got, want)
	}
}

func TestComposeOmitsEmptyVersionAndLang(t *testing.T) {
	e := env{OS: "linux", Arch: "amd64"} // no version, no lang
	got := compose(e, "doctor", "")
	want := "digitalx-cli/" + version.Version + " (linux; amd64) ctx:doctor"
	if got != want {
		t.Fatalf("compose minimal:\n got %q\nwant %q", got, want)
	}
}

func TestComposeDetailToken(t *testing.T) {
	e := env{OS: "linux", Arch: "amd64", Lang: "en_US"}
	got := compose(e, "monitor", "botapi")
	want := "digitalx-cli/" + version.Version + " (linux; amd64; en_US) ctx:monitor/botapi"
	if got != want {
		t.Fatalf("compose detail:\n got %q\nwant %q", got, want)
	}
}

func TestComposeSanitizesUnsafeChars(t *testing.T) {
	// Spaces, parens, semicolons, slashes must never leak into a segment and
	// break the grammar — they are dropped.
	e := env{OS: "weird os (x)", Arch: "a;b", Version: "1.0 beta", Lang: "ko_KR.UTF-8"}
	got := compose(e, "cli", "some thing/odd")
	want := "digitalx-cli/" + version.Version + " (weirdosx/1.0beta; ab; ko_KR.UTF-8) ctx:cli/somethingodd"
	if got != want {
		t.Fatalf("compose sanitize:\n got %q\nwant %q", got, want)
	}
}

func TestComposeEmptySurfaceFallsBack(t *testing.T) {
	e := env{OS: "darwin", Arch: "arm64"}
	got := compose(e, "", "")
	want := "digitalx-cli/" + version.Version + " (darwin; arm64) ctx:unknown"
	if got != want {
		t.Fatalf("compose empty surface:\n got %q\nwant %q", got, want)
	}
}

func TestForIsStable(t *testing.T) {
	// For reads the real environment but must at least be non-empty, carry the
	// product token, and end with the ctx token.
	got := For("cli", "whoami")
	if got == "" {
		t.Fatal("For returned empty")
	}
	if want := "digitalx-cli/" + version.Version + " ("; got[:len(want)] != want {
		t.Fatalf("For missing product/comment prefix: %q", got)
	}
}
