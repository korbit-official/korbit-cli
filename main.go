// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Command digitalx is a command-line client for the Digital X Open API v2 — market
// data, ED25519-signed trading, and key management — built as a stable,
// JSON-in/JSON-out tool surface for AI agents.
package main

import (
	"os"
	"path/filepath"

	"github.com/digitalx-official/digitalx-cli/internal/cli"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

func main() {
	// Resolve the program name from how the binary was invoked, so a renamed
	// binary shows its own name in help/examples/guidance. Set once here, before
	// the command tree runs; everything downstream reads progname.Name().
	if len(os.Args) > 0 && os.Args[0] != "" {
		progname.Set(filepath.Base(os.Args[0]))
	}
	os.Exit(cli.Execute(os.Args[1:], cli.Deps{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		SkillFS: skillSource(),
	}))
}
