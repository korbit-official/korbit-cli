// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandboxcmd

import (
	"errors"
	"os/exec"
	goruntime "runtime"
	"testing"

	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
)

// passthroughExit forwards a streamed sandbox subprocess's non-zero exit as the
// CLI's exit code (via clienv.ExitError, which prints no envelope) and surfaces any
// other failure unchanged.
func TestPassthroughExit(t *testing.T) {
	if got := passthroughExit(nil); got != nil {
		t.Errorf("nil should stay nil, got %v", got)
	}

	if goruntime.GOOS != "windows" {
		// A real non-zero subprocess exit → clienv.ExitError carrying that exact code…
		runErr := exec.Command("sh", "-c", "exit 2").Run()
		got := passthroughExit(runErr)
		var ee clienv.ExitError
		if !errors.As(got, &ee) {
			t.Fatalf("a bundle exit should map to clienv.ExitError, got %T %v", got, got)
		}
		if ee.Code != 2 {
			t.Errorf("exit code should pass through as 2, got %d", ee.Code)
		}
		// …and ExitError carries no error envelope text.
		if ee.Error() != "" {
			t.Errorf("ExitError must print no envelope, got %q", ee.Error())
		}
	}

	// A non-exit failure (e.g. runtime/bundle resolution) surfaces unchanged.
	plain := errors.New("deno not found")
	if got := passthroughExit(plain); got != plain {
		t.Errorf("a non-exit error should pass through unchanged, got %v", got)
	}
}
