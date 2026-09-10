// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/digitalx-official/digitalx-cli/internal/cmdmeta"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
	"github.com/digitalx-official/digitalx-cli/internal/spec"
	"github.com/digitalx-official/digitalx-cli/internal/version"
)

// progPlaceholder is the token spec strings (examples, notes, descriptions) use
// in place of the program name; render/catalog code substitutes the resolved
// name (progname.Name()) so the spec source itself never hardcodes the binary's
// name.
const progPlaceholder = "{prog}"

// withProg substitutes the resolved program name for progPlaceholder in s.
func withProg(s string) string { return strings.ReplaceAll(s, progPlaceholder, progname.Name()) }

// withProgAll applies withProg to each element, returning a fresh slice (nil
// stays nil so callers can normalize it to [] where the contract requires).
func withProgAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = withProg(s)
	}
	return out
}

// concatStrings returns a followed by b, or nil when both are empty (so a
// json:omitempty field stays omitted). Used to fold a command's experimental
// notes/examples in after its stable ones for the machine catalog and the MCP
// reference, which list the whole surface (only `--help` hides it).
func concatStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

var sectionOrder = []cmdmeta.Section{
	cmdmeta.SectionMarket, cmdmeta.SectionOrders, cmdmeta.SectionAccount, cmdmeta.SectionFunding, cmdmeta.SectionKeys, cmdmeta.SectionMeta,
}

var sectionTitles = map[cmdmeta.Section]string{
	cmdmeta.SectionMarket:  "Market data (public, no key needed)",
	cmdmeta.SectionOrders:  "Orders & fills (private)",
	cmdmeta.SectionAccount: "Account (private)",
	cmdmeta.SectionFunding: "Deposits & withdrawals (private)",
	cmdmeta.SectionKeys:    "Keys & setup",
	cmdmeta.SectionMeta:    "Meta",
}

// pad left-justifies s to width, measuring in runes (not bytes) so non-ASCII
// values in human tables still align.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n >= width {
		return s + "  "
	}
	return s + strings.Repeat(" ", width-utf8.RuneCountInString(s))
}

func positionalToken(p cmdmeta.Positional) string {
	if p.Variadic {
		return "[" + p.Name + "...]"
	}
	if p.Required {
		return "<" + p.Name + ">"
	}
	return "[" + p.Name + "]"
}

func flagValueToken(p cmdmeta.Param) string {
	if p.Kind == cmdmeta.KindFlag {
		return ""
	}
	if len(p.EnumValues) > 0 {
		return " <" + strings.Join(p.EnumValues, "|") + ">"
	}
	return " <" + string(p.Kind) + ">"
}

func usageFor(c surfaceCmd) string {
	parts := append([]string{progname.Name()}, c.ID...)
	for _, p := range c.Positionals {
		parts = append(parts, positionalToken(p))
	}
	hasOptional := false
	for _, p := range c.Params {
		if p.Required {
			parts = append(parts, "--"+p.Flag+flagValueToken(p))
		} else {
			hasOptional = true
		}
	}
	if hasOptional {
		parts = append(parts, "[options]")
	}
	return strings.Join(parts, " ")
}

func optionLine(p cmdmeta.Param) string {
	head := "--" + p.Flag + flagValueToken(p)
	desc := p.Desc
	// The "[experimental]" tag is rendered from the structural Experimental bit,
	// never stored in Desc — an experimental flag reaches this line only when help
	// was requested with --enable-experimental (RenderCommandHelp filters first).
	if p.Experimental {
		desc = "[experimental] " + desc
	}
	bits := []string{desc}
	if p.Required {
		bits = append(bits, "(required)")
	}
	if p.Min != nil || p.Max != nil {
		lo, hi := "", ""
		if p.Min != nil {
			lo = fmt.Sprint(*p.Min)
		}
		if p.Max != nil {
			hi = fmt.Sprint(*p.Max)
		}
		bits = append(bits, fmt.Sprintf("(range: %s..%s)", lo, hi))
	}
	if p.Default != "" {
		bits = append(bits, fmt.Sprintf("(default: %s)", p.Default))
	}
	return "  " + pad(head, 30) + strings.Join(bits, " ")
}

func authLabel(c surfaceCmd) string {
	if c.Auth == nil {
		return "public"
	}
	if c.Auth.Permission != "" {
		return "signed, requires " + c.Auth.Permission + " permission"
	}
	return "signed, any key"
}

