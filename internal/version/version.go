// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package version holds the single CLI version string.
package version

// Version is the CLI version — reported by `--version`, embedded in the
// User-Agent header, and published in the commands catalog. It is injected at
// release build time via the linker:
//
//	-ldflags "-X github.com/korbit-official/korbit-cli/internal/version.Version=<tag>"
//
// (the release pipeline feeds it the git tag). A plain `go build` / `go run`
// from source leaves the "dev" default. It is therefore a var, not a const:
// `-X` can only overwrite a string variable.
var Version = "dev"
