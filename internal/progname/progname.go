// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package progname holds the program's invoked name — the basename of the
// executable as it was run. The entrypoint sets it once at startup (before any
// goroutine), and every layer that builds help, examples, or guidance reads it
// here, so a binary renamed on disk produces matching output without the name
// being hardcoded anywhere.
package progname

// name is the resolved program name. It is written exactly once, by Set in
// main() before the command tree runs, and only read afterwards — so the
// concurrent reads from streaming commands (monitor, mcp) need no
// synchronization. The default is the name the binary ships as, used when the
// entrypoint supplies nothing (an unusual exec) and in tests, which call the
// command layer directly without going through main().
var name = "korbit"

// Name returns the program name to use in help, examples, and guidance strings.
func Name() string { return name }

// Set records the invoked program name. An empty value is ignored, leaving the
// default. Call this once at startup, before the command tree runs.
func Set(s string) {
	if s != "" {
		name = s
	}
}
