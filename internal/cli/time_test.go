// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"strings"
	"testing"
)

// TestTimeReportsSkew: `time` reports the local-clock skew alongside the server
// time, through the shared ops op (so `mcp serve`'s time tool carries the same
// fields). The request bracket is the real system clock (not injectable), so the
// exact offset is non-deterministic here — the arithmetic is pinned by
// TestWithTimeSkew in internal/ops. This asserts the skew fields are present, the
// server `time` is preserved verbatim, and the human output gains the skew line.
func TestTimeReportsSkew(t *testing.T) {
	doer := routeDoer{timeBody: `{"success":true,"data":{"time":1700000000250}}`}

	out, _, code := runCLI([]string{"time", "--json", "--compact"},
		map[string]string{"KORBIT_CLI_HOME": t.TempDir()}, doer)
	if code != 0 {
		t.Fatalf("exit = %d — %s", code, out)
	}
	for _, want := range []string{`"time":1700000000250`, `"localTime":`, `"offsetMs":`, `"rttMs":`, `"uncertaintyMs":`} {
		if !strings.Contains(out, want) {
			t.Fatalf("json missing %s: %s", want, out)
		}
	}

	human, _, code := runCLI([]string{"time"},
		map[string]string{"KORBIT_CLI_HOME": t.TempDir()}, doer)
	if code != 0 {
		t.Fatalf("human exit = %d — %s", code, human)
	}
	if !strings.Contains(human, "server time (unix ms): 1700000000250") {
		t.Fatalf("human missing server time: %s", human)
	}
	if !strings.Contains(human, "local clock:") || !strings.Contains(human, "the server") {
		t.Fatalf("human missing skew line: %s", human)
	}
}
