// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testConfig builds a Config rooted at a temp home, with the binary dir NOT on
// PATH (so PATH wiring exercises the instructions branch) and a fixed clock.
func testConfig(home string) Config {
	return Config{
		Getenv: func(k string) string {
			switch k {
			case "HOME", "USERPROFILE":
				return home
			case "KORBIT_CLI_HOME":
				return filepath.Join(home, ".korbit-cli")
			case "LOCALAPPDATA":
				return filepath.Join(home, "AppData", "Local")
			case "PATH":
				return "/usr/bin:/bin" // binary dir deliberately absent
			}
			return ""
		},
		Now:      func() int64 { return 1700000000000 },
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Version:  "v1.0.0",
		Repo:     DefaultRepo,
		Progress: io.Discard,
	}
}

// writeFakeBinary writes a stand-in "binary" file and returns its path.
func writeFakeBinary(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "korbit")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestInstallFresh(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary content = %q", got)
	}
	m, found, err := loadManifest(l.ManifestPath())
	if err != nil || !found {
		t.Fatalf("manifest: found=%v err=%v", found, err)
	}
	if m.Method != MethodManagedScript || m.Version != "v1.0.0" {
		t.Errorf("manifest = %+v", m)
	}
	if m.SHA256 == "" || m.SHA256 != res.SHA256 {
		t.Errorf("manifest sha256 = %q, result sha256 = %q", m.SHA256, res.SHA256)
	}
	// The install dir isn't on PATH → the install reports instructions, not an edit.
	if res.Path.Action != pathActionInstructions || res.Path.OnPath {
		t.Errorf("path result = %+v (want instructions, not on path)", res.Path)
	}
	// A fresh install has nothing to repair.
	if len(res.Repaired) != 0 {
		t.Errorf("fresh install repaired = %v (want none)", res.Repaired)
	}
}

func TestInstallRepairsDeletedBinary(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	// The named case: the user deleted the installed binary from ~/.local/bin.
	if err := os.Remove(c.Layout().ExecutablePath()); err != nil {
		t.Fatal(err)
	}
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(c.Layout().ExecutablePath()) {
		t.Fatal("installed binary was not recreated")
	}
	if !containsSubstr(res.Repaired, "binary") {
		t.Errorf("repaired = %v (want a binary repair)", res.Repaired)
	}
}

func TestInstallRepairsCorruptManifest(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Layout().ManifestPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(res.Repaired, "manifest") {
		t.Errorf("repaired = %v (want a manifest repair)", res.Repaired)
	}
	if _, found, err := loadManifest(c.Layout().ManifestPath()); err != nil || !found {
		t.Fatalf("manifest not rebuilt: found=%v err=%v", found, err)
	}
}

func TestInstallRepairsMissingBinDir(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.Layout().ExecutableDir()); err != nil {
		t.Fatal(err)
	}
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !dirExists(c.Layout().ExecutableDir()) || !fileExists(c.Layout().ExecutablePath()) {
		t.Fatal("bin dir / binary not recreated")
	}
	if !containsSubstr(res.Repaired, "bin directory") {
		t.Errorf("repaired = %v (want a bin-dir repair)", res.Repaired)
	}
}

func TestInstallRefusesDevBuild(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.Version = "dev"
	c.exeOverride = writeFakeBinary(t, "x")
	if _, err := c.Install(); err == nil {
		t.Fatal("expected a dev build to be refused")
	}
}

func TestPathWiringPrompt(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil } // user says yes

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if res.Path.Action != pathActionEditedRC {
		t.Fatalf("path action = %q (want edited-shell-rc)", res.Path.Action)
	}
	// The managed block lands in every startup file, including the login profiles
	// (so desktop apps that resolve PATH via a login shell see it).
	for _, name := range []string{".bashrc", ".zshrc", ".profile", ".zprofile"} {
		p := filepath.Join(home, name)
		if !strings.Contains(mustContent(t, p), pathBlockBegin) {
			t.Errorf("%s missing the managed PATH block", name)
		}
	}
	// Idempotent: re-running does not add a second block.
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	body := mustContent(t, filepath.Join(home, ".zshrc"))
	if n := strings.Count(body, pathBlockBegin); n != 1 {
		t.Errorf("managed block appears %d times after re-run (want 1)", n)
	}
}

// TestAdditionForMatchesWrite pins that the diff preview is byte-identical to
// what appendBlockTo actually writes: joining the context tail + added lines back
// together must reproduce the file after the append. It also checks the line
// numbering, the context window, and the NewFile flag across the tricky cases (a
// file with/without a trailing newline, and a missing file).
func TestAdditionForMatchesWrite(t *testing.T) {
	const exportLine = `export PATH="$HOME/.local/bin:$PATH"`
	body := pathBlockBody("$HOME/.local/bin")
	cases := []struct {
		name        string
		existing    *string // nil = file does not exist
		wantNewFile bool
		wantCtxNums []int // line numbers of the context lines shown
		wantAddHead int   // line number of the first added line
	}{
		{"trailing newline", ptr("a\nb\nc\nd\n"), false, []int{2, 3, 4}, 5},
		{"no trailing newline", ptr("a\nb"), false, []int{1, 2}, 3},
		{"missing file", nil, true, nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".zshrc")
			if tc.existing != nil {
				if err := os.WriteFile(path, []byte(*tc.existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			add := additionFor(path, body)
			if add == nil {
				t.Fatal("additionFor returned nil for a file without the block")
			}
			if add.NewFile != tc.wantNewFile {
				t.Errorf("NewFile = %v (want %v)", add.NewFile, tc.wantNewFile)
			}
			var ctxNums []int
			for _, dl := range add.Context {
				ctxNums = append(ctxNums, dl.Num)
			}
			if !reflect.DeepEqual(ctxNums, tc.wantCtxNums) {
				t.Errorf("context line numbers = %v (want %v)", ctxNums, tc.wantCtxNums)
			}
			if add.Added[0].Num != tc.wantAddHead {
				t.Errorf("first added line number = %d (want %d)", add.Added[0].Num, tc.wantAddHead)
			}
			// The added block must contain the managed markers and the export line.
			var addedText []string
			for _, dl := range add.Added {
				addedText = append(addedText, dl.Text)
			}
			joined := strings.Join(addedText, "\n")
			for _, want := range []string{pathBlockBegin, exportLine, pathBlockEnd} {
				if !strings.Contains(joined, want) {
					t.Errorf("added lines missing %q; got:\n%s", want, joined)
				}
			}

			// Byte-accuracy: applying the real write must produce a file whose numbered
			// lines equal context-precursor + (context) + added, i.e. the preview's
			// added lines are exactly the new tail of the file.
			if _, err := appendBlockTo(path, body); err != nil {
				t.Fatal(err)
			}
			final := numberedLines(mustContent(t, path))
			for _, dl := range add.Added {
				if dl.Num-1 >= len(final) || final[dl.Num-1].Text != dl.Text {
					t.Errorf("added line %d %q not found at that position in the written file", dl.Num, dl.Text)
				}
			}
			// A second additionFor now returns nil (block already present → idempotent).
			if additionFor(path, body) != nil {
				t.Error("additionFor should return nil once the block is present")
			}
		})
	}
}

// TestManagedBlockGuardsAgainstDuplicatePATH proves the guarded prepend is a
// no-op when the dir is already on PATH: sourcing the block twice (as a bash login
// shell effectively does when both a login profile and .bashrc carry it) must
// leave the dir on PATH exactly once, not twice.
func TestManagedBlockGuardsAgainstDuplicatePATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh block is unix-only")
	}
	const dir = "/opt/dgx/bin"
	block := managedBlock(pathBlockBody(dir))
	// Fresh PATH, source the block twice, then count how many components equal dir.
	script := "PATH=/usr/bin:/bin\n" + block + block +
		`n=0; IFS=:; for p in $PATH; do [ "$p" = "` + dir + `" ] && n=$((n+1)); done; printf '%s' "$n"`
	out, err := exec.Command("/bin/sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("running the block under /bin/sh: %v", err)
	}
	if got := string(out); got != "1" {
		t.Errorf("%s appears %s time(s) in PATH after double-sourcing (want 1)", dir, got)
	}
}

