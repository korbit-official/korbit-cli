// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package ids

import "testing"

func TestUUIDv7ShapeAndCharset(t *testing.T) {
	id := uuidv7At(1700000000000)
	if len(id) != 36 {
		t.Fatalf("want 36 chars, got %d (%q)", len(id), id)
	}
	if !ClientOrderIDPattern.MatchString(id) {
		t.Fatalf("uuidv7 must satisfy the clientOrderId charset: %q", id)
	}
	if id[14] != '7' {
		t.Fatalf("version nibble must be 7, got %q in %q", id[14], id)
	}
	if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("variant nibble must be 8-b, got %q in %q", v, id)
	}
}

func TestUUIDv7Unique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := UUIDv7()
		if seen[id] {
			t.Fatalf("collision: %q", id)
		}
		seen[id] = true
	}
}

func TestClientOrderIDPattern(t *testing.T) {
	ok := []string{"a", "ABC-123.x:y_z", "019eabcf-7f2e-7587-979c-d67bde2b8967"}
	bad := []string{"", "has space", "emoji😀", string(make([]byte, 37))}
	for _, s := range ok {
		if !ClientOrderIDPattern.MatchString(s) {
			t.Errorf("expected valid: %q", s)
		}
	}
	for _, s := range bad {
		if ClientOrderIDPattern.MatchString(s) {
			t.Errorf("expected invalid: %q", s)
		}
	}
}
