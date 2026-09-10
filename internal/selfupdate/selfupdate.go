// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package selfupdate owns the on-disk install layout and the self-update
// mechanics behind the `self install|update|uninstall|doctor` commands. It is a
// plain internal package (like internal/agentskill): the cli layer (cli/selfcmd)
// builds a Config from the invocation environment and calls the operations here,
// which do the file/network work and return result structs; no cli-facing
// formatting lives here.
//
// The model is a single installed binary on PATH plus a manifest. The binary
// sits at a stable name on PATH (~/.local/bin/dgx-cli on unix,
// %LOCALAPPDATA%\bin\dgx-cli.exe on windows) — a real file, not a symlink
// (Windows file symlinks need admin/Developer Mode). <home>/install.json records
// what was installed (version, sha256, provenance, aliases) and is what gates
// self update. self update replaces the binary in place with
// github.com/minio/selfupdate, which also handles the Windows running-exe swap.
//
// An install may additionally carry ALIASES — extra command names on PATH in the
// same directory that run the same binary (a relative symlink on unix, a second
// copy on windows, since an unprivileged Windows install has no usable file
// symlink). Layout.LegacyBinName is the one alias name this package manages, so
// the `korbit` command keeps working on an install that has it. install and
// update keep every alias current; uninstall removes them with the primary.
//
// Trust is TLS + sha256 + a release signature: self update fetches the release
// checksums.txt and verifies the downloaded archive's sha256 against it before
// the swap, and — gated by a release-signing certificate published on the
// trusted docs host (a different host than the GitHub release, so it defends
// against a compromised release) — verifies the detached RSA signature over
// checksums.txt. See verify.go for the full policy (including the empty-cert
// kill switch and why any fetch failure is fatal).
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/korbit-official/korbit-cli/internal/config"
	"github.com/korbit-official/korbit-cli/internal/logging"
)

// DefaultRepo is the public release repository self update resolves against.
const DefaultRepo = "korbit-official/korbit-cli"

// DefaultReleaseCertURL is where self update fetches the release-signing
// certificate to verify a release's checksums.txt signature. It is served from
// the managed docs host — deliberately a different, access-controlled host than
// the GitHub release it authenticates — so the pin defends against a compromised
// release even though the archive, its checksums, and the signature all come
// from GitHub. See verify.go for the fetch/verify policy.
const DefaultReleaseCertURL = "https://docs.korbit.co.kr/release-signing-cert.pem"

// MethodManagedScript is the manifest `method` for an install created by the
// managed install script / `self install`. It is the only method self update
// operates on; any other provenance (go install, Homebrew, a hand-downloaded
// archive, a dev build) is refused with guidance.
const MethodManagedScript = "managed-script"

// devVersion is the version string a source build carries (see internal/version).
// A dev build has unknown provenance, so the self-management commands refuse it.
const devVersion = "dev"

// Doer performs an HTTP request. *http.Client satisfies it, as does the CLI's
// shared apiclient.Doer; tests inject a stub. Declared locally so the package does
// not depend on internal/apiclient.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config is the injected environment one self-management operation runs against,
// so the whole package is unit-testable against a temp home, a stub HTTP client,
// and a fixed clock.
type Config struct {
	// Getenv reads the process environment (KORBIT_CLI_HOME, HOME/USERPROFILE,
	// LOCALAPPDATA, PATH). Injectable so tests point it at a temp dir.
	Getenv func(string) string
	// Now is the local clock in unix-ms, stamped into the manifest's installedAt.
	Now func() int64
	// Doer is the HTTP client self update / version resolution run through
	// (nil is fine for install/uninstall/doctor, which touch no network).
	Doer Doer
	// GOOS / GOARCH are the target platform (runtime.GOOS/GOARCH in production;
	// injectable so the path layout can be tested for another OS on this host).
	GOOS   string
	GOARCH string
	// Version is the running binary's version (internal/version.Version). A "dev"
	// value marks an unmanaged source build and is refused.
	Version string
	// Repo is the release repository (owner/name); empty means DefaultRepo.
	Repo string
	// ReleaseCertURL is the URL of the release-signing certificate on the trusted
	// docs host; empty means DefaultReleaseCertURL. Injectable so tests point it
	// at a stub endpoint.
	ReleaseCertURL string
	// PathConfirm is asked once per PATH location that needs the installer's entry
	// (each shell rc file on unix; the User PATH on windows) and returns whether to
	// apply it. nil means non-interactive (no TTY reachable) — PATH wiring then only
	// prints guidance instead of editing dotfiles (unix) / keeps the silent write
	// (windows). The cli layer wires a /dev/tty-backed confirm that renders the
	// addition's diff preview and defaults an empty answer to yes.
	PathConfirm func(add PathAddition) (bool, error)
	// Progress is the human progress/instruction sink (stderr). It is never the
	// result: everything an agent needs is in the returned struct.
	Progress io.Writer
	// Logger is the optional operational logger for the fragile network/filesystem
	// path (release resolution, each fetch, the signature/checksum verdicts, the
	// in-place binary swap, manifest writes, PATH edits). nil = silent (resolved
	// via logging.Or in log). It is distinct from Progress: Progress is program
	// output, Logger is the level-gated --debug/--log-level telemetry.
	Logger *slog.Logger

	// exeOverride replaces os.Executable() as the "running binary" — a test seam
	// (white-box) so Install can be pointed at a fixture file and Update/Doctor can
	// simulate running from the managed executable. Empty in production.
	exeOverride string
}

