// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFillInstallTemplates runs scripts/fill-install-templates.sh against a
// synthetic checksums.txt and asserts the produced installers embed the release
// version and exactly the platform ARCHIVE checksums — not the .mcpb bundles or
// checksums.txt itself. The install-time download-and-verify path is covered by
// the Go update tests (same sha256 + extract logic); this pins the fill step,
// the one bespoke piece of the release pipeline.
func TestFillInstallTemplates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fill script is POSIX shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "fill-install-templates.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("fill script not found: %v", err)
	}

	out := t.TempDir()
	checksums := filepath.Join(out, "checksums.txt")
	// Archives (must be embedded) + a .mcpb and checksums.txt (must be excluded).
	sums := strings.Join([]string{
		"aaaa000000000000000000000000000000000000000000000000000000000001  digitalx-cli_darwin_arm64.tar.gz",
		"aaaa000000000000000000000000000000000000000000000000000000000002  digitalx-cli_linux_amd64.tar.gz",
		"aaaa000000000000000000000000000000000000000000000000000000000003  digitalx-cli_linux_arm64.tar.gz",
		"aaaa000000000000000000000000000000000000000000000000000000000004  digitalx-cli_windows_amd64.zip",
		"aaaa000000000000000000000000000000000000000000000000000000000005  digitalx-cli_windows_arm64.zip",
		"bbbb000000000000000000000000000000000000000000000000000000000006  digitalx_darwin_arm64.mcpb",
		"cccc000000000000000000000000000000000000000000000000000000000007  checksums.txt",
	}, "\n") + "\n"
	if err := os.WriteFile(checksums, []byte(sums), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bash, script, "v1.2.3", checksums, filepath.Join(root, "install"), out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fill script failed: %v\n%s", err, b)
	}

	sh := readFile(t, filepath.Join(out, "install.sh"))
	ps := readFile(t, filepath.Join(out, "install.ps1"))

	// Version pinned in both.
	if !strings.Contains(sh, `PIN_VERSION="v1.2.3"`) {
		t.Error("install.sh version not pinned")
	}
	if !strings.Contains(ps, `$PIN_VERSION = 'v1.2.3'`) {
		t.Error("install.ps1 version not pinned")
	}
	// Every archive hash embedded; non-archive assets excluded.
	for _, want := range []string{"digitalx-cli_darwin_arm64.tar.gz", "digitalx-cli_linux_amd64.tar.gz", "digitalx-cli_windows_arm64.zip"} {
		if !strings.Contains(sh, want) || !strings.Contains(ps, want) {
			t.Errorf("filled scripts missing archive %q", want)
		}
	}
	for _, unwanted := range []string{".mcpb", "checksums.txt  "} {
		if strings.Contains(sh, unwanted) {
			t.Errorf("install.sh must not embed %q", unwanted)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root (the
// dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
