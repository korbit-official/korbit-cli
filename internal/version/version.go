// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package version holds the CLI's wire identity: its product token and the
// single version string.
package version

// Version is the CLI version — reported by `--version`, embedded in the
// User-Agent header, and published in the commands catalog. It is injected at
// release build time via the linker:
//
//	-ldflags "-X github.com/digitalx-official/digitalx-cli/internal/version.Version=<tag>"
//
// (the release pipeline feeds it the git tag). A plain `go build` / `go run`
// from source leaves the "dev" default. It is therefore a var, not a const:
// `-X` can only overwrite a string variable.
var Version = "dev"

// Product is the product token identifying this CLI on the wire — the first half
// of the User-Agent's leading "<product>/<version>" pair. It is the PRODUCT's
// name, deliberately independent of the invoked command name (internal/progname):
// server-side traffic attributes to one client whichever of the binary's names a
// user typed.
const Product = "digitalx-cli"

// Token is the "<product>/<version>" pair the User-Agent leads with — the single
// source of that string, so the wire layer's default, the WebSocket dial default,
// and the rich composed value in internal/useragent cannot drift apart.
func Token() string { return Product + "/" + Version }