// runningExe returns the running binary's resolved path (the source Install
// copies onto PATH, and the anchor Update/Doctor test against the installed
// binary), honoring the exeOverride test seam.
func (c Config) runningExe() (string, error) {
	if c.exeOverride != "" {
		return c.exeOverride, nil
	}
	return resolveExecutable()
}

// repo returns the effective release repository.
func (c Config) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultRepo
}

// releaseCertURL returns the effective release-signing certificate URL.
func (c Config) releaseCertURL() string {
	if c.ReleaseCertURL != "" {
		return c.ReleaseCertURL
	}
	return DefaultReleaseCertURL
}

// progressf writes a human progress/instruction line to c.Progress (no-op if
// unset). It never carries anything the result struct doesn't also carry.
func (c Config) progressf(format string, a ...any) {
	if c.Progress == nil {
		return
	}
	fmt.Fprintf(c.Progress, format+"\n", a...)
}

// log returns the operational logger, never nil (logging.Or), so call sites log
// unconditionally; a nil Logger is silent.
func (c Config) log() *slog.Logger { return logging.Or(c.Logger) }

// now returns the local clock in unix-ms, tolerating a nil Now.
func (c Config) now() int64 {
	if c.Now != nil {
		return c.Now()
	}
	return 0
}

// requireManaged rejects a dev build up front — the one provenance check every
// mutating command shares (install of a dev build, or updating one, is
// nonsensical: there is no release to place or resolve against).
func (c Config) requireReleaseBuild() error {
	if c.Version == "" || c.Version == devVersion {
		return fmt.Errorf("this is a development build (version %q), not a released one — the self-management commands only manage a binary installed from a release", c.Version)
	}
	return nil
}

// ---- layout ----

// Layout computes the install paths from the environment. Paths derive from two
// roots: the CLI home (config.Home — the manifest + lock) and the OS user home
// (the installed binary dir), so $KORBIT_CLI_HOME relocates the manifest without
// moving the installed binary.
type Layout struct {
	getenv func(string) string
	goos   string
}

// Layout returns the path layout for this configuration.
func (c Config) Layout() Layout {
	return Layout{getenv: orGetenv(c.Getenv), goos: c.os()}
}

// Home is the CLI home (config.Home): $KORBIT_CLI_HOME else ~/.korbit-cli. The
// manifest and the lock live under it.
func (l Layout) Home() string { return config.Home(l.getenv) }

// ManifestPath is the install manifest: <home>/install.json.
func (l Layout) ManifestPath() string { return filepath.Join(l.Home(), "install.json") }

// LockPath is the advisory lock serializing install/update/uninstall against a
// concurrent korbit-cli process mutating the same store.
func (l Layout) LockPath() string { return filepath.Join(l.Home(), "self.lock") }

// BinName is the installed binary/stored binary's filename: dgx-cli, or
// dgx-cli.exe on Windows. It is the fixed canonical name (not the
// possibly-renamed program basename) so the installed binary is predictable.
func (l Layout) BinName() string {
	if l.goos == "windows" {
		return "dgx-cli.exe"
	}
	return "dgx-cli"
}

// LegacyBinName is the alias command name an install may carry alongside the
// primary binary: korbit, or korbit.exe on Windows. An install that has it on
// PATH keeps it working — pointed at the primary binary — so both command names
// run the same version.
func (l Layout) LegacyBinName() string {
	if l.goos == "windows" {
		return "korbit.exe"
	}
	return "korbit"
}

// ExecutableDir is the directory on PATH the active binary is copied into:
// ~/.local/bin on unix, %LOCALAPPDATA%\bin (= %USERPROFILE%\AppData\Local\bin)
// on Windows.
func (l Layout) ExecutableDir() string {
	if l.goos == "windows" {
		if la := l.getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "bin")
		}
		return filepath.Join(l.userHome(), "AppData", "Local", "bin")
	}
	return filepath.Join(l.userHome(), ".local", "bin")
}

// ExecutablePath is the active binary's stable path on PATH.
func (l Layout) ExecutablePath() string { return filepath.Join(l.ExecutableDir(), l.BinName()) }

// LegacyExecutablePath is where the LegacyBinName alias sits — next to the
// primary binary, so one PATH entry serves both command names.
func (l Layout) LegacyExecutablePath() string {
	return filepath.Join(l.ExecutableDir(), l.LegacyBinName())
}

// AliasPath is where the alias named name sits: in the install dir, next to the
// primary binary.
func (l Layout) AliasPath(name string) string { return filepath.Join(l.ExecutableDir(), name) }

