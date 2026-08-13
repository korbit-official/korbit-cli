// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryGoFileHasSPDXHeader asserts every Go source file in the module
// carries the SPDX license identifier near its top, so a new file cannot ship
// without the Apache-2.0 header.
func TestEveryGoFileHasSPDXHeader(t *testing.T) {
	const want = "SPDX-License-Identifier: Apache-2.0"
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The header sits at the very top (above any build constraint or
		// package doc comment), so a small prefix is enough to check.
		head := b
		if len(head) > 256 {
			head = head[:256]
		}
		if !strings.Contains(string(head), want) {
			t.Errorf("%s: missing %q in its header", path, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
