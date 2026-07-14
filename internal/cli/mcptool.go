// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the MCP equivalent of catalog.go (surface -> JSON-Schema tool
// definitions) and the validation half of botapi/bindings.go (surface ->
// argument parsing). The MCP tool surface is GENERATED from the ops catalog
// (endpoint operations only), so it can never drift from the command surface:
// one new endpoint operation becomes one new tool, validated by the same ops
// validation engine (NormalizeValue / CrossValidate) the CLI and the bot runtime
// use, and executed through the same operation.

// mcpKeyArg is the option name carrying the per-call key selector in
// --multi-key mode. It is NOT a wire parameter — it is stripped before the call
// is built — so it can never collide with a real param (no /v2 endpoint takes a
// "key" parameter; the catalog guard would surface it if one ever did).
const mcpKeyArg = "key"

// mcpDryRunArg is the synthetic boolean exposed ONLY on the order_place tool: a
// preview that simulates the order against the live public orderbook and returns
// the estimated fill + risk warnings instead of placing it. It is NOT a wire
// parameter (stripped before the call is built), and it is added to no other
// tool's schema on purpose — only a placement is worth previewing.
const mcpDryRunArg = "dryRun"

// korbitGuideName is the read-only tool that returns the bundled Korbit Agent
// Skill's workflow guidance — the safety rules and the recommended workflow for
// each task. An MCP-only client (e.g. Claude Desktop via the .mcpb Desktop
// Extension) never loads the skill the way Claude Code does, so this tool is how
// it reaches the same guidance. The text is read from the embedded skill, so it
// cannot drift from what `agent skill install` writes to disk.
const korbitGuideName = "korbit_guide"

// mcpGuideTopicArg selects a focused playbook for korbit_guide; omitted, the
// tool returns the overview and task router (SKILL.md).
const mcpGuideTopicArg = "topic"

// korbitGuideDesc renders the korbit_guide description, naming the focused
// topics discovered in the skill so the model can target one directly.
func korbitGuideDesc(topics []string) string {
	var b strings.Builder
	b.WriteString("Korbit workflow guidance from the bundled agent skill: the safety rules (dry-run-first order placement, idempotency, decimal-string money) and the recommended workflow for each task. Call with no `topic` for the overview and task router; pass a `topic` for a focused playbook. Read this before placing an order or driving an unfamiliar flow. Read-only; returns documentation text and makes no API call.")
	if len(topics) > 0 {
		b.WriteString(" Topics: " + strings.Join(topics, ", ") + ".")
	}
	return b.String()
}

