// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package botapi

import (
	"strings"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/spec"
)

// TestMonitorExamplesCompile guards the monitor command's documented examples:
// every --where/--on/--init snippet shipped in help text must be valid JS that
// the runtime can actually compile. An example is copy-pasted by agents, so a
// snippet that fails to parse (e.g. statement syntax in the expression-only
// --where) is a shipped-broken instruction, not a runtime bug. New compiles
// --where/--on and runs --init, so a bad snippet fails here.
//
// The monitor examples' --init only uses the synchronous ta library (no
// korbit.*/db. calls), so a nil API is enough to drive compilation + init.
func TestMonitorExamplesCompile(t *testing.T) {
	cmd := spec.Find([]string{"monitor"})
	if cmd == nil {
		t.Fatal("monitor command not found in registry")
	}
	for _, ex := range cmd.Examples {
		args := splitArgs(ex)
		where := flagValue(args, "--where")
		on := flagValue(args, "--on")
		initSrc := flagValue(args, "--init")
		if where == "" && on == "" && initSrc == "" {
			continue // plain streaming / --jq example: no JS to compile
		}
		r, err := New(Options{Where: where, On: on, Init: initSrc})
		if err != nil {
			t.Errorf("monitor example failed to compile: %q\n  --where=%q --on=%q --init=%q\n  error: %v",
				ex, where, on, initSrc, err)
			continue
		}
		r.Close()
	}
}

// flagValue returns the token following the named flag, or "" if absent. The
// example args are already split so a quoted snippet is a single token.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// splitArgs splits a shell-style command line into tokens, treating a
// single-quoted run as one verbatim token. The monitor examples only use
// single quotes (no escapes), which is all this needs to handle.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote, hasTok := false, false
	flush := func() {
		if hasTok {
			args = append(args, cur.String())
			cur.Reset()
			hasTok = false
		}
	}
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			hasTok = true
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
			hasTok = true
		}
	}
	flush()
	return args
}
