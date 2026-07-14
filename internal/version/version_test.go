// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"strings"
	"testing"
)

// Version is injected into the User-Agent header (a single HTTP token) and the
// catalog, so whatever the build sets it to — the "dev" default or an injected
// git tag — it must be non-empty and free of whitespace/control characters that
// would corrupt the header. The exact format (semver, "dev", "v1.2.3-5-gabc")
// is the release pipeline's concern, not this package's.
func TestVersionIsHeaderSafe(t *testing.T) {
	if Version == "" {
		t.Fatal("version is empty")
	}
	if strings.ContainsFunc(Version, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		t.Fatalf("version %q contains whitespace or control characters", Version)
	}
}
