// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package spec declares the BUILTIN command surface (setup, doctor, ip, key and
// keystore management, monitor, mcp, logs, debug, commands, license, sandbox) and reuses
// the shared command-vocabulary types — Param, Positional, the value kinds, the
// section labels, ResponseField — from internal/cmdmeta. The endpoint command
// surface lives in the ops catalog; the cli layer merges the two into one view.
// Help text, the machine-readable catalog, and the cobra command tree derive
// from the merged surface, so a builtin is one entry in this registry.
package spec

import (
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
)

// Command is one CLI command (one to three id segments, e.g. ["ticker"],
// ["order","place"], or ["sandbox","runtime","status"]). Segments before the
// last name the enclosing command groups, which are synthesized in the cobra
// tree and never have their own Command entry.
type Command struct {
	ID          []string
	Section     cmdmeta.Section
	Summary     string
	Params      []cmdmeta.Param
	Positionals []cmdmeta.Positional
	Notes       []string
	Examples    []string
	// ExperimentalNotes / ExperimentalExamples document an opt-in, not-yet-stable
	// feature. Plain `--help` hides them; help shown with --enable-experimental
	// appends them after the stable Notes/Examples. The machine catalog and the
	// MCP bot_runtime_reference always include them.
	ExperimentalNotes    []string
	ExperimentalExamples []string
	// Response is the success-response field hints (empty for builtins, which
	// make no API call).
	Response []cmdmeta.ResponseField
	// Hidden keeps a command out of the user-facing surfaces — the grouped root
	// help, a group's subcommand list, and the machine `commands` catalog — while
	// leaving it fully dispatchable (its cobra leaf is built and `<cmd> --help`
	// still works). It is for a command that is a private implementation detail,
	// not an agent operation: `self install`, invoked only by the managed install
	// script.
	Hidden bool
}

// GlobalFlag is a flag accepted by every command.
type GlobalFlag struct {
	Flag       string
	TakesValue bool
	Desc       string
}

// GlobalFlags are accepted by every command.
var GlobalFlags = []GlobalFlag{
	{Flag: "key", TakesValue: true, Desc: "named key to sign with (default: the key set via `korbit-cli key use`; also: DIGITALX_CLI_KEY). Or supply a key inline via DIGITALX_CLI_API_KEY_ID + DIGITALX_CLI_API_KEY_SECRET + DIGITALX_CLI_API_KEY_TYPE — mutually exclusive with --key/DIGITALX_CLI_KEY"},
	{Flag: "base-url", TakesValue: true, Desc: "override the REST base URL (also: DIGITALX_CLI_BASE_URL env var, baseUrl in config.json)"},
	{Flag: "ws-base-url", TakesValue: true, Desc: "override the WebSocket base URL for monitor (scheme ws/wss; also: DIGITALX_CLI_WS_BASE_URL env var, wsBaseUrl in config.json). Default: derived from the REST base URL. Also the value persisted by `key set-base-url`"},
	{Flag: "bind", TakesValue: true, Desc: "bind outbound connections (REST + WebSocket) to a single source IP or network interface, e.g. --bind 192.0.2.10 or --bind eth0 (also: DIGITALX_CLI_NET_BIND env var). A value that parses as an IP is a source address, otherwise an interface name; prefix with addr! or if! to force. Useful on multi-homed hosts to pick the link to use, or to spread calls across source IPs (public endpoints are rate-limited per IP). A source IP binds that one family; bind an interface to cover IPv4+IPv6 (true device binding — on Linux needs root/CAP_NET_RAW, otherwise restrict to one family with --family or bind a source IP)"},
	{Flag: "family", TakesValue: true, Desc: "outbound IP family: dualstack (default), ipv4, or ipv6 (also: DIGITALX_CLI_NET_IP_FAMILY env var). dualstack auto-selects; ipv4/ipv6 force that family for every connection"},
	{Flag: "timeout", TakesValue: true, Desc: "HTTP timeout in ms (default 15000)"},
	{Flag: "time-sync", TakesValue: true, Desc: "server-clock sync mode: auto (default), on, or off (also: DIGITALX_CLI_TIME_SYNC env var). auto signs with the local clock and resyncs reactively when the server rejects a correctable call with EXCEED_TIME_WINDOW (and, while streaming, when data arrives delayed against a still-unmeasured clock); on measures the server clock via /v2/time up front and signs every call with a corrected timestamp (fixes a wrong local clock; corrects the single send, never resends); off disables all clock correction (no proactive measure, no reactive resync) and always signs with the local clock"},
	{Flag: "retry-timeout", TakesValue: true, Desc: "total budget in ms for auto-retrying idempotent calls (network/5xx/429 backoff, EXCEED_TIME_WINDOW auto-correct); default 5000, 0 disables. Money-moving writes are never auto-retried"},
	{Flag: "no-reconcile", TakesValue: false, Desc: "order place only: send once and return the raw accept acknowledgement, skipping the clientOrderId reconcile + full-order fetch (still single-shot and journaled; an ambiguous failure is reported UNKNOWN, verify before retrying)"},
	{Flag: "dry-run", TakesValue: false, Desc: "print the request that would be sent (unsigned) and exit without sending it; for order place, also fetch public market data and emit advisory warnings for risky orders (slippage, far-from-market price, post-only/FOK that would not rest, tick/notional issues, an order with nothing to fill against, a check the book left unevaluated)"},
	{Flag: "json", TakesValue: false, Desc: "emit the machine-readable JSON result on stdout instead of the human-readable default"},
	{Flag: "compact", TakesValue: false, Desc: "single-line JSON output (implies --json)"},
	{Flag: "debug", TakesValue: false, Desc: "verbose diagnostics on stderr, and journal read calls too — which are otherwise excluded from the action journal, where only writes are journaled by default (also: DIGITALX_CLI_DEBUG env var)"},
	{Flag: "log-level", TakesValue: true, Desc: "operational-log level: trace, debug, info, warn, error, or off (also: DIGITALX_CLI_LOG_LEVEL env var). Defaults to error — a command's result and errors come through stdout/the error envelope, so warn-and-below logs are an opt-in diagnostic; turn them on to troubleshoot. debug explains each request (and an order's full place/reconcile trail); trace adds per-iteration detail (each send/lookup, each history/candles page). Sets the level only — separate from --debug, and takes precedence over it when both are set. Default: error (debug under --debug)"},
	{Flag: "log-file", TakesValue: true, Desc: "append operational logs to this file instead of stderr (also: DIGITALX_CLI_LOG_FILE env var). For the tui command this is the only way to capture diagnostics, since the full-screen UI owns the terminal. In the default text format, file lines are stamped with a local RFC3339 timestamp and drop the `korbit-cli: ` tag (a file trail wants a wall-clock anchor); --log-format json is unaffected by destination"},
	{Flag: "log-format", TakesValue: true, Desc: "operational-log format: text (default) or json (also: DIGITALX_CLI_LOG_FORMAT env var). text is one human line per record; json is one object per line (time, level, msg, then attributes) for log pipelines. The level vocabulary (trace/debug/info/warn/error) is identical in both"},
	{Flag: "lang", TakesValue: true, Desc: "display language for human-facing interactive UI chrome (labels, key hints, dialogs). Defaults to your OS locale, else English; an unsupported code is rejected with the list of supported ones. Market data, trading vocabulary, API messages, and all machine/JSON output stay English regardless"},
	{Flag: "no-fsync", TakesValue: false, Desc: "open the action journal and the monitor bot database with PRAGMA synchronous=OFF (no fsync) for faster writes; a power loss or OS crash may then lose or corrupt recent writes. The journal is a recreatable local log, so this only trades crash-durability for speed (also: DIGITALX_CLI_NO_FSYNC env var)"},
	{Flag: "enable-experimental", TakesValue: false, Desc: "opt into experimental, not-yet-stable features — currently the monitor command's JavaScript bot runtime (--where/--on/--init). Off by default; its API may change in future versions or have rough edges (also: DIGITALX_CLI_ENABLE_EXPERIMENTAL env var)"},
	// --version is declared here only so help/catalog list it; it is actually
	// implemented by cobra's Command.Version field (see cli/root.go), NOT by the
	// persistent-flag loop that binds the flags above. Keep both in sync.
	{Flag: "version", TakesValue: false, Desc: "print the CLI version"},
}