func ptr(s string) *string { return &s }

// TestPathWiringPerFileSelection pins that the managed block is written ONLY to
// the rc files the confirm accepted — declining a file leaves it untouched — and
// that declining every file falls back to guidance rather than a false success.
func TestPathWiringPerFileSelection(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	// Accept only .zshrc; decline the rest.
	c.PathConfirm = func(add PathAddition) (bool, error) {
		return filepath.Base(add.Location) == ".zshrc", nil
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if res.Path.Action != pathActionEditedRC {
		t.Fatalf("path action = %q (want edited-shell-rc)", res.Path.Action)
	}
	if len(res.Path.Files) != 1 || filepath.Base(res.Path.Files[0]) != ".zshrc" {
		t.Fatalf("edited files = %v (want only .zshrc)", res.Path.Files)
	}
	if !strings.Contains(mustContent(t, filepath.Join(home, ".zshrc")), pathBlockBegin) {
		t.Error(".zshrc should carry the managed block")
	}
	for _, name := range []string{".bashrc", ".profile", ".zprofile"} {
		if fileExists(filepath.Join(home, name)) {
			t.Errorf("%s was created/edited despite being declined", name)
		}
	}

	// Declining every file yields guidance (Action=instructions), not a false edit.
	home2 := t.TempDir()
	c2 := testConfig(home2)
	c2.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c2.PathConfirm = func(PathAddition) (bool, error) { return false, nil }
	res2, err := c2.Install()
	if err != nil {
		t.Fatal(err)
	}
	if res2.Path.Action != pathActionInstructions {
		t.Errorf("path action after declining all = %q (want instructions)", res2.Path.Action)
	}
	for _, name := range []string{".bashrc", ".zshrc", ".profile", ".zprofile"} {
		if fileExists(filepath.Join(home2, name)) {
			t.Errorf("%s was written despite declining all", name)
		}
	}
}

// TestPathWiringPrewiredFileIsNotReportedMissing is the regression for: after a
// file already carries the block (an earlier accepted run), a fresh install must
// not tell the user the dir is "not on your PATH". The already-wired file is
// skipped from the per-file prompts, so nothing is written THIS run — but the dir
// is wired, so the outcome is "restart your shell", not the add-this-line guidance.
func TestPathWiringPrewiredFileIsNotReportedMissing(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	var progress bytes.Buffer
	c.Progress = &progress

	// Pre-wire .zshrc as an earlier accepted run would have; leave the others bare.
	dir := c.Layout().ExecutableDir()
	if _, err := appendBlockTo(filepath.Join(home, ".zshrc"), pathBlockBody(c.Layout().homeForm(dir))); err != nil {
		t.Fatal(err)
	}

	var prompted []string
	c.PathConfirm = func(add PathAddition) (bool, error) {
		prompted = append(prompted, filepath.Base(add.Location))
		return false, nil // decline every file still offered
	}

	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	// The already-wired .zshrc must not be offered again.
	for _, p := range prompted {
		if p == ".zshrc" {
			t.Errorf(".zshrc already carries the block but was offered again")
		}
	}
	// The dir IS wired (via .zshrc), so the outcome is edited-shell-rc (restart
	// needed), NOT the "not on your PATH — add this line" guidance.
	if res.Path.Action != pathActionEditedRC {
		t.Errorf("path action = %q (want edited-shell-rc; a pre-wired file must not read as missing)", res.Path.Action)
	}
	if strings.Contains(progress.String(), "is not on your PATH") {
		t.Errorf("printed the not-on-PATH guidance despite .zshrc being wired:\n%s", progress.String())
	}
}

func TestUpdate(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()

	// Simulate running from the managed binary for the provenance check.
	c.exeOverride = l.ExecutablePath()
	// Serve a fake v2.0.0 release: the latest redirect, the archive, and checksums.
	newBin := "BINARY-v2"
	archive := makeArchive(t, c.os(), l.BinName(), newBin)
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.LatestVersion != "v2.0.0" {
		t.Fatalf("update result = %+v", res)
	}
	if res.SignatureCheck != SigVerified {
		t.Errorf("SignatureCheck = %q (want %q)", res.SignatureCheck, SigVerified)
	}
	if got := mustContent(t, l.ExecutablePath()); got != newBin {
		t.Errorf("binary after update = %q (want %q)", got, newBin)
	}
	m, _, _ := loadManifest(l.ManifestPath())
	if m.Version != "v2.0.0" {
		t.Errorf("manifest version = %q (want v2.0.0)", m.Version)
	}
	if m.SHA256 != res.SHA256 || res.SHA256 == "" {
		t.Errorf("manifest sha256 = %q, result sha256 = %q", m.SHA256, res.SHA256)
	}
}

func TestUpdateRejectsTamperedArchive(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	// A valid signature over the (tampered) checksums.txt, so verification passes
	// and the mismatch surfaces at the archive-hash check, not the signature.
	d := &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t)}
	d.tamperChecksum = true // advertise a wrong hash
	c.Doer = d
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum-mismatch error, got %v", err)
	}
	// The binary is untouched on a rejected update.
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// managedForUpdate returns a Config installed as a managed build and posed as if
// running from the installed binary, plus its layout — the common setup every
// signature-verification test shares before wiring a fakeDoer.
func managedForUpdate(t *testing.T) (Config, Layout) {
	t.Helper()
	c := testConfig(t.TempDir())
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()
	return c, l
}

