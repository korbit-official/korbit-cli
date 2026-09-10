// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package selfupdate

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/korbit-official/korbit-cli/internal/progname"
)

// unsafeToRewrite has no unix ownership/hard-link semantics to check on Windows,
// where PATH is edited in the registry rather than by rewriting a file, so it
// always reports the rewrite safe. It exists only so the shared removeBlockFrom
// (a unix-only code path) compiles here.
func unsafeToRewrite(os.FileInfo) string { return "" }

// wirePath ensures the installed binary dir is on the User PATH on Windows: it adds the
// dir to HKCU\Environment\Path (no admin needed) when absent, then broadcasts
// WM_SETTINGCHANGE so new shells pick it up. The current shell still needs a
// restart. If it is already present, it does nothing.
func (c Config) wirePath(dir string) (PathResult, error) {
	pr := PathResult{Dir: dir}
	// effectivelyOnPath consults both the process env and the persisted User PATH,
	// so a re-run right after a prior install (registry set, process env stale) is
	// still recognized as already-wired.
	if c.effectivelyOnPath(dir) {
		pr.OnPath = true
		pr.Action = pathActionAlready
		c.log().Debug("PATH already configured", "dir", dir)
		return pr, nil
	}
	// When a confirm is available, show the pending User-PATH entry and ask; a
	// decline leaves the registry untouched and reports the manual step. With no
	// confirm (no controlling terminal) the write stays silent, as before.
	if c.PathConfirm != nil {
		if adds := c.pathAdditions(dir); len(adds) > 0 {
			ok, err := c.PathConfirm(adds[0])
			if err != nil {
				return pr, err
			}
			if !ok {
				pr.Action = pathActionInstructions
				pr.Hint = "add " + dir + " to your User PATH to run " + progname.Name() + " from any directory"
				return pr, nil
			}
		}
	}
	cur, err := readUserPath()
	if err != nil {
		return pr, err
	}
	updated := dir
	if cur != "" {
		updated = dir + ";" + cur
	}
	if err := writeUserPath(updated); err != nil {
		return pr, err
	}
	broadcastEnvChange()
	pr.Action = pathActionAddedUser
	pr.Hint = "added to your User PATH — restart your shell (or sign out/in) to pick it up"
	c.log().Debug("added dir to User PATH", "dir", dir)
	return pr, nil
}

// userPathLocation is how the User PATH is named to the user, so install and
// uninstall can point at it whether they add, remove, or keep the entry.
const userPathLocation = `your User PATH (HKCU\Environment\Path)`

// pathAdditions returns the pending PATH-add edit for the User PATH — a single
// PathAddition whose one line (Num 0: not a file line) is the install dir to be
// added — or nil when the dir is already on the User PATH.
func (c Config) pathAdditions(dir string) []PathAddition {
	if c.effectivelyOnPath(dir) {
		return nil
	}
	return []PathAddition{{Location: userPathLocation, Added: []DiffLine{{Text: dir}}}}
}

// pathEdits returns the pending PATH-undo edit for the User PATH — a single
// PathEdit whose one diff line is the install dir to be removed (Num 0: not a
// file line) — or nil when the dir is not on the User PATH.
func (c Config) pathEdits() []PathEdit {
	dir := c.Layout().ExecutableDir()
	cur, err := readUserPath()
	if err != nil {
		return nil
	}
	if !pathListContains(cur, dir) {
		return nil
	}
	return []PathEdit{{Location: userPathLocation, Lines: []DiffLine{{Text: dir}}}}
}

// applyPathEdit removes the installed binary dir from the User PATH, keeping every
// other entry, and broadcasts the change. location is the User PATH label (there
// is only one on windows). Returns whether it changed anything.
func (c Config) applyPathEdit(location string) (bool, error) {
	dir := c.Layout().ExecutableDir()
	cur, err := readUserPath()
	if err != nil {
		return false, err
	}
	if !pathListContains(cur, dir) {
		return false, nil
	}
	var kept []string
	for _, p := range strings.Split(cur, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			continue
		}
		if p != "" {
			kept = append(kept, p)
		}
	}
	if err := writeUserPath(strings.Join(kept, ";")); err != nil {
		return false, err
	}
	broadcastEnvChange()
	c.log().Debug("removed dir from User PATH", "dir", dir)
	return true, nil
}

// effectivelyOnPath reports whether dir is on PATH as a new shell will see it:
// the current process PATH, or — since a just-written registry entry is not in
// this process's environment yet — the persisted User PATH in the registry. This
// is the authoritative source on Windows, so doctor run right after install
// reports correctly instead of a false "not on PATH".
func (c Config) effectivelyOnPath(dir string) bool {
	if c.dirOnPath(dir) {
		return true
	}
	cur, err := readUserPath()
	if err != nil {
		return false
	}
	return pathListContains(cur, dir)
}

func pathListContains(list, dir string) bool {
	for _, p := range strings.Split(list, ";") {
		if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
			return true
		}
	}
	return false
}

func readUserPath() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if err == registry.ErrNotExist {
		return "", nil
	}
	return v, err
}

func writeUserPath(v string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	// EXPAND_SZ preserves any %VAR% references a user may have in their PATH.
	return k.SetExpandStringValue("Path", v)
}

// broadcastEnvChange posts WM_SETTINGCHANGE("Environment") to all top-level
// windows so already-running shells refresh their environment. Best-effort.
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")
	env, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	_, _, _ = proc.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(env)),
		uintptr(smtoAbortIfHung),
		5000,
		uintptr(unsafe.Pointer(&result)),
	)
}