// RenderCommandHelp renders per-command help from the unified surface. The
// program name in the usage line and examples comes from progname.Name().
//
// experimental gates the command's opt-in, not-yet-stable surface: when false
// (plain `--help`), experimental flags/notes/examples are omitted and a single
// pointer note tells the reader how to reveal them; when true (help requested
// with --enable-experimental), they are shown, the experimental notes/examples
// appended after the stable ones, and each experimental flag tagged.
func RenderCommandHelp(c surfaceCmd, experimental bool) string {
	prog := progname.Name()
	var l []string
	l = append(l, fmt.Sprintf("%s %s — %s", prog, strings.Join(c.ID, " "), c.Summary), "", "Usage:", "  "+usageFor(c))
	if c.IsEndpoint() {
		l = append(l, "", fmt.Sprintf("Endpoint: %s %s  (%s)", c.Method, c.Path, authLabel(c)))
	}
	if len(c.Positionals) > 0 {
		l = append(l, "", "Arguments:")
		for _, p := range c.Positionals {
			req := ""
			if p.Required {
				req = " (required)"
			}
			l = append(l, "  "+pad(positionalToken(p), 30)+p.Desc+req)
		}
	}
	// Options: hide experimental flags unless experimental help was requested.
	hiddenExperimental := false
	var params []cmdmeta.Param
	for _, p := range c.Params {
		if p.Experimental && !experimental {
			hiddenExperimental = true
			continue
		}
		params = append(params, p)
	}
	if len(params) > 0 {
		l = append(l, "", "Options:")
		for _, p := range params {
			l = append(l, optionLine(p))
		}
	}
	// Notes: stable always; experimental notes appended only when revealed. When
	// hiding experimental content, a single generated pointer note replaces it.
	notes := append([]string{}, c.Notes...)
	if experimental {
		notes = append(notes, c.ExperimentalNotes...)
	} else if hiddenExperimental || len(c.ExperimentalNotes) > 0 || len(c.ExperimentalExamples) > 0 {
		notes = append(notes, fmt.Sprintf("Experimental options are hidden — run `%s %s --enable-experimental --help` to show the experimental flags, notes, and examples.", prog, strings.Join(c.ID, " ")))
	}
	if len(notes) > 0 {
		l = append(l, "", "Notes:")
		for _, n := range notes {
			l = append(l, "  - "+n)
		}
	}
	examples := append([]string{}, c.Examples...)
	if experimental {
		examples = append(examples, c.ExperimentalExamples...)
	}
	if len(examples) > 0 {
		l = append(l, "", "Examples:")
		for _, e := range examples {
			l = append(l, "  $ "+e)
		}
	}
	l = append(l, "", fmt.Sprintf("Global options (--key, --dry-run, --json, --compact, ...): run `%s --help`.", prog))
	return withProg(strings.Join(l, "\n"))
}

// RenderGroupHelp renders focused help for a command group (e.g. `mcp`,
// `order`, `key`, or a nested group like `sandbox runtime`): the group's
// subcommands with their summaries, so `<prog> <group> --help` shows what is
// available instead of dumping the full root help. Falls back to root help if
// path is not a known group. The program name comes from progname.Name().
func RenderGroupHelp(path ...string) string {
	prog := progname.Name()
	subs := surfaceChildren(path...)
	if len(subs) == 0 {
		return RenderRootHelp()
	}
	group := strings.Join(path, " ")
	l := []string{
		fmt.Sprintf("%s %s — subcommands", prog, group),
		"",
		"Usage:",
		fmt.Sprintf("  %s %s <subcommand> [options]", prog, group),
		"",
		"Subcommands:",
	}
	for _, s := range subs {
		l = append(l, "  "+pad(s.Name, 30)+s.Summary)
	}
	l = append(l,
		"",
		fmt.Sprintf("Run `%s %s <subcommand> --help` for details.", prog, group),
		fmt.Sprintf("Global options (--key, --dry-run, --json, --compact, ...): run `%s --help`.", prog),
	)
	return withProg(strings.Join(l, "\n"))
}

// RenderRootHelp renders the top-level help from the spec. The program name
// comes from progname.Name().
func RenderRootHelp() string {
	prog := progname.Name()
	var l []string
	l = append(l, fmt.Sprintf("%s v%s — Digital X Open API v2 CLI", prog, version.Version))
	l = append(l, "", fmt.Sprintf("Usage: %s <command> [options]        (%s <command> --help for details)", prog, prog))

	surface := commandSurface()
	for _, section := range sectionOrder {
		var entries []surfaceCmd
		for _, c := range surface {
			if c.Section == section && !c.Hidden {
				entries = append(entries, c)
			}
		}
		if len(entries) == 0 {
			continue
		}
		l = append(l, "", sectionTitles[section]+":")
		for _, c := range entries {
			id := append([]string{}, c.ID...)
			for _, p := range c.Positionals {
				id = append(id, positionalToken(p))
			}
			l = append(l, "  "+pad(strings.Join(id, " "), 28)+c.Summary)
		}
	}

	l = append(l, "", "Global options:")
	for _, g := range spec.GlobalFlags {
		head := "--" + g.Flag
		if g.TakesValue {
			head += " <value>"
		}
		l = append(l, "  "+pad(head, 28)+g.Desc)
	}
	l = append(l, "", spec.GlobalEnvNote)

	l = append(l, "", "Output: stdout is human-readable by default; pass --json (or --compact) for the",
		`machine-readable JSON result. Diagnostics and structured errors ({"error": {...}},`,
		"always JSON) go to stderr.", "", "Exit codes:")
	codes := make([]string, 0, len(output.ExitCodes))
	for code := range output.ExitCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		l = append(l, "  "+code+"  "+output.ExitCodes[code])
	}
	l = append(l, "", "Proxy: outbound connections (REST + WebSocket) honor the standard",
		"HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables.")
	l = append(l, "", fmt.Sprintf("First time? Run `%s setup`. Machine-readable surface: `%s commands`.", prog, prog))
	l = append(l, fmt.Sprintf("License: Apache-2.0 — run `%s license` for the notice and terms.", prog))
	return withProg(strings.Join(l, "\n"))
}