// TestUpdateRejectsBadSignature: a published cert but a signature over other
// bytes (tampering) is fatal, and the binary is left untouched.
func TestUpdateRejectsBadSignature(t *testing.T) {
	c, l := managedForUpdate(t)
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t), signWrong: true}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "did not verify") {
		t.Fatalf("expected a signature-verification failure, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateRejectsMissingSignature: a cert is published, so a release with no
// signature is a downgrade signal and fatal — the check can't be bypassed by
// stripping the signature.
func TestUpdateRejectsMissingSignature(t *testing.T) {
	c, l := managedForUpdate(t)
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t), omitSig: true}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("expected a missing-signature (downgrade) failure, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateSkipsWhenCertEmpty: an empty cert body is the deliberate kill switch
// — verification is disabled and the update proceeds on TLS + SHA-256 even with
// no signature served.
func TestUpdateSkipsWhenCertEmpty(t *testing.T) {
	c, l := managedForUpdate(t)
	newBin := "BINARY-v2"
	archive := makeArchive(t, c.os(), l.BinName(), newBin)
	// No kit: the signature endpoint 404s, and it must not matter.
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, certBody: []byte("\n  \n")}
	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatalf("update with verification disabled failed: %v", err)
	}
	if !res.Updated {
		t.Fatalf("update result = %+v", res)
	}
	if res.SignatureCheck != SigDisabled {
		t.Errorf("SignatureCheck = %q (want %q)", res.SignatureCheck, SigDisabled)
	}
	if got := mustContent(t, l.ExecutablePath()); got != newBin {
		t.Errorf("binary after update = %q (want %q)", got, newBin)
	}
}

// TestUpdateFailsWhenCertUnreachable: a transport-level failure reaching the
// cert host (DNS/TLS/connection) is fail-closed, not a silent skip.
func TestUpdateFailsWhenCertUnreachable(t *testing.T) {
	c, l := managedForUpdate(t)
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t), certErr: true}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "release-signing certificate") {
		t.Fatalf("expected a fail-closed cert-fetch error, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateFailsWhenCertEndpointErrors: a non-200 from the cert host (here 503)
// is fail-closed — absence is never a silent skip, only the empty body is.
func TestUpdateFailsWhenCertEndpointErrors(t *testing.T) {
	c, l := managedForUpdate(t)
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t), certStatus: 503}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "unexpected status 503") {
		t.Fatalf("expected a fail-closed cert-fetch error, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateFailsOnSoftFour04Cert: a 200 whose body is a non-cert page (a
// soft-404, or any junk) is fatal — it is neither a valid pin nor the empty-body
// kill switch, so it must not silently disable verification.
func TestUpdateFailsOnSoftFour04Cert(t *testing.T) {
	c, l := managedForUpdate(t)
	archive := makeArchive(t, c.os(), l.BinName(), "BINARY-v2")
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: newSignerKit(t), certBody: []byte("<!doctype html><title>404 Not Found</title>")}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
		t.Fatalf("expected a non-cert-body failure, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateRotatesCert: a bundle of several certs verifies if ANY of them
// matches the signing key (lossless key rotation).
func TestUpdateRotatesCert(t *testing.T) {
	c, l := managedForUpdate(t)
	newBin := "BINARY-v2"
	archive := makeArchive(t, c.os(), l.BinName(), newBin)
	signing := newSignerKit(t)
	stale := newSignerKit(t)
	// Publish two certs: a stale one first, then the one that actually signs.
	bundle := append(append([]byte{}, stale.certPEM...), signing.certPEM...)
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: archive, kit: signing, certBody: bundle}
	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatalf("update against a rotated cert bundle failed: %v", err)
	}
	if !res.Updated || mustContent(t, l.ExecutablePath()) != newBin {
		t.Fatalf("update result = %+v", res)
	}
}

// ---- the release manifest ----

// manifestFor builds a release-manifest.json body naming archive and binary for
// c's platform — the shape a release publishes.
func manifestFor(t *testing.T, c Config, archive, binary string) []byte {
	t.Helper()
	// A literal, not a marshalled releaseManifest: the parser deliberately omits
	// `schema`, so round-tripping the Go type would serve a body no release has.
	m := map[string]any{
		"schema":    1,
		"platforms": map[string]any{c.os() + "/" + c.arch(): map[string]string{"archive": archive, "binary": binary}},
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestUpdateFollowsRenamedRelease is the whole point of the manifest: a release
// whose archive AND inner binary are both named something this build knows
// nothing about still updates it. Nothing here serves the compiled-in names, so
// a fallback would fail.
func TestUpdateFollowsRenamedRelease(t *testing.T) {
	c, l := managedForUpdate(t)
	renamedArchive := "renamed-cli_" + c.os() + "_" + c.arch() + ".tar.gz"
	if c.os() == "windows" {
		renamedArchive = "renamed-cli_" + c.os() + "_" + c.arch() + ".zip"
	}
	renamedBin := "rn-cli"
	if c.os() == "windows" {
		renamedBin += ".exe"
	}
	newBin := "BINARY-v2"
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0",
		asset:    renamedArchive,
		archive:  makeArchive(t, c.os(), renamedBin, newBin),
		kit:      newSignerKit(t),
		manifest: manifestFor(t, c, renamedArchive, renamedBin),
	}

	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatalf("update against a renamed release failed: %v", err)
	}
	if !res.Updated || res.LatestVersion != "v2.0.0" {
		t.Fatalf("update result = %+v", res)
	}
	// The bytes land at this install's own path under its own name: the manifest
	// says what to download, never where to put it.
	if got := mustContent(t, l.ExecutablePath()); got != newBin {
		t.Errorf("binary after update = %q (want %q)", got, newBin)
	}
}

// TestUpdateRejectsUnlistedManifest: a manifest the signed checksums.txt does
// not cover may not steer where code comes from.
func TestUpdateRejectsUnlistedManifest(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:          makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:              newSignerKit(t),
		manifest:         manifestFor(t, c, c.assetName(), l.BinName()),
		manifestUnlisted: true,
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "not listed in checksums.txt") {
		t.Fatalf("expected an unauthenticated-manifest error, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateRejectsTamperedManifest: the manifest is listed, but the body served
// is not the body the signed checksums cover.
func TestUpdateRejectsTamperedManifest(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:        makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:            newSignerKit(t),
		manifest:       manifestFor(t, c, c.assetName(), l.BinName()),
		manifestBadSum: true,
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch for "+releaseManifestName) {
		t.Fatalf("expected a manifest checksum mismatch, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateRejectsManifestWithoutThisPlatform: an authentic manifest is
// authoritative, so a platform it does not list is a release with no build for
// it — not a reason to guess the compiled-in name.
func TestUpdateRejectsManifestWithoutThisPlatform(t *testing.T) {
	c, l := managedForUpdate(t)
	other := []byte(`{"schema":1,"platforms":{"plan9/mips":{"archive":"x.tar.gz","binary":"x"}}}`)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:  makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:      newSignerKit(t),
		manifest: other,
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "has no build for "+c.os()+"/"+c.arch()) {
		t.Fatalf("expected a missing-platform error, got %v", err)
	}
}

// TestUpdateRejectsManifestPathEscape: an entry naming anything but a plain
// filename is refused, so a wrong-but-authentic manifest still cannot reach
// outside the release the signature covers.
func TestUpdateRejectsManifestPathEscape(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:  makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:      newSignerKit(t),
		manifest: manifestFor(t, c, "../../../other/repo/releases/download/v1/x.tar.gz", l.BinName()),
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("expected an unusable-entry error, got %v", err)
	}
}

// TestUpdateFallsBackWhenManifestUnobtainable covers the fallback outcome across
// all three ways it is reached — a 404 (the release publishes none), any other
// non-200, and a transport failure. Each must land on the compiled-in names and
// update normally, so withholding the manifest cannot block an update.
func TestUpdateFallsBackWhenManifestUnobtainable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		fail   bool
	}{
		{"404", 404, false},
		{"503", 503, false},
		{"transport failure", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, l := managedForUpdate(t)
			newBin := "BINARY-v2"
			d := &fakeDoer{
				repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
				archive: makeArchive(t, c.os(), l.BinName(), newBin),
				kit:     newSignerKit(t),
			}
			d.manifestStatus, d.manifestErr = tc.status, tc.fail
			c.Doer = d
			res, err := c.Update(context.Background(), UpdateOptions{})
			if err != nil {
				t.Fatalf("update should fall back to the compiled-in names, got %v", err)
			}
			if !res.Updated || mustContent(t, l.ExecutablePath()) != newBin {
				t.Fatalf("update result = %+v", res)
			}
		})
	}
}

