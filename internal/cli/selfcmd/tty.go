// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfcmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/korbit-official/korbit-cli/internal/cli/clienv"
)

// openTTYConfirm returns a yes/no confirm (with a per-question default) backed by
// the controlling terminal, or nil when there is no terminal to prompt on. The
// install script pipes into `sh`, so stdin is the pipe — a prompt must talk to
// /dev/tty directly. When that isn't available (no controlling terminal, a CI
// runner, or Windows, where /dev/tty doesn't exist and PATH wiring is
// non-interactive anyway), it returns nil so PATH setup falls back to printing
// instructions. The question text is written to /dev/tty, not stdout, so it never
// pollutes the machine-readable result.
func openTTYConfirm() clienv.Confirm {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	if !term.IsTerminal(tty.Fd()) {
		_ = tty.Close()
		return nil
	}
	reader := bufio.NewReader(tty)
	return func(question string, defaultYes bool) (bool, error) {
		hint := "[y/N]"
		if defaultYes {
			hint = "[Y/n]"
		}
		fmt.Fprintf(tty, "%s %s ", question, hint)
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF / read error (Ctrl-D, a closed terminal): decline. A bare Enter
			// takes the shown default, but an aborted/closed input must never silently
			// edit the user's dotfiles.
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}

// colorize renders the install/uninstall PATH diff previews with ANSI color when
// enabled. It is disabled when stderr is not a terminal, NO_COLOR is set, or
// TERM=dumb, so piped/CI output stays plain.
type colorize struct{ enabled bool }

// detectColor decides whether to color the PATH diff previews (written to
// stderr).
func detectColor(getenv func(string) string) colorize {
	return colorize{enabled: term.IsTerminal(os.Stderr.Fd()) && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"}
}

// red wraps s in red (a removed diff line); a no-op when color is disabled.
func (c colorize) red(s string) string {
	if !c.enabled {
		return s
	}
	return "\x1b[31m" + s + "\x1b[0m"
}

// green wraps s in green (an added diff line); a no-op when color is disabled.
func (c colorize) green(s string) string {
	if !c.enabled {
		return s
	}
	return "\x1b[32m" + s + "\x1b[0m"
}

// dim wraps s in dim (a line number); a no-op when color is disabled.
func (c colorize) dim(s string) string {
	if !c.enabled {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// StdinConfirmer returns a clienv.Confirm backed by the process stdin, or nil
// when stdin is not a terminal. Unlike the install prompt it does NOT use
// /dev/tty: `self uninstall` is invoked directly (never piped like the install
// script), so its stdin IS the terminal — and reading stdin also works on
// Windows, where /dev/tty does not exist. A nil return is how the caller detects
// "not interactive" and refuses to run. Questions are written to w (stderr).
func StdinConfirmer(w io.Writer) clienv.Confirm {
	if !term.IsTerminal(os.Stdin.Fd()) {
		return nil
	}
	reader := bufio.NewReader(os.Stdin)
	return func(question string, defaultYes bool) (bool, error) {
		hint := "[y/N]"
		if defaultYes {
			hint = "[Y/n]"
		}
		fmt.Fprintf(w, "%s %s ", question, hint)
		line, err := reader.ReadString('\n')
		if err != nil {
			return defaultYes, nil // EOF/read error → the default, not a hard failure
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes, nil
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}
