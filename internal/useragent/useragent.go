// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package useragent composes the User-Agent header sent on Digital X API requests
// (both REST and the WebSocket upgrade). The string identifies the CLI and its
// version, the host environment (OS name, OS/kernel version, CPU architecture,
// and locale), and the call context — which frontend and sub-context issued the
// call, so server-side traffic can be attributed to "a CLI command", "a monitor
// bot's API call", "the streaming socket", and so on.
//
// Format:
//
//	digitalx-cli/<version> (<os>[/<osver>]; <arch>[; <lang>]) ctx:<surface>[/<detail>]
//
// e.g.
//
//	digitalx-cli/1.2.3 (darwin/25.6.0; arm64; ko_KR) ctx:cli/order.place
//	digitalx-cli/1.2.3 (linux/6.1.0; amd64; en_US) ctx:monitor/botapi
//	digitalx-cli/1.2.3 (windows/10.0.22631; amd64) ctx:doctor
//
// It is applied ONLY to Digital X requests. This package deliberately lives ABOVE
// the wire layer (internal/apiclient): that package gathers no OS info and only
// carries the UserAgent seam, so callers compose the rich value here and inject
// it down. Nothing else (any future non-Digital X HTTP) should use this.
package useragent

import (
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/digitalx-official/digitalx-cli/internal/version"
)

// env is the host-environment snapshot that forms the comment block. It is
// process-constant, so current caches it; the pure formatter takes it as input
// so tests can pin every field deterministically.
type env struct {
	OS      string // runtime.GOOS, e.g. "darwin"
	Arch    string // runtime.GOARCH, e.g. "arm64"
	Version string // OS/kernel version; "" omits the segment
	Lang    string // locale, e.g. "ko_KR"; "" omits the segment
}

var (
	currentOnce sync.Once
	currentEnv  env
)

// current returns the cached host environment, gathering it once.
func current() env {
	currentOnce.Do(func() {
		currentEnv = env{
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Version: osVersion(),
			Lang:    osLang(),
		}
	})
	return currentEnv
}

// For returns the full User-Agent for a Digital X call attributed to the given
// surface (the frontend identity, e.g. "cli", "monitor", "doctor") and optional
// finer detail (e.g. the command key, or "botapi"). Both map straight onto the
// apiclient.Origin{Surface,Detail} the caller already sets on the request.
func For(surface, detail string) string { return compose(current(), surface, detail) }

// compose is the pure formatter: it never reads the environment, so it is fully
// testable.
func compose(e env, surface, detail string) string {
	var b strings.Builder
	b.WriteString(version.Token())
	b.WriteString(" (")
	b.WriteString(clean(e.OS))
	if v := clean(e.Version); v != "" {
		b.WriteByte('/')
		b.WriteString(v)
	}
	b.WriteString("; ")
	b.WriteString(clean(e.Arch))
	if l := clean(e.Lang); l != "" {
		b.WriteString("; ")
		b.WriteString(l)
	}
	b.WriteString(") ctx:")
	b.WriteString(ctxToken(surface, detail))
	return b.String()
}

// ctxToken renders the trailing surface[/detail] context, sanitized.
func ctxToken(surface, detail string) string {
	s := clean(surface)
	if s == "" {
		s = "unknown"
	}
	if d := clean(detail); d != "" {
		return s + "/" + d
	}
	return s
}

// clean keeps only characters that are safe inside a User-Agent token/comment
// without needing escaping or colliding with the format's own separators
// (space, ';', '(', ')', '/'), dropping everything else. The retained set —
// alphanumerics plus '.', '_', '-' — covers OS/kernel versions ("25.6.0"),
// locales ("ko_KR"), command keys ("order.place"), and surfaces
// ("stream-backfill") unchanged.
func clean(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// osLang returns the host locale from the standard POSIX environment variables,
// stripping any ".<codeset>" / "@<modifier>" suffix (so "ko_KR.UTF-8" → "ko_KR").
// The portable "C"/"POSIX" locales carry no real language and are treated as
// absent. This covers Unix hosts; on Windows these vars are usually unset, in
// which case the locale segment is simply omitted.
func osLang() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := os.Getenv(k)
		if i := strings.IndexAny(v, ".@"); i >= 0 {
			v = v[:i]
		}
		if v == "" || v == "C" || v == "POSIX" {
			continue
		}
		return v
	}
	return ""
}