// korbitGuideSchema is the input schema for korbit_guide: an optional `topic`
// constrained to the discovered playbook names. With no topics (no skill
// embedded) the topic argument is omitted entirely.
func korbitGuideSchema(topics []string) json.RawMessage {
	if len(topics) == 0 {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
	enum, _ := json.Marshal(topics)
	return json.RawMessage(fmt.Sprintf(
		`{"type":"object","properties":{%q:{"type":"string","enum":%s,"description":"focused playbook to return; omit for the overview and task router"}},"additionalProperties":false}`,
		mcpGuideTopicArg, enum))
}

// toolName renders the MCP tool name for a command: the command key with spaces
// replaced by underscores (order place -> order_place, krw deposit history ->
// krw_deposit_history). Command keys are unique, so tool names are too. This is a stable
// agent contract — extend additively.
func toolName(c surfaceCmd) string {
	return strings.Join(c.ID, "_")
}

// toolDescription is the model-facing description: the one-line summary, the
// method/path/auth line, and the command's notes. The model selects tools by
// name + description, so this is written for it.
func toolDescription(c surfaceCmd) string {
	var b strings.Builder
	b.WriteString(withProg(c.Summary))
	if c.IsEndpoint() {
		b.WriteString(fmt.Sprintf("\n\n%s %s · %s", c.Method, c.Path, authLabel(c)))
	}
	for _, n := range c.Notes {
		b.WriteString("\n\n")
		b.WriteString(withProg(n))
	}
	return b.String()
}

// botRuntimeReferenceName is the read-only doc tool that describes the CLI's bot
// runtime (the monitor command's JavaScript hooks, the korbit.*/db.* API, and the
// ta indicator library). An MCP client drives REST endpoints as tools but cannot
// run a streaming bot itself, so this tool only tells a capable agent the runtime
// exists and how to invoke it from a shell.
const botRuntimeReferenceName = "bot_runtime_reference"

const botRuntimeReferenceDesc = "Reference for the korbit-cli bot runtime: the `monitor` command's --init/--where/--on JavaScript hooks, the async korbit.*/db.* API, and the synchronous `ta` technical-indicator library. This MCP server exposes REST endpoints as tools but cannot run a streaming bot — use the CLI (`" + progPlaceholder + " monitor --enable-experimental ...`) to run one. The bot runtime is EXPERIMENTAL and off by default (hence the --enable-experimental flag), and its API may change. Read-only; returns documentation text and makes no API call."

// botRuntimeReferenceText renders the monitor command's surface entry — summary,
// flags, notes, examples — into model-facing markdown. It is sourced entirely
// from the unified surface (the same data behind `monitor --help`), so the
// ta/korbit/db reference can never drift from the command surface.
func botRuntimeReferenceText() string {
	c := findSurface([]string{"monitor"})
	if c == nil {
		return "the monitor bot runtime is unavailable in this build"
	}
	var b strings.Builder
	b.WriteString("# korbit-cli bot runtime (`" + progPlaceholder + " monitor`)\n\n")
	b.WriteString(c.Summary)
	b.WriteString("\n\nRun it from a shell — this MCP server cannot stream a bot. The JavaScript hooks (--init/--where/--on) share one runtime with the full async korbit.*/db.* API and the synchronous ta/ta.stream indicator library.\n")
	if len(c.Params) > 0 {
		b.WriteString("\n## Flags\n")
		for _, p := range c.Params {
			b.WriteString("- `--" + p.Flag + "`: " + p.Desc + "\n")
		}
	}
	// The bot runtime IS the experimental surface, so the reference lists the
	// experimental notes/examples alongside the stable ones (unlike `--help`,
	// which hides them until --enable-experimental).
	if notes := concatStrings(c.Notes, c.ExperimentalNotes); len(notes) > 0 {
		b.WriteString("\n## Notes\n")
		for _, n := range notes {
			b.WriteString("- " + n + "\n")
		}
	}
	if examples := concatStrings(c.Examples, c.ExperimentalExamples); len(examples) > 0 {
		b.WriteString("\n## Examples\n")
		for _, ex := range examples {
			b.WriteString("- `" + ex + "`\n")
		}
	}
	return withProg(b.String())
}

// toolAnnotations advertise a tool's behavior to the host (hints only). GET
// commands are read-only; the operation's Safety drives the idempotent hint
// (a read or an idempotent write is idempotent); the operation's Destructive
// flag flags the money-movers/cancels.
func toolAnnotations(c surfaceCmd) *mcp.ToolAnnotations {
	a := &mcp.ToolAnnotations{Title: c.Summary}
	if c.Method == "GET" {
		a.ReadOnlyHint = true
		// DestructiveHint is meaningful only when ReadOnlyHint is false; leave nil.
		return a
	}
	m := c.op.Meta()
	a.IdempotentHint = m.Safety != cmdmeta.SafetyNonIdempotent
	d := m.Destructive
	a.DestructiveHint = &d
	return a
}

// inputSchema builds the tool's JSON Schema (draft 2020-12) from the command's
// positionals and params, keyed on wire (API) names — the same key space the
// bot API uses. When multiKey is set, authenticated tools also accept an
// optional `key` naming one of keyNames (default = the launch key).
//
// NOTE: this schema is advisory to the host/model. The raw mcp.Server.AddTool
// path does NOT validate arguments against it (only the generic AddTool[In,Out]
// would) — so the tool handler is the SOLE enforcement point: parseToolArgs +
// jsonRawToString + cmdmeta.NormalizeValue / CrossValidate re-check everything.
// Do not remove a handler check on the assumption the schema enforces it.
func inputSchema(c surfaceCmd, multiKey bool, keyNames []string) json.RawMessage {
	props := map[string]any{}
	var required []string
	seen := map[string]bool{}

	add := func(api string, p cmdmeta.Param, isRequired bool) {
		if seen[api] {
			return
		}
		seen[api] = true
		props[api] = propSchema(p)
		if isRequired {
			required = append(required, api)
		}
	}
	for _, ps := range c.Positionals {
		add(ps.API, cmdmeta.Param{API: ps.API, Kind: ps.Kind, Desc: ps.Desc}, ps.Required)
	}
	for _, p := range c.Params {
		add(p.API, p, p.Required)
	}
	if multiKey && c.Auth != nil {
		// A nil slice would marshal to "enum":null (invalid JSON Schema); use an
		// empty array when no keys are configured yet.
		enum := keyNames
		if enum == nil {
			enum = []string{}
		}
		props[mcpKeyArg] = map[string]any{
			"type":        "string",
			"enum":        enum,
			"description": "which configured key/account to sign with; defaults to the server's launch key",
		}
	}
	if c.Key() == cmdPlace {
		// Preview-only flag on order_place (see mcpDryRunArg).
		props[mcpDryRunArg] = map[string]any{
			"type":        "boolean",
			"description": "preview only: simulate this order against the live public orderbook and return the estimated fill plus risk warnings WITHOUT placing it (nothing is signed or sent to your account). Use it to sanity-check sizing and market impact before a real placement.",
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return b
}

// propSchema maps one param to its JSON Schema property. The ValueKind ->
// schema mapping mirrors the type/enum/min/max/pattern fields catalog.go emits;
// money/quantity (decimal) and the CSV kinds stay strings on the wire, with the
// constraint spelled out in the description (the handler enforces it too).
func propSchema(p cmdmeta.Param) map[string]any {
	prop := map[string]any{}
	desc := withProg(p.Desc)
	switch p.Kind {
	case cmdmeta.KindInt:
		prop["type"] = "integer"
		if p.Min != nil {
			prop["minimum"] = *p.Min
		}
		if p.Max != nil {
			prop["maximum"] = *p.Max
		}
	case cmdmeta.KindMs:
		prop["type"] = "integer"
		desc = appendNote(desc, "unix timestamp in milliseconds")
	case cmdmeta.KindDuration:
		prop["type"] = "string"
		desc = appendNote(desc, `duration like "90s" — units: ms, s, m, h`)
	case cmdmeta.KindEnum:
		prop["type"] = "string"
		prop["enum"] = p.EnumValues
	case cmdmeta.KindFlag:
		prop["type"] = "boolean"
	case cmdmeta.KindDecimal:
		prop["type"] = "string"
		desc = appendNote(desc, `decimal string like "0.001" — never a JSON number`)
	case cmdmeta.KindSymbols, cmdmeta.KindCSV:
		prop["type"] = "string"
		desc = appendNote(desc, "comma-separated")
	default: // KindString, KindSymbol, KindCurrency
		prop["type"] = "string"
		if p.Pattern != nil {
			prop["pattern"] = p.Pattern.String()
		}
	}
	if p.Default != "" {
		desc = appendNote(desc, "default: "+p.Default)
	}
	if desc != "" {
		prop["description"] = desc
	}
	return prop
}

func appendNote(desc, note string) string {
	if desc == "" {
		return note
	}
	return desc + " (" + note + ")"
}

// parseToolArgs maps a decoded tool-call arguments object onto the command's
// params, mirroring collectParams (CLI) and botapi.parseArgs (bot) but for JSON
// values. args is keyed on wire (API) names; params is keyed by API name for the
// cross-field rules. Unknown keys are rejected, defaults applied, and required
// fields checked — identical semantics to the other two frontends. CrossValidate
// is the caller's job (after any key-arg stripping).
func parseToolArgs(c surfaceCmd, args map[string]json.RawMessage, acctSeqDefault string) (map[string]string, error) {
	valid := map[string]cmdmeta.Param{}
	for _, p := range c.Params {
		valid[p.API] = p
	}
	for _, ps := range c.Positionals {
		if _, ok := valid[ps.API]; !ok {
			valid[ps.API] = cmdmeta.Param{API: ps.API, Kind: ps.Kind}
		}
	}

	params := map[string]string{}
	for k, raw := range args {
		target, ok := valid[k]
		if !ok {
			return nil, output.Usagef("unknown argument %q — valid: %s", k, validKeyList(c))
		}
		rawStr, present, err := jsonRawToString(target, raw, k)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		v, err := cmdmeta.NormalizeValue(target, rawStr, k)
		if err != nil {
			return nil, err
		}
		params[k] = v
	}

	// Apply defaults and enforce required positionals/params.
	for _, ps := range c.Positionals {
		if _, ok := params[ps.API]; !ok && ps.Required {
			return nil, output.Usagef("%s is required", ps.API)
		}
	}
	for _, p := range c.Params {
		if _, ok := params[p.API]; !ok {
			if p.Default != "" {
				v, err := cmdmeta.NormalizeValue(p, p.Default, p.API)
				if err != nil {
					return nil, err
				}
				params[p.API] = v
			} else if p.Required {
				return nil, output.Usagef("%s is required", p.API)
			}
		}
	}
	if _, err := accountseq.Ensure(c.Params, params, acctSeqDefault); err != nil {
		return nil, err
	}
	return params, nil
}

// validKeyList lists a command's accepted argument names (wire/API names) for
// an unknown-argument error.
func validKeyList(c surfaceCmd) string {
	var b bytes.Buffer
	seen := map[string]bool{}
	add := func(api string) {
		if seen[api] {
			return
		}
		seen[api] = true
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(api)
	}
	for _, ps := range c.Positionals {
		add(ps.API)
	}
	for _, p := range c.Params {
		add(p.API)
	}
	return b.String()
}

// jsonRawToString converts one decoded JSON argument value to the raw string
// NormalizeValue expects. The JSON-specific empty/null "omitted" markers are
// handled here; the per-kind type rules (incl. the money-must-be-a-string
// guard — a decimal given a JSON number is rejected before anything is sent)
// live in the shared cmdmeta.CoerceValue, so the MCP server and the bot API can
// never diverge on them.
func jsonRawToString(p cmdmeta.Param, raw json.RawMessage, label string) (string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", false, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, output.Usagef("%s: invalid JSON value", label)
	}
	return cmdmeta.CoerceValue(p, v, label)
}