// TestUpdateRejectsUnparseableManifest: an authentic manifest that does not
// parse is fatal — the release stated something this binary cannot read, and
// guessing is what the manifest exists to avoid.
func TestUpdateRejectsUnparseableManifest(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:  makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:      newSignerKit(t),
		manifest: []byte("{not json"),
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "reading "+releaseManifestName) {
		t.Fatalf("expected an unparseable-manifest error, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

// TestUpdateIgnoresUnknownManifestFields pins the additive-schema rule: a later
// release may add fields, including a `schema` of an unanticipated shape, and an
// older binary must still read the platforms it understands rather than failing
// the parse and stranding itself.
func TestUpdateIgnoresUnknownManifestFields(t *testing.T) {
	c, l := managedForUpdate(t)
	newBin := "BINARY-v2"
	body := []byte(`{"schema":{"major":9},"addedLater":["x"],"platforms":{"` +
		c.os() + "/" + c.arch() + `":{"archive":"` + c.assetName() + `","binary":"` + l.BinName() + `","note":"ignored"}}}`)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:  makeArchive(t, c.os(), l.BinName(), newBin),
		kit:      newSignerKit(t),
		manifest: body,
	}
	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatalf("a manifest with unknown fields must still resolve, got %v", err)
	}
	if !res.Updated || mustContent(t, l.ExecutablePath()) != newBin {
		t.Fatalf("update result = %+v", res)
	}
}

// TestUpdateNeverReadsManifestBeforeSignature pins the ORDERING that makes the
// manifest safe to act on: every other manifest test serves a valid signature,
// so moving resolveTarget above verifyChecksumsSignature would leave them green
// while making the manifest an unauthenticated input.
func TestUpdateNeverReadsManifestBeforeSignature(t *testing.T) {
	c, l := managedForUpdate(t)
	d := &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:  makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:      newSignerKit(t),
		manifest: manifestFor(t, c, c.assetName(), l.BinName()),
		omitSig:  true, // a cert is published but the release is unsigned: fatal
	}
	c.Doer = d
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil {
		t.Fatal("expected the update to fail on the missing signature")
	}
	if d.manifestRequested {
		t.Error("the release manifest was fetched before the signature was verified")
	}
}

// TestUpdateRejectsEmptyArchiveEntry: the manifest names the entry to extract,
// so a release-side mistake can select a directory or an empty file — and zero
// bytes written over the installed binary leaves an install that can neither run
// nor update itself.
func TestUpdateRejectsEmptyArchiveEntry(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive: makeArchive(t, c.os(), l.BinName(), ""),
		kit:     newSignerKit(t),
	}
	if _, err := c.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected an empty-entry error, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("binary changed on a rejected update: %q", got)
	}
}

