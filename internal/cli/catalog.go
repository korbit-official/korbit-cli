// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/korbit-official/korbit-cli/internal/cli/textout"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
	"github.com/korbit-official/korbit-cli/internal/spec"
	"github.com/korbit-official/korbit-cli/internal/version"
)

// The catalog is the `commands` output — a machine-readable description of the
// whole CLI surface, so an agent (or a Skill) can discover commands, flags,
// enums, and the output contract without scraping help text. It is built from
// the merged command surface: endpoint metadata comes from the ops catalog
// (OpMeta, cmdmeta types), builtin metadata from the spec registry.
//
// catalogVersion policy: additive changes (new commands, flags, fields) keep the
// version. Removing or renaming a top-level catalog field, or changing the
// meaning of `output`/`exitCodes`/`errorShapes`, bumps it. A consumer that
// branches on catalogVersion can treat a same-version catalog as backward-compatible.
//
// Scope: catalogVersion covers the catalog's own schema AND the CLI-OWNED success
// reshapes/contracts — the bare-ack normalization, the order-place full-order +
// clientOrderId echo, the paged-array merge, the truncation envelope, and the
// error envelope. It does NOT cover the upstream API's per-endpoint `data` field
// set, which the CLI passes through verbatim and cannot version (a renamed API
// field flows straight to stdout). See the "JSON stability model" invariant in
// AGENTS.md. So bump catalogVersion when a CLI-owned shape changes, not when the
// API's own response fields change.

type catalog struct {
	Name           string            `json:"name"`
	Version        string            `json:"version"`
	CatalogVersion int               `json:"catalogVersion"`
	Output         catOutput         `json:"output"`
	ExitCodes      map[string]string `json:"exitCodes"`
	RetryGuidance  map[string]string `json:"retryGuidance"`
	ErrorShapes    catErrorShapes    `json:"errorShapes"`
	GlobalFlags    []catGlobalFlag   `json:"globalFlags"`
	Commands       []catCommand      `json:"commands"`
}

// FormatText renders the catalog as a grouped command listing for a human
// (textout.TextFormatter); the full catalog (flags, enums, response hints, the
// output contract) is available with --json, which marshals this struct.
func (c catalog) FormatText(w io.Writer) {
	fmt.Fprintf(w, "%s %s — %d commands\n", c.Name, c.Version, len(c.Commands))
	var order []string
	groups := map[string]*[][2]string{}
	for _, cmd := range c.Commands {
		sec := string(cmd.Section)
		g, exists := groups[sec]
		if !exists {
			rows := &[][2]string{}
			groups[sec] = rows
			g = rows
			order = append(order, sec)
		}
		*g = append(*g, [2]string{cmd.Command, cmd.Summary})
	}
	for _, sec := range order {
		rows := *groups[sec]
		width := 0
		for _, r := range rows {
			if n := utf8.RuneCountInString(r[0]); n > width {
				width = n
			}
		}
		fmt.Fprint(w, "\n")
		if sec != "" {
			fmt.Fprint(w, sec+"\n")
		}
		for _, r := range rows {
			fmt.Fprintf(w, "  %s  %s\n", textout.PadRune(r[0], width), r[1])
		}
	}
	fmt.Fprintf(w, "\n(run `%s commands --json` for the full machine-readable catalog)", progname.Name())
}

type catOutput struct {
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr"`
	Success string `json:"success"`
}

// catErrorShapes documents the two structured-error shapes emitted on stderr, so
// an agent can parse failures without scraping prose.
type catErrorShapes struct {
	Usage string `json:"usage"`
	API   string `json:"api"`
}

// retryGuidance maps each exit code to machine-actionable retry advice. Note the
// CLI already auto-retries IDEMPOTENT calls (reads, cancels, deposit-address
// generate) within --retry-timeout (default 5s): network/5xx/429 (and the
// cancel-only TRY_AGAIN "mid-processing" condition) with backoff, and
// EXCEED_TIME_WINDOW with an automatic clock re-sync. So a non-zero exit on
// an idempotent call means that budget was already exhausted. Money-moving
// writes are never blindly resent on an AMBIGUOUS (network/5xx) failure — handle
// that yourself, idempotently. The one exception is the pre-execution
// EXCEED_TIME_WINDOW class on order place: it provably did not land, so the place
// protocol re-syncs the clock once and resends the SAME clientOrderId. The
// strictly single-shot writes (withdraw request, krw deposit/withdraw request) have no
// idempotency key and are NOT auto-corrected — pass --time-sync on there.
// exitCodes returns output.ExitCodes with the {prog} placeholder resolved to the
// running program name.
func exitCodes() map[string]string {
	out := make(map[string]string, len(output.ExitCodes))
	for k, v := range output.ExitCodes {
		out[k] = withProg(v)
	}
	return out
}

func retryGuidance() map[string]string {
	return map[string]string{
		strconv.Itoa(output.ExitSuccess):  "success",
		strconv.Itoa(output.ExitInternal): "transient (network/internal); idempotent calls were already retried within --retry-timeout — raise it, or for a write reconcile state before resending",
		strconv.Itoa(output.ExitUsage):    "do not retry — fix the invocation",
		strconv.Itoa(output.ExitAPI):      "do not blindly retry — inspect error.code; on HTTP 429 honor error.retryAfterSec; on EXCEED_TIME_WINDOW order place auto-resyncs the clock once and resends the same clientOrderId, but the strictly single-shot writes (withdraw/krw) are not auto-corrected — pass --time-sync on to sign with a corrected clock before the send",
		strconv.Itoa(output.ExitConfig):   fmt.Sprintf("do not retry — fix the key/config (%s key ... or config.json)", progname.Name()),
	}
}