// GlobalEnvNote states the environment-variable naming rule behind every
// "(also: DIGITALX_CLI_… env var)" above, so a user whose shell profile or CI
// job predates the rename knows it still works. Rendered under the global
// options in the root help.
const GlobalEnvNote = "Environment: the legacy KORBIT_CLI_* spellings are still accepted; a non-empty DIGITALX_CLI_* name wins when both are set. " +
	"Setting a DIGITALX_CLI_* name to an empty value does not blank a set KORBIT_CLI_* one — unset the legacy name instead."

// Find resolves a command from positional path segments, preferring the deepest
// matching command (e.g. "sandbox runtime status" over "sandbox runtime" over
// "sandbox") so a nested leaf wins over a shorter prefix.
func Find(path []string) *Command {
	for n := len(path); n >= 1; n-- {
		for i := range Registry {
			c := &Registry[i]
			if len(c.ID) == n && idHasPrefix(c.ID, path[:n]) {
				return c
			}
		}
	}
	return nil
}

// Child is one direct child of a command group: either a leaf command or a
// nested subgroup (which has no Command entry of its own).
type Child struct {
	Name    string // the child's own id segment (last segment of its path)
	Summary string // a leaf's summary, or for a subgroup a list of its children
	IsGroup bool
}

// Children returns the direct children of the command group named by prefix
// (e.g. Children("sandbox") or Children("sandbox", "runtime")), in registry
// order: every leaf command one segment deeper, and every nested subgroup (a
// segment one deeper that only deeper commands share). A subgroup's Summary is
// the synthesized list of its own children so the parent's help stays
// self-describing without a separate group entry.
func Children(prefix ...string) []Child {
	var out []Child
	seen := map[string]bool{}
	for _, c := range Registry {
		if len(c.ID) <= len(prefix) || !idHasPrefix(c.ID, prefix) {
			continue
		}
		name := c.ID[len(prefix)]
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(c.ID) == len(prefix)+1 {
			out = append(out, Child{Name: name, Summary: c.Summary})
			continue
		}
		sub := Children(append(append([]string{}, prefix...), name)...)
		names := make([]string, len(sub))
		for i, s := range sub {
			names[i] = s.Name
		}
		out = append(out, Child{Name: name, Summary: "subcommands: " + strings.Join(names, ", "), IsGroup: true})
	}
	return out
}

// idHasPrefix reports whether id begins with every segment of prefix.
func idHasPrefix(id, prefix []string) bool {
	if len(id) < len(prefix) {
		return false
	}
	for i := range prefix {
		if id[i] != prefix[i] {
			return false
		}
	}
	return true
}

// Key returns the space-joined command key, e.g. "order place" or
// "sandbox runtime status".
func (c *Command) Key() string {
	return strings.Join(c.ID, " ")
}

func intPtr(n int) *int { return &n }