// userHome resolves the OS user's home directory the way os.UserHomeDir does —
// USERPROFILE on Windows, HOME elsewhere — but through the injected getenv so
// tests can redirect it (see agentskillcmd.agentHomeFor for the same rationale).
func (l Layout) userHome() string {
	envVar := "HOME"
	if l.goos == "windows" {
		envVar = "USERPROFILE"
	}
	if h := l.getenv(envVar); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// ---- filesystem helpers ----

// orGetenv returns getenv, or os.Getenv when nil, so a zero Config still works
// against the real environment.
func orGetenv(getenv func(string) string) func(string) string {
	if getenv != nil {
		return getenv
	}
	return os.Getenv
}

// tmpPattern is the os.CreateTemp pattern for this package's scratch files. It
// is a hidden name in the destination directory so the write + rename stays
// atomic on one filesystem, and sweepLeftovers can recognize and clear one an
// interrupted run left behind.
const tmpPattern = ".dgx-cli-*.tmp"

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// copyFileAtomic copies src to dst with the given mode, atomically: it writes a
// temp file in dst's directory and renames it over dst, so a crash mid-copy
// never leaves a half-written binary at dst. dst's directory must already exist.
func copyFileAtomic(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), tmpPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// writeBytesAtomic writes data to path with the given mode via temp file +
// rename. path's directory must already exist.
func writeBytesAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// sha256File returns the lowercase hex sha256 of a file's contents.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256Bytes returns the lowercase hex sha256 of a byte slice.
func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// dirOnPath reports whether dir is one of the entries in the PATH environment
// variable, comparing cleaned paths so a trailing slash or "." segment doesn't
// hide a match.
func (c Config) dirOnPath(dir string) bool {
	want := filepath.Clean(dir)
	for _, p := range filepath.SplitList(orGetenv(c.Getenv)("PATH")) {
		if p == "" {
			continue
		}
		if filepath.Clean(p) == want {
			return true
		}
	}
	return false
}

// resolveExecutable returns the absolute, symlink-resolved path of the running
// binary — the source self install copies onto PATH, and the anchor
// self update / doctor use to confirm the running binary is the installed one.
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// resolveOrClean returns an absolute, symlink-resolved form of path for use as a
// comparison key: two paths run through it are equal when they point at the same
// real file even if a component is a symlink — e.g. $HOME is a symlink onto
// another volume, so ~/.local/bin/dgx-cli and its resolved /mnt/.../dgx-cli form
// are the same binary. os.Executable already resolves the running binary, so a
// raw filepath.Clean of a layout path would spuriously differ from it.
//
// It absolutizes before resolving because filepath.EvalSymlinks preserves a
// relative input as a relative result (and only makes it absolute if a component
// is an absolute symlink); a relative-vs-absolute comparison would then never
// match. Relative symlink *targets* within the tree (e.g. korbit -> dgx-cli)
// are resolved correctly by EvalSymlinks regardless, once the input is absolute.
//
// The two fallbacks are best-effort and, in practice, unreachable for the callers
// here (which pass absolute paths): filepath.Abs only fails for a relative path
// when os.Getwd fails, and with no cwd a relative path cannot be absolutized, so
// the cleaned original is all that remains; EvalSymlinks fails when the path does
// not exist yet, leaving the already-absolute form.
func resolveOrClean(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path) // relative path + no cwd: nothing better available
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// isInstalledBinary reports whether path is the managed installed binary on PATH
// — the check that keeps a copy dragged elsewhere (or a Homebrew/go-install
// binary) from being treated as a managed install and updated in place.
// Comparison resolves symlinks on both sides so a symlinked home (or bin dir)
// does not make the running binary look unmanaged; two paths that resolve to the
// same real file are the same binary, and a genuinely different file still won't
// match.
func (l Layout) isInstalledBinary(path string) bool {
	return resolveOrClean(path) == resolveOrClean(l.ExecutablePath())
}

// isLegacyBinary reports whether path is the install's alias name in the install
// dir. On unix the alias is a symlink onto the primary binary, so a running
// alias already satisfies isInstalledBinary; this additionally covers the layout
// where the binary on PATH is a real file under the alias name and the primary
// name is not there yet.
func (l Layout) isLegacyBinary(path string) bool {
	return resolveOrClean(path) == resolveOrClean(l.LegacyExecutablePath())
}

// isManagedBinary reports whether path is a binary this install owns — the
// primary on PATH, or its alias name in the same directory. It is the gate
// self update / uninstall use, so an install running under either name manages
// itself while a copy dragged elsewhere still does not.
func (l Layout) isManagedBinary(path string) bool {
	return l.isInstalledBinary(path) || l.isLegacyBinary(path)
}

// defaultGOOS / defaultGOARCH fill a zero Config from the build's own platform.
func (c Config) os() string {
	if c.GOOS != "" {
		return c.GOOS
	}
	return runtime.GOOS
}

func (c Config) arch() string {
	if c.GOARCH != "" {
		return c.GOARCH
	}
	return runtime.GOARCH
}