type catGlobalFlag struct {
	Flag       string `json:"flag"`
	TakesValue bool   `json:"takesValue"`
	Desc       string `json:"desc"`
}

type catCommand struct {
	Command     string          `json:"command"`
	Usage       string          `json:"usage"`
	Section     cmdmeta.Section `json:"section"`
	Summary     string          `json:"summary"`
	Endpoint    *catEndpoint    `json:"endpoint,omitempty"`
	Positionals []catPositional `json:"positionals,omitempty"`
	Flags       []catFlag       `json:"flags"`
	Notes       []string        `json:"notes,omitempty"`
	// ResponseFields are concise success-response field HINTS for endpoint
	// commands (top-level fields; for array responses, the element's fields).
	// Builtins (no API call) omit it. Sourced from the operation's
	// OpMeta.Response in the ops catalog. They are
	// advisory, NOT a guaranteed schema: the spec shares response fields across
	// endpoints via YAML anchors, so a hint can name a field that lives on a
	// sibling endpoint, and the success JSON is the API's own payload passed
	// through verbatim (see the JSON stability model in AGENTS.md). Treat them as
	// a discovery aid, not a contract.
	ResponseFields []catResponseField `json:"responseFields,omitempty"`
	Examples       []string           `json:"examples"`
}

type catResponseField struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Desc string `json:"desc"`
}

type catEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
}

type catPositional struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic"`
	Desc     string `json:"desc"`
}

type catFlag struct {
	Flag     string   `json:"flag"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Enum     []string `json:"enum,omitempty"`
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Default  string   `json:"default,omitempty"`
	// Experimental marks a flag behind --enable-experimental. Plain `--help`
	// hides such flags; the catalog always lists them with this bit set so an
	// agent can still discover and opt into them.
	Experimental bool   `json:"experimental,omitempty"`
	Desc         string `json:"desc"`
}

func buildCatalog() catalog {
	c := catalog{
		Name:           progname.Name(),
		Version:        version.Version,
		CatalogVersion: 3,
		Output: catOutput{
			Stdout:  "by default, human-readable text (tables / key-value); pass --json (or --compact) for one machine-readable JSON document: the command result (the API data payload; bare acks normalize to {\"success\": true})",
			Stderr:  `diagnostics, and on failure the error: by default a plain "error: <message>" line, or under --json the structured object {"error": {"type", "message", ...}}. Exit code is identical in both modes`,
			Success: `with --json: {"success": true, "data": ...} unwrapped to its data; a null/absent data becomes {"success": true}. A truncated paged result (order history/fills/candles/funding histories) is wrapped as {"data": [...], "truncated": true, "note": string} instead of a bare array — narrow the window`,
		},
		ExitCodes:     exitCodes(),
		RetryGuidance: retryGuidance(),
		ErrorShapes: catErrorShapes{
			Usage: `{"error":{"type":"usage"|"config"|"internal","message":string}}`,
			API:   `{"error":{"type":"api","message":string,"httpStatus":number,"code":string|null,"retryAfterSec"?:number,"body"?:any}}`,
		},
	}
	for _, g := range spec.GlobalFlags {
		c.GlobalFlags = append(c.GlobalFlags, catGlobalFlag{Flag: "--" + g.Flag, TakesValue: g.TakesValue, Desc: withProg(g.Desc)})
	}
	for _, e := range commandSurface() {
		if e.Hidden {
			continue // a hidden command (self install) is not an agent operation
		}
		cmd := catCommand{
			Command: e.Key(),
			Usage:   usageFor(e),
			Section: e.Section,
			Summary: withProg(e.Summary),
			// The catalog stays complete: experimental notes/examples are listed
			// after the stable ones (only `--help` hides the experimental surface).
			Notes: withProgAll(concatStrings(e.Notes, e.ExperimentalNotes)),
		}
		// Examples carry a {prog} placeholder (the surface stores no program name);
		// substitute the resolved name so the catalog shows runnable command
		// lines. Always emit a JSON array (never null) so machine consumers see a
		// consistent type — mirrors the Flags normalization below.
		cmd.Examples = withProgAll(concatStrings(e.Examples, e.ExperimentalExamples))
		if cmd.Examples == nil {
			cmd.Examples = []string{}
		}
		if e.IsEndpoint() {
			cmd.Endpoint = &catEndpoint{Method: e.Method, Path: e.Path, Auth: catAuth(e.Auth)}
		}
		for _, p := range e.Positionals {
			cmd.Positionals = append(cmd.Positionals, catPositional{
				Name: p.Name, Type: string(p.Kind), Required: p.Required, Variadic: p.Variadic, Desc: withProg(p.Desc),
			})
		}
		cmd.Flags = []catFlag{}
		for _, p := range e.Params {
			f := catFlag{Flag: "--" + p.Flag, Type: string(p.Kind), Required: p.Required, Enum: p.EnumValues, Min: p.Min, Max: p.Max, Default: p.Default, Experimental: p.Experimental, Desc: withProg(p.Desc)}
			if p.Pattern != nil {
				f.Pattern = p.Pattern.String()
			}
			cmd.Flags = append(cmd.Flags, f)
		}
		for _, r := range e.Response {
			cmd.ResponseFields = append(cmd.ResponseFields, catResponseField{Name: r.Name, Type: r.Type, Desc: r.Desc})
		}
		c.Commands = append(c.Commands, cmd)
	}
	return c
}

func catAuth(a *cmdmeta.Auth) string {
	if a == nil {
		return "public"
	}
	if a.Permission != "" {
		return a.Permission
	}
	return "signed"
}
