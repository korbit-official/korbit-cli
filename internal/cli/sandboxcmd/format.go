// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandboxcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/sandbox"
	"github.com/korbit-official/korbit-cli/internal/sandbox/deno"
)

// The sandbox lifecycle results are defined in the lower internal/sandbox layer,
// which must not depend on the human-output toolkit. These thin view types wrap
// each result so the command package owns its rendering: each embeds the result
// (so its --json shape is byte-identical) and implements textout.TextFormatter,
// reading typed fields directly rather than through a keyed JSON formatter.

type startView struct {
	sandbox.StartResult
	Reminder string `json:"reminder,omitempty"`
}

func (v startView) FormatText(w io.Writer) {
	mode := "simulated market"
	if v.Paper {
		mode = "paper trading (live production market data; fills simulated)"
		// Partial paper: name the pairs whose market data is NOT mirrored (state,
		// not cause — a pair may be non-launched on production, or flipped to the
		// walk by hand) so the mode line never overpromises.
		if len(v.WalkPairs) > 0 {
			mode = fmt.Sprintf(
				"paper trading (%d of %d pairs mirror live production data; fills simulated; %s on the simulated market)",
				v.PairCount-len(v.WalkPairs), v.PairCount, strings.Join(v.WalkPairs, ", "))
		}
	}
	rows := [][2]string{
		{"REST", v.RestBaseURL},
		{"WebSocket", v.WSBaseURL},
		{"mode", mode},
		{"key", v.KeyName},
		{"apiKeyId", v.APIKeyID},
		{"runtime", v.Runtime},
		{"pid", fmt.Sprintf("%d", v.PID)},
		{"port", fmt.Sprintf("%d", v.Port)},
		{"db", v.DB},
		{"log", v.LogPath},
	}
	head := "Sandbox running."
	switch {
	case v.Recreated:
		head = "Sandbox running (fresh database — previous data discarded)."
	case !v.Imported:
		head = "Sandbox running (key re-pinned)."
	}
	fmt.Fprintf(w, "%s\n%s\n\nUse it with: %s whoami --key %s", head, textout.KVBlock(rows), progname.Name(), v.KeyName)
	if v.Reminder != "" {
		fmt.Fprintf(w, "\n\n%s", v.Reminder)
	}
}

type stopView struct{ sandbox.StopResult }

func (v stopView) FormatText(w io.Writer) {
	if v.Stopped {
		fmt.Fprintf(w, "Sandbox stopped (was PID %d on port %d).", v.PID, v.Port)
		return
	}
	fmt.Fprint(w, "No running sandbox to stop.")
}

type statusView struct {
	sandbox.StatusResult
	Reminder string `json:"reminder,omitempty"`
}

func (v statusView) FormatText(w io.Writer) {
	bundle := v.SourceURL
	if bundle == "" {
		bundle = "(unknown source)"
	}
	if v.BundleCached {
		bundle += " (cached)"
	} else {
		bundle += " (not yet fetched)"
	}
	server := "stopped"
	if v.Server.Running {
		reach := "reachable"
		if !v.Server.Reachable {
			reach = "not responding"
		}
		server = fmt.Sprintf("running (pid %d, port %d, %s)", v.Server.PID, v.Server.Port, reach)
	}
	// The "deno" row reflects the resolved runtime: a system deno is just its path;
	// the managed deno shows its cached-vs-pinned state.
	denoState := "managed, not installed"
	if v.Runtime == string(sandbox.RuntimeSystemDeno) {
		denoState = "system — " + v.DenoPath
	} else if v.DenoStatus.Installed {
		state := "matches pin"
		if !v.DenoStatus.UpToDate {
			state = "needs update (pinned " + v.DenoStatus.PinnedVersion + ")"
		}
		denoState = fmt.Sprintf("managed %s (%s)", v.DenoStatus.CachedVersion, state)
	}
	key := "not imported"
	if v.KeyImported {
		key = "imported as " + v.KeyName
	}
	rows := [][2]string{
		{"cache", v.CachePath},
		{"bundle", bundle},
		{"runtime", v.Runtime},
		{"deno", denoState},
		{"server", server},
		{"db", v.DB},
		{"key", key},
	}
	// This report is runtime-free (nothing is downloaded or started), so market
	// modes and paper-trading health live in the bundle's own status — point at
	// it while a server is up to answer.
	if v.Server.Running {
		rows = append(rows, [2]string{
			"detail",
			fmt.Sprintf("`%s sandbox exec status` shows each pair's market mode and paper-trading health (mirror age, live-feed state)", progname.Name()),
		})
	}
	fmt.Fprint(w, textout.KVBlock(rows))
	if v.Reminder != "" {
		fmt.Fprintf(w, "\n\n%s", v.Reminder)
	}
}

type runtimeView struct{ deno.Status }

func (v runtimeView) FormatText(w io.Writer) {
	state := "not installed"
	if v.Installed {
		state = "installed but out of date"
		if v.UpToDate {
			state = "installed, matches the pinned version"
		}
	}
	rows := [][2]string{
		{"managed Deno", state},
		{"cached version", v.CachedVersion},
		{"pinned version", v.PinnedVersion},
		{"target", v.Target},
		{"path", v.Path},
	}
	if v.Installed && !v.UpToDate {
		fmt.Fprintf(w, "%s\n\nUpdate it with `%s sandbox runtime update`.", textout.KVBlock(rows), progname.Name())
		return
	}
	fmt.Fprint(w, textout.KVBlock(rows))
}
