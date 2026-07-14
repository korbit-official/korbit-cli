// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package progname

import "testing"

func TestNameDefault(t *testing.T) {
	if got := Name(); got != "korbit" {
		t.Fatalf("default Name() = %q, want %q", got, "korbit")
	}
}

func TestSet(t *testing.T) {
	orig := name
	t.Cleanup(func() { name = orig })

	Set("kb")
	if got := Name(); got != "kb" {
		t.Fatalf("after Set(\"kb\"), Name() = %q, want %q", got, "kb")
	}

	// An empty value is ignored, leaving the previous name.
	Set("")
	if got := Name(); got != "kb" {
		t.Fatalf("Set(\"\") must not change the name: Name() = %q, want %q", got, "kb")
	}
}
