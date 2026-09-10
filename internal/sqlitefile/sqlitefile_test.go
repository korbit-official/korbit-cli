// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sqlitefile

import (
	"strings"
	"testing"
)

// TestDSNEscapesEveryPathShape pins the DSN for the shapes that would otherwise
// be misread. `?` is the one that matters most (the driver would truncate the
// path there and read the rest as parameters), and it is also why a caller cannot
// append query parameters to this DSN — they would land inside the filename.
func TestDSNEscapesEveryPathShape(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{`C:\Users\a\.digitalx-cli\journal.db`, `file:C:%5CUsers%5Ca%5C.digitalx-cli%5Cjournal.db`},
		{"/home/a/we?ird/journal.db", "file:%2Fhome%2Fa%2Fwe%3Fird%2Fjournal.db"},
		{"/home/a/100%/journal.db", "file:%2Fhome%2Fa%2F100%25%2Fjournal.db"},
		{"/home/a/a#b/journal.db", "file:%2Fhome%2Fa%2Fa%23b%2Fjournal.db"},
		{"/home/a/a&b/journal.db", "file:%2Fhome%2Fa%2Fa&b%2Fjournal.db"},
		{"/home/a/with space/journal.db", "file:%2Fhome%2Fa%2Fwith%20space%2Fjournal.db"},
	} {
		if got := DSN(tc.path); got != tc.want {
			t.Errorf("DSN(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	// The `?` is escaped, so nothing after a DSN can be read as a parameter list.
	if got := DSN("/a/b.db"); strings.Contains(got, "?") {
		t.Errorf("DSN(%q) = %q; a raw `?` would let a caller believe parameters can be appended", "/a/b.db", got)
	}
}
