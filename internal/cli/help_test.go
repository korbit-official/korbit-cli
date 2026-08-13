// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"strings"
	"testing"
)

// TestHelpGatesExperimentalSurface asserts the experimental gate reaches help:
// plain `--help` hides a command's experimental flags/notes/examples and shows a
// single pointer instead, while `--enable-experimental --help` (or the env var)
// reveals them, tagged. The monitor command is the one with an experimental
// surface today.
func TestHelpGatesExperimentalSurface(t *testing.T) {
	const (
		expFlagLine = "--where <string>" // an experimental flag's option line
		expTag      = "[experimental]"   // the per-flag tag shown only when revealed
		expNote     = "EXPERIMENTAL: the JavaScript bot runtime"
		pointer     = "Experimental options are hidden"
		stableFlag  = "--jq <string>" // a stable flag always shown
	)

	t.Run("plain help hides the experimental surface", func(t *testing.T) {
		out, _, code := runCLI([]string{"monitor", "--help"}, nil, nil)
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		for _, hidden := range []string{expFlagLine, expTag, expNote} {
			if strings.Contains(out, hidden) {
				t.Errorf("plain help must not show experimental content %q", hidden)
			}
		}
		if !strings.Contains(out, pointer) {
			t.Errorf("plain help must point at --enable-experimental; got:\n%s", out)
		}
		if !strings.Contains(out, stableFlag) {
			t.Errorf("plain help must still show stable flags like %q", stableFlag)
		}
	})

	t.Run("--enable-experimental --help reveals it", func(t *testing.T) {
		out, _, code := runCLI([]string{"monitor", "--enable-experimental", "--help"}, nil, nil)
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		for _, shown := range []string{expFlagLine, expTag, expNote} {
			if !strings.Contains(out, shown) {
				t.Errorf("experimental help must show %q; got:\n%s", shown, out)
			}
		}
		if strings.Contains(out, pointer) {
			t.Errorf("experimental help must not show the hidden-options pointer")
		}
	})

	t.Run("KORBIT_CLI_ENABLE_EXPERIMENTAL reveals it too", func(t *testing.T) {
		out, _, code := runCLI([]string{"monitor", "--help"},
			map[string]string{"KORBIT_CLI_ENABLE_EXPERIMENTAL": "1"}, nil)
		if code != 0 {
			t.Fatalf("exit=%d", code)
		}
		if !strings.Contains(out, expTag) {
			t.Errorf("env-enabled help must reveal the experimental surface; got:\n%s", out)
		}
	})
}