func TestIsPlainFilename(t *testing.T) {
	for _, ok := range []string{"digitalx-cli_linux_amd64.tar.gz", "dgx-cli", "dgx-cli.exe", "a"} {
		if !isPlainFilename(ok) {
			t.Errorf("isPlainFilename(%q) = false (want true)", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../x", "/abs"} {
		if isPlainFilename(bad) {
			t.Errorf("isPlainFilename(%q) = true (want false)", bad)
		}
	}
}

// releaseMatrix is the platform set .goreleaser.yaml builds — Intel macOS
// (darwin/amd64) is deliberately absent. Keep the two in sync.
var releaseMatrix = []string{"darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}

// TestReleaseManifestMatchesBuild guards the checked-in release-manifest.json
// against drifting from what the release produces: an entry naming an archive
// the build does not carry breaks self update on that platform for every install
// that reads it, and would only surface after publish.
func TestReleaseManifestMatchesBuild(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", releaseManifestName))
	if err != nil {
		t.Fatal(err)
	}
	// A LOCAL type carrying schema, because the production parser deliberately
	// omits it (see releaseManifest); the published file still declares it for a
	// human reader.
	var m struct {
		Schema    int                      `json:"schema"`
		Platforms map[string]releaseTarget `json:"platforms"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing %s: %v", releaseManifestName, err)
	}
	if m.Schema != 1 {
		t.Errorf("schema = %d (want 1)", m.Schema)
	}
	if len(m.Platforms) != len(releaseMatrix) {
		t.Errorf("manifest covers %d platforms (want %d: %v)", len(m.Platforms), len(releaseMatrix), releaseMatrix)
	}
	for _, platform := range releaseMatrix {
		entry, ok := m.Platforms[platform]
		if !ok {
			t.Errorf("manifest has no entry for %s", platform)
			continue
		}
		goos, goarch, _ := strings.Cut(platform, "/")
		c := Config{GOOS: goos, GOARCH: goarch}
		if want := c.assetName(); entry.Archive != want {
			t.Errorf("%s archive = %q (want %q)", platform, entry.Archive, want)
		}
		if want := c.Layout().BinName(); entry.Binary != want {
			t.Errorf("%s binary = %q (want %q)", platform, entry.Binary, want)
		}
	}
}

// TestParseReleaseKeys covers the bundle parser: multiple certs, a skipped
// non-cert block, and the empty/garbage cases.
func TestParseReleaseKeys(t *testing.T) {
	k1, k2 := newSignerKit(t), newSignerKit(t)
	if got := parseReleaseKeys(append(append([]byte{}, k1.certPEM...), k2.certPEM...)); len(got) != 2 {
		t.Errorf("two-cert bundle parsed %d keys (want 2)", len(got))
	}
	junk := append([]byte("-----BEGIN NONSENSE-----\nAAAA\n-----END NONSENSE-----\n"), k1.certPEM...)
	if got := parseReleaseKeys(junk); len(got) != 1 {
		t.Errorf("bundle with a non-cert block parsed %d keys (want 1)", len(got))
	}
	if got := parseReleaseKeys([]byte("not a pem at all")); got != nil {
		t.Errorf("non-PEM input parsed %d keys (want 0)", len(got))
	}
}

func TestUpdateDryRun(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()
	// A dry run runs the full verification chain short of the download, so the
	// release has to be signed for it to succeed.
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v2.0.0", asset: c.assetName(), archive: makeArchive(t, c.os(), l.BinName(), "BINARY-v2"), kit: newSignerKit(t)}

	res, err := c.Update(context.Background(), UpdateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated || !res.CheckedOnly || res.LatestVersion != "v2.0.0" {
		t.Fatalf("dry-run result = %+v", res)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("dry-run changed the binary: %q", got)
	}
	// The dry run reports what it resolved, so --json says which asset a real run
	// would fetch and whether the release was signature-verified.
	if res.Archive != c.assetName() {
		t.Errorf("dry-run archive = %q (want %q)", res.Archive, c.assetName())
	}
	if res.SignatureCheck != SigVerified {
		t.Errorf("dry-run SignatureCheck = %q (want %q)", res.SignatureCheck, SigVerified)
	}
}

// TestUpdateDryRunResolvesRenamedRelease: a dry run reports the archive the
// RELEASE names, not the one this build was compiled with, so it answers what
// would actually be fetched — the question during a rename.
func TestUpdateDryRunResolvesRenamedRelease(t *testing.T) {
	c, l := managedForUpdate(t)
	renamed := "renamed-cli_" + c.os() + "_" + c.arch() + ".tar.gz"
	if c.os() == "windows" {
		renamed = "renamed-cli_" + c.os() + "_" + c.arch() + ".zip"
	}
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: renamed,
		archive:  makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:      newSignerKit(t),
		manifest: manifestFor(t, c, renamed, l.BinName()),
	}
	res, err := c.Update(context.Background(), UpdateOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Archive != renamed {
		t.Errorf("dry-run archive = %q (want %q)", res.Archive, renamed)
	}
}

// TestUpdateDryRunCatchesUnusableRelease is why the dry run verifies at all: a
// release whose manifest the signed checksums do not cover is uninstallable, and
// reporting "an update is available" for it would send the operator away
// believing the release is fine.
func TestUpdateDryRunCatchesUnusableRelease(t *testing.T) {
	c, l := managedForUpdate(t)
	c.Doer = &fakeDoer{
		repo: c.repo(), tag: "v2.0.0", asset: c.assetName(),
		archive:          makeArchive(t, c.os(), l.BinName(), "BINARY-v2"),
		kit:              newSignerKit(t),
		manifest:         manifestFor(t, c, c.assetName(), l.BinName()),
		manifestUnlisted: true,
	}
	if _, err := c.Update(context.Background(), UpdateOptions{DryRun: true}); err == nil || !strings.Contains(err.Error(), "not listed in checksums.txt") {
		t.Fatalf("a dry run must surface an unusable release, got %v", err)
	}
	if got := mustContent(t, l.ExecutablePath()); got != "BINARY-v1" {
		t.Errorf("dry-run changed the binary: %q", got)
	}
}

func TestUpdateAlreadyLatest(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	c.exeOverride = c.Layout().ExecutablePath()
	c.Doer = &fakeDoer{repo: c.repo(), tag: "v1.0.0", asset: c.assetName()}
	res, err := c.Update(context.Background(), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated {
		t.Errorf("expected no update when already latest: %+v", res)
	}
}

func TestUpdateRefusesUnmanaged(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	// No install → no manifest.
	c.exeOverride = writeFakeBinary(t, "x")
	_, err := c.Update(context.Background(), UpdateOptions{})
	var pe *ProvenanceError
	if err == nil || !asProvenance(err, &pe) {
		t.Fatalf("expected a ProvenanceError, got %v", err)
	}
	if pe.Guidance == "" {
		t.Error("provenance error should carry guidance")
	}
}

func TestUninstall(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	var locs []string
	for _, e := range c.PathEdits() {
		locs = append(locs, e.Location)
	}
	if len(locs) == 0 {
		t.Fatal("expected pending PATH edits after install")
	}
	res, err := c.Uninstall(UninstallOptions{RemoveBinary: true, EditPaths: locs})
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(l.ExecutablePath()) || fileExists(l.ManifestPath()) {
		t.Error("uninstall left managed files behind")
	}
	if len(res.Edited) == 0 {
		t.Error("expected the PATH edit to be recorded in Edited")
	}
	if strings.Contains(mustContent(t, filepath.Join(home, ".zshrc")), pathBlockBegin) {
		t.Error(".zshrc still has the managed PATH block after uninstall")
	}
}

// TestManagedThroughSymlinkedHome pins that a symlinked home (e.g. HOME points
// at a link onto another volume, so ~/.local/bin/dgx-cli resolves to a different
// real path than its literal form) does not make the installed binary look
// unmanaged. os.Executable resolves the running binary, so the provenance check
// must resolve the layout path too — otherwise update/uninstall wrongly refuse
// with a ProvenanceError. Regression for the symlinked-home case.
func TestManagedThroughSymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	linkHome := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	// HOME is the symlink; the layout paths are all expressed through it.
	c := testConfig(linkHome)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()

	// The running binary is the fully symlink-resolved path, exactly what
	// resolveExecutable produces in production.
	resolved, err := filepath.EvalSymlinks(l.ExecutablePath())
	if err != nil {
		t.Fatalf("resolve installed binary: %v", err)
	}
	if resolved == filepath.Clean(l.ExecutablePath()) {
		t.Fatalf("test setup did not exercise a symlinked home: %s", resolved)
	}
	c.exeOverride = resolved

	// The provenance guard must recognize this as the managed binary.
	if err := c.AssertManaged(); err != nil {
		t.Fatalf("AssertManaged rejected the managed binary reached through a symlinked home: %v", err)
	}

	// And a full uninstall must proceed and remove the binary.
	res, err := c.Uninstall(UninstallOptions{RemoveBinary: true})
	if err != nil {
		t.Fatalf("uninstall through symlinked home: %v", err)
	}
	if fileExists(l.ExecutablePath()) {
		t.Error("uninstall left the installed binary behind")
	}
	if len(res.Removed) == 0 {
		t.Error("uninstall reported nothing removed")
	}
}

// TestResolveOrClean covers the path-comparison helper directly, including the
// two symlink shapes the Go EvalSymlinks doc distinguishes: a relative symlink
// *target* in the tree (resolved fine once the input is absolute) and a relative
// *input path* (which must be absolutized so it never spuriously differs from an
// absolute operand pointing at the same file).
func TestResolveOrClean(t *testing.T) {
	// Nonexistent path: still returns an absolute, cleaned form (no resolution).
	if got := resolveOrClean("/no/such/dgx/./bin"); got != "/no/such/dgx/bin" {
		t.Errorf("nonexistent path: got %q", got)
	}

	real := t.TempDir()
	bin := filepath.Join(real, "korbit")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A *relative* symlink target inside the tree: link -> korbit (no directory
	// part). EvalSymlinks must resolve it to the real absolute file.
	rel := filepath.Join(real, "korbit-link")
	if err := os.Symlink("korbit", rel); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if got := resolveOrClean(rel); got != resolveOrClean(bin) {
		t.Errorf("relative symlink target: %q != %q", got, resolveOrClean(bin))
	}

	// A *relative* input path (cwd-relative) must resolve to the same absolute
	// real file as its absolute form — the case the doc warns keeps a result
	// relative. Drive it from the real dir as cwd.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(real); err != nil {
		t.Fatal(err)
	}
	if got := resolveOrClean("korbit"); got != resolveOrClean(bin) {
		t.Errorf("relative input path: %q != %q", got, resolveOrClean(bin))
	}
	if !filepath.IsAbs(resolveOrClean("korbit")) {
		t.Error("relative input did not absolutize")
	}
}

// TestUninstallKeepsPathWhenNotEdited pins that with no EditPaths the managed
// block stays in the rc files and PathEdits still reports it, so the cli layer
// can list it as kept for the user to remove by hand.
func TestUninstallKeepsPathWhenNotEdited(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	res, err := c.Uninstall(UninstallOptions{RemoveBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Edited) != 0 {
		t.Errorf("nothing should be edited, got %v", res.Edited)
	}
	if !strings.Contains(mustContent(t, filepath.Join(home, ".zshrc")), pathBlockBegin) {
		t.Error(".zshrc lost the managed PATH block even though no edit was requested")
	}
	if len(c.PathEdits()) == 0 {
		t.Error("PathEdits should still report the remaining block")
	}
}

// TestUninstallEditPathPreservesOtherLines pins that undoing the PATH edit removes
// ONLY the marker block, leaving the rest of the rc file intact.
func TestUninstallEditPathPreservesOtherLines(t *testing.T) {
	home := t.TempDir()
	zshrc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(zshrc, []byte("echo hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	if _, err := c.Uninstall(UninstallOptions{EditPaths: []string{zshrc}}); err != nil {
		t.Fatal(err)
	}
	got := mustContent(t, zshrc)
	if strings.Contains(got, pathBlockBegin) {
		t.Errorf(".zshrc still has the block:\n%s", got)
	}
	if !strings.Contains(got, "echo hello") {
		t.Errorf(".zshrc lost the user's own line:\n%s", got)
	}
}

// TestUninstallRemovesData pins that RemoveData unlinks the CLI-home data files
// in DataPaths (and silently ignores non-existent ones).
func TestUninstallRemovesData(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	cfgFile := filepath.Join(l.Home(), "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(l.Home(), "keys.json") // never created

	res, err := c.Uninstall(UninstallOptions{RemoveData: true, DataPaths: []string{cfgFile, absent}})
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(cfgFile) {
		t.Error("RemoveData left a config file behind")
	}
	var sawCfg bool
	for _, r := range res.Removed {
		if r == cfgFile {
			sawCfg = true
		}
	}
	if !sawCfg {
		t.Errorf("expected config file in Removed, got %v", res.Removed)
	}
}

// TestUninstallKeepsDataWhenNotSelected pins that data files survive unless
// RemoveData is set — the re-install-keeps-your-setup guarantee.
func TestUninstallKeepsDataWhenNotSelected(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	cfgFile := filepath.Join(l.Home(), "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Uninstall(UninstallOptions{RemoveBinary: true, DataPaths: []string{cfgFile}}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(cfgFile) {
		t.Error("config file removed even though RemoveData was not set")
	}
}

// mkArtifact creates a directory with a file inside and returns its path — a
// stand-in for a sandbox state dir / artifact cache uninstall may remove.
func mkArtifact(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestUninstallRemovesCaches(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	stateDir := mkArtifact(t, filepath.Join(l.Home(), "sandbox"))
	cacheDir := mkArtifact(t, filepath.Join(t.TempDir(), "digitalx-cli"))
	// A non-existent artifact must be silently ignored (neither removed nor reported).
	absent := filepath.Join(t.TempDir(), "does-not-exist")

	res, err := c.Uninstall(UninstallOptions{RemoveCaches: true, Artifacts: []string{stateDir, cacheDir, absent}})
	if err != nil {
		t.Fatal(err)
	}
	if dirExists(stateDir) || dirExists(cacheDir) {
		t.Error("cache removal left a sandbox artifact dir behind")
	}
	var n int
	for _, r := range res.Removed {
		if r == stateDir || r == cacheDir {
			n++
		}
	}
	if n != 2 {
		t.Errorf("expected 2 removed cache dirs in %v", res.Removed)
	}
}

func TestUninstallKeepsCachesWhenNotSelected(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	stateDir := mkArtifact(t, filepath.Join(l.Home(), "sandbox"))
	cacheDir := mkArtifact(t, filepath.Join(t.TempDir(), "digitalx-cli"))

	if _, err := c.Uninstall(UninstallOptions{RemoveBinary: true, Artifacts: []string{stateDir, cacheDir}}); err != nil {
		t.Fatal(err)
	}
	if !dirExists(stateDir) || !dirExists(cacheDir) {
		t.Error("caches removed even though RemoveCaches was not set")
	}
}

// TestUninstallCachesRefusesHomeContainingArtifact pins the safety guard: an
// artifact dir that IS (or contains) the CLI home — e.g. a KORBIT_CLI_SANDBOX_CACHE
// misconfigured to point at home — must never be removed, since that would wipe
// config/keys/journal. It is skipped and reported as a warning instead.
func TestUninstallCachesRefusesHomeContainingArtifact(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	// A config file directly under home stands in for the account setup that must
	// survive.
	cfgFile := filepath.Join(l.Home(), "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Artifact pointed AT the home dir itself, plus its parent — both contain home.
	res, err := c.Uninstall(UninstallOptions{RemoveCaches: true, Artifacts: []string{l.Home(), filepath.Dir(l.Home())}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Removed {
		if r == l.Home() || r == filepath.Dir(l.Home()) {
			t.Errorf("must not remove a home-containing artifact, removed %v", res.Removed)
		}
	}
	if !dirExists(l.Home()) || !fileExists(cfgFile) {
		t.Fatal("cache removal deleted the CLI home / config despite the safety guard")
	}
	if len(res.Warnings) != 2 {
		t.Errorf("expected a warning per skipped home-containing artifact, got %v", res.Warnings)
	}
}

// TestUninstallPrunesEmptyHome pins that when a data removal empties the CLI home
// (only the CLI's own lock files remain), the home dir itself is removed.
func TestUninstallPrunesEmptyHome(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	// Remove the binary (drops the manifest) + data (none present here), so the
	// only thing left in the CLI home is the lock file — home should be removed.
	res, err := c.Uninstall(UninstallOptions{RemoveBinary: true, RemoveData: true})
	if err != nil {
		t.Fatal(err)
	}
	if dirExists(l.Home()) {
		t.Errorf("expected the emptied CLI home to be removed; %s still exists", l.Home())
	}
	var sawHome bool
	for _, r := range res.Removed {
		if r == l.Home() {
			sawHome = true
		}
	}
	if !sawHome {
		t.Errorf("expected the CLI home in Removed, got %v", res.Removed)
	}
}

// TestUninstallClearKeysCallback pins that ClearKeys runs (and its output is
// merged) only under RemoveData.
func TestUninstallClearKeysCallback(t *testing.T) {
	newInstalled := func(t *testing.T) Config {
		home := t.TempDir()
		c := testConfig(home)
		c.exeOverride = writeFakeBinary(t, "BINARY-v1")
		c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
		if _, err := c.Install(); err != nil {
			t.Fatal(err)
		}
		c.exeOverride = c.Layout().ExecutablePath()
		return c
	}
	has := func(s []string, want string) bool {
		for _, v := range s {
			if v == want {
				return true
			}
		}
		return false
	}

	// With RemoveData: ClearKeys is called and merged.
	c := newInstalled(t)
	called := false
	res, err := c.Uninstall(UninstallOptions{RemoveData: true, ClearKeys: func() ([]string, []string) {
		called = true
		return []string{`API key "bot"`}, []string{"kept some secret"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("ClearKeys was not called for RemoveData")
	}
	if !has(res.Removed, `API key "bot"`) {
		t.Errorf("ClearKeys removed lines not merged: %v", res.Removed)
	}
	if !has(res.Warnings, "kept some secret") {
		t.Errorf("ClearKeys warnings not merged: %v", res.Warnings)
	}

	// Without RemoveData: ClearKeys must NOT run.
	c2 := newInstalled(t)
	ran := false
	if _, err := c2.Uninstall(UninstallOptions{RemoveBinary: true, ClearKeys: func() ([]string, []string) {
		ran = true
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("ClearKeys must not run without RemoveData")
	}
}

// TestUninstallEditPathFollowsSymlink pins that undoing the PATH edit on a
// symlinked rc file (a dotfiles-managed ~/.zshrc) rewrites the REAL target and
// keeps the symlink — it must not clobber the link with a regular-file copy.
func TestUninstallEditPathFollowsSymlink(t *testing.T) {
	home := t.TempDir()
	realDir := t.TempDir()
	target := filepath.Join(realDir, "zshrc")
	if err := os.WriteFile(target, []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zshrc := filepath.Join(home, ".zshrc")
	if err := os.Symlink(target, zshrc); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()
	if !strings.Contains(mustContent(t, target), pathBlockBegin) {
		t.Fatal("install did not write the block to the symlink target")
	}

	if _, err := c.Uninstall(UninstallOptions{EditPaths: []string{zshrc}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(zshrc)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error(".zshrc is no longer a symlink after the edit (the link was clobbered)")
	}
	if strings.Contains(mustContent(t, target), pathBlockBegin) {
		t.Error("block not removed from the symlink target")
	}
	if !strings.Contains(mustContent(t, target), "echo hi") {
		t.Error("user content lost from the symlink target")
	}
}

// TestBlockLinesMatchesRemoveBlockFrom pins that the diff preview (blockLines)
// covers exactly what removeBlockFrom deletes — including a hand-duplicated block
// — so the preview never under-represents the edit.
func TestBlockLinesMatchesRemoveBlockFrom(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rc")
	content := "a\n" + pathBlockBegin + "\nX\n" + pathBlockEnd + "\nb\n" + pathBlockBegin + "\nY\n" + pathBlockEnd + "\nc\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := len(blockLines(p)); got != 6 {
		t.Errorf("preview should cover both blocks (6 lines), got %d", got)
	}
	removed, err := removeBlockFrom(p)
	if err != nil || !removed {
		t.Fatalf("removeBlockFrom: removed=%v err=%v", removed, err)
	}
	got := mustContent(t, p)
	for _, gone := range []string{pathBlockBegin, "X", "Y"} {
		if strings.Contains(got, gone) {
			t.Errorf("removeBlockFrom left %q behind:\n%s", gone, got)
		}
	}
	for _, keep := range []string{"a", "b", "c"} {
		if !strings.Contains(got, keep) {
			t.Errorf("removeBlockFrom dropped kept line %q:\n%s", keep, got)
		}
	}
}

// TestUninstallKeepsHomeWhenManifestKept pins that a data removal that leaves a
// deliberately-kept item in the home (here the manifest, because the binary was
// not removed) does NOT report the home as a failed removal.
func TestUninstallKeepsHomeWhenManifestKept(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	c.PathConfirm = func(PathAddition) (bool, error) { return true, nil }
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()
	c.exeOverride = l.ExecutablePath()

	res, err := c.Uninstall(UninstallOptions{RemoveData: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 0 {
		t.Errorf("a deliberately-kept manifest must not be reported as a failure: %v", res.Failed)
	}
	if !dirExists(l.Home()) {
		t.Error("home removed even though the manifest was kept")
	}
	if !fileExists(l.ManifestPath()) {
		t.Error("manifest should be kept when RemoveBinary is false")
	}
}

func TestDoctor(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}
	l := c.Layout()

	c.exeOverride = l.ExecutablePath()
	rep, err := c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Managed {
		t.Error("doctor: expected managed")
	}
	if rep.InstalledVersion != "v1.0.0" {
		t.Errorf("doctor installed version = %q (want v1.0.0)", rep.InstalledVersion)
	}
	if !rep.ExecutableValid {
		t.Error("doctor: expected the installed binary to be valid (matches recorded sha256)")
	}
	// Not on PATH → doctor is not OK and names the problem.
	if rep.OK() {
		t.Error("doctor OK() should be false (binary dir not on PATH)")
	}

	// Corrupt the installed binary → doctor flags the sha256 mismatch.
	if err := os.WriteFile(l.ExecutablePath(), []byte("TAMPERED"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err = c.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if rep.ExecutableValid {
		t.Error("doctor should mark a tampered binary invalid")
	}
	// The mismatch problem is attached to the binary diagnosis (not a flat list).
	if p := rep.Problem(FieldBinary); !strings.Contains(p, "doesn't match") {
		t.Errorf("binary problem = %q (want a mismatch problem)", p)
	}
}

// TestInstallUpgradeIsNotARepair pins the fix for the false "replaced the stale
// binary" note: installing a DIFFERENT version over an existing install (a normal
// one-liner upgrade) is not a repair, while re-installing the SAME version whose
// on-disk bytes were corrupted IS.
func TestInstallUpgradeIsNotARepair(t *testing.T) {
	home := t.TempDir()
	c := testConfig(home)
	c.exeOverride = writeFakeBinary(t, "BINARY-v1")
	if _, err := c.Install(); err != nil {
		t.Fatal(err)
	}

	// Upgrade: a new version's binary installed over v1 — expected, not a repair.
	c.Version = "v2.0.0"
	c.exeOverride = writeFakeBinary(t, "BINARY-v2")
	res, err := c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstr(res.Repaired, "stale") {
		t.Errorf("a version upgrade was misreported as a repair: %v", res.Repaired)
	}
	if got := mustContent(t, c.Layout().ExecutablePath()); got != "BINARY-v2" {
		t.Errorf("binary after upgrade = %q (want BINARY-v2)", got)
	}

	// Corruption: same version, but the on-disk binary was mangled — that IS a repair.
	if err := os.WriteFile(c.Layout().ExecutablePath(), []byte("MANGLED"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = c.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(res.Repaired, "stale") {
		t.Errorf("a corrupted same-version binary should be repaired: %v", res.Repaired)
	}
}

// ---- helper unit tests ----

func TestParseChecksums(t *testing.T) {
	data := []byte("aa11  digitalx-cli_linux_amd64.tar.gz\nbb22 *digitalx-cli_windows_amd64.zip\n\n")
	m := parseChecksums(data)
	if m["digitalx-cli_linux_amd64.tar.gz"] != "aa11" {
		t.Errorf("linux hash = %q", m["digitalx-cli_linux_amd64.tar.gz"])
	}
	if m["digitalx-cli_windows_amd64.zip"] != "bb22" { // "*" binary-mode marker stripped
		t.Errorf("windows hash = %q", m["digitalx-cli_windows_amd64.zip"])
	}
}

func TestExtractBinary(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		bin := "dgx-cli"
		if goos == "windows" {
			bin = "dgx-cli.exe"
		}
		archive := makeArchive(t, goos, bin, "PAYLOAD")
		got, err := extractBinary(archive, goos, bin)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if string(got) != "PAYLOAD" {
			t.Errorf("%s: extracted %q", goos, got)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{"1.2.3": "v1.2.3", "v1.2.3": "v1.2.3", "": "", "dev": "dev"} {
		if got := normalizeTag(in); got != want {
			t.Errorf("normalizeTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- test doubles ----

// fakeDoer serves a synthetic release: the GitHub releases/latest redirect (via
// resp.Request.URL), the platform archive, checksums.txt, its detached signature
// (checksums.txt.sig), and the release-signing certificate on the docs host.
//
// Its zero value serves no cert (docs 404) and no signature, so a test that
// reaches the signature check must set kit — the happy path — or exercise one of
// the failure knobs below.
type fakeDoer struct {
	repo           string
	tag            string
	asset          string
	archive        []byte
	tamperChecksum bool // advertise a wrong hash in checksums.txt

	// kit signs checksums.txt and backs the published cert. When nil, the cert
	// endpoint 404s and the signature endpoint 404s.
	kit *signerKit
	// certBody, when non-nil, overrides the cert endpoint body (for the
	// empty-body kill switch and soft-404 cases); certStatus overrides its status.
	certBody   []byte
	certStatus int
	// signWrong signs bytes other than the served checksums.txt, so a
	// well-formed-but-invalid signature is served (tampering).
	signWrong bool
	// omitSig serves the signature endpoint as a 404 even when kit is set.
	omitSig bool
	// certErr makes the cert endpoint fail at the transport level (DNS/TLS/conn),
	// so the Doer returns an error rather than any HTTP response.
	certErr bool

	// manifest, when non-nil, is the release-manifest.json body served; nil
	// serves a 404 there, the no-manifest release every other test in this file
	// exercises.
	manifest []byte
	// manifestUnlisted serves the manifest without listing it in checksums.txt,
	// so it arrives unauthenticated.
	manifestUnlisted bool
	// manifestBadSum lists the manifest in checksums.txt under a hash that does
	// not match the body served.
	manifestBadSum bool
	// manifestStatus overrides the manifest endpoint's status (0 = the default
	// 404-when-absent / 200-when-present); manifestErr makes it fail at the
	// transport level — the two non-404 ways the manifest can be unobtainable.
	manifestStatus int
	manifestErr    bool
	// manifestRequested records whether the manifest endpoint was hit at all, so
	// a test can assert it is NOT reached before the signature is verified.
	manifestRequested bool
}

// checksumsBody is the exact checksums.txt served (and, unless signWrong,
// signed), so the signature covers the same bytes the verifier reads back. The
// manifest is listed alongside the archive, which is what authenticates it.
func (d *fakeDoer) checksumsBody() []byte {
	h := sha256.Sum256(d.archive)
	hexsum := hex.EncodeToString(h[:])
	if d.tamperChecksum {
		hexsum = strings.Repeat("0", 64)
	}
	body := hexsum + "  " + d.asset + "\n"
	if d.manifest != nil && !d.manifestUnlisted {
		mh := sha256.Sum256(d.manifest)
		msum := hex.EncodeToString(mh[:])
		if d.manifestBadSum {
			msum = strings.Repeat("1", 64)
		}
		body += msum + "  " + releaseManifestName + "\n"
	}
	return []byte(body)
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	switch {
	case strings.HasSuffix(u, "/releases/latest"):
		// The real client follows the redirect; expose the resolved tag URL.
		final, _ := url.Parse("https://github.com/" + d.repo + "/releases/tag/" + d.tag)
		return resp(200, nil, &http.Request{URL: final}), nil
	case strings.HasSuffix(u, "/"+d.asset):
		return resp(200, d.archive, req), nil
	case strings.HasSuffix(u, "release-signing-cert.pem"):
		if d.certErr {
			return nil, errors.New("dial tcp: connection refused")
		}
		if d.certStatus != 0 || d.certBody != nil {
			status := d.certStatus
			if status == 0 {
				status = 200
			}
			return resp(status, d.certBody, req), nil
		}
		if d.kit != nil {
			return resp(200, d.kit.certPEM, req), nil
		}
		return resp(404, []byte("not found"), req), nil
	case strings.HasSuffix(u, "/"+releaseManifestName):
		d.manifestRequested = true
		if d.manifestErr {
			return nil, errors.New("dial tcp: connection refused")
		}
		if d.manifestStatus != 0 && d.manifestStatus != 200 {
			return resp(d.manifestStatus, []byte("unavailable"), req), nil
		}
		if d.manifest == nil {
			return resp(404, []byte("not found"), req), nil
		}
		return resp(200, d.manifest, req), nil
	case strings.HasSuffix(u, "/checksums.txt.sig"):
		if d.kit == nil || d.omitSig {
			return resp(404, []byte("not found"), req), nil
		}
		signed := d.checksumsBody()
		if d.signWrong {
			signed = append([]byte("tampered"), signed...)
		}
		return resp(200, d.kit.sign(signed), req), nil
	case strings.HasSuffix(u, "/checksums.txt"):
		return resp(200, d.checksumsBody(), req), nil
	}
	return resp(404, []byte("not found"), req), nil
}

// signerKit is a throwaway RSA keypair plus its self-signed cert, standing in
// for the release-signing key and the cert published on the docs host.
type signerKit struct {
	certPEM []byte
	priv    *rsa.PrivateKey
}

// newSignerKit generates a 2048-bit RSA keypair and a self-signed cert wrapping
// its public key — the same shape as the real release-signing cert.
func newSignerKit(t *testing.T) *signerKit {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "digitalx-cli release signing (test)"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return &signerKit{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		priv:    priv,
	}
}

// sign produces a detached RSA/SHA-256 (PKCS#1 v1.5) signature over b, matching
// goreleaser's openssl-sign hook.
func (s *signerKit) sign(b []byte) []byte {
	h := sha256.Sum256(b)
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.priv, crypto.SHA256, h[:])
	if err != nil {
		panic(err)
	}
	return sig
}

func resp(code int, body []byte, req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
}

// makeArchive builds a tar.gz (or zip for windows) holding one file named bin
// with the given content — a stand-in for a release archive.
func makeArchive(t *testing.T, goos, bin, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if goos == "windows" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(bin)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: bin, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func containsSubstr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func asProvenance(err error, target **ProvenanceError) bool {
	for err != nil {
		if pe, ok := err.(*ProvenanceError); ok {
			*target = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
