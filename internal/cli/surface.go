// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"

	"github.com/korbit-official/korbit-cli/internal/cmdmeta"
	"github.com/korbit-official/korbit-cli/internal/ops"
	"github.com/korbit-official/korbit-cli/internal/spec"
)

// surfaceCmd is the unified view of one command the cobra tree, help, and the
// catalog iterate. It merges the two command sources into one field shape:
//   - ENDPOINT commands come from the ops catalog (op != nil, Builtin false);
//     their metadata is the operation's OpMeta and they execute via op.Run.
//   - BUILTIN commands come from spec.Registry (cmd != nil, Builtin true); their
//     metadata is the spec.Command and they dispatch through the existing
//     builtin handlers.
//
// Both sources speak the cmdmeta vocabulary (Param/Positional/Section/…), so the
// surface carries those types throughout and copies them straight across. The
// frontends never branch on the source again — they read the surface.
type surfaceCmd struct {
	ID          []string
	Section     cmdmeta.Section
	Summary     string
	Params      []cmdmeta.Param
	Positionals []cmdmeta.Positional
	Notes       []string
	Examples    []string
	// ExperimentalNotes / ExperimentalExamples document the command's opt-in,
	// not-yet-stable surface. Plain `--help` hides them; --enable-experimental
	// reveals them. The catalog and the MCP bot_runtime_reference always include
	// them.
	ExperimentalNotes    []string
	ExperimentalExamples []string
	Response             []cmdmeta.ResponseField
	Auth                 *cmdmeta.Auth // nil = public (always nil for builtins)
	// Method/Path describe the REST endpoint a command maps to; empty for
	// builtins (which make no API call).
	Method string
	Path   string
	// Builtin distinguishes a spec builtin (true) from an ops endpoint (false).
	Builtin bool
	// Hidden keeps a builtin out of the user-facing surfaces (root help, group
	// child lists, the catalog) while staying dispatchable. Always false for
	// endpoints. See spec.Command.Hidden.
	Hidden bool
	// op is the operation for an endpoint; nil for a builtin.
	op ops.Operation
	// cmd is the spec command for a builtin; nil for an endpoint.
	cmd *spec.Command
}

// Key returns the space-joined command key, e.g. "order place".
func (s surfaceCmd) Key() string { return strings.Join(s.ID, " ") }

// IsEndpoint reports whether this command maps to a REST endpoint.
func (s surfaceCmd) IsEndpoint() bool { return !s.Builtin }

// commandSurface merges the ops catalog (endpoints) and the spec builtins into
// one ordered list: every endpoint operation first (catalog/registration order),
// then every builtin (registry order). The split is intentional and stable so
// help/catalog ordering is deterministic; section ordering, not source ordering,
// drives the grouped help.
func commandSurface() []surfaceCmd {
	var out []surfaceCmd
	for _, op := range ops.Catalog() {
		m := op.Meta()
		out = append(out, surfaceCmd{
			ID:                   m.ID,
			Section:              m.Section,
			Summary:              m.Summary,
			Params:               m.Params,
			Positionals:          m.Positionals,
			Notes:                m.Notes,
			Examples:             m.Examples,
			ExperimentalNotes:    m.ExperimentalNotes,
			ExperimentalExamples: m.ExperimentalExamples,
			Response:             m.Response,
			Auth:                 m.Auth,
			Method:               m.Method,
			Path:                 m.Path,
			Builtin:              false,
			op:                   op,
		})
	}
	for i := range spec.Registry {
		c := &spec.Registry[i]
		out = append(out, surfaceCmd{
			ID:                   c.ID,
			Section:              c.Section,
			Summary:              c.Summary,
			Params:               c.Params,
			Positionals:          c.Positionals,
			Notes:                c.Notes,
			Examples:             c.Examples,
			ExperimentalNotes:    c.ExperimentalNotes,
			ExperimentalExamples: c.ExperimentalExamples,
			Builtin:              true,
			Hidden:               c.Hidden,
			cmd:                  c,
		})
	}
	return out
}

// findSurface resolves a surface command from command-path segments, preferring
// the deepest match (mirrors spec.Find / ops.Find) so a nested leaf wins over a
// shorter prefix. nil when nothing matches.
func findSurface(path []string) *surfaceCmd {
	all := commandSurface()
	for n := len(path); n >= 1; n-- {
		for i := range all {
			if len(all[i].ID) == n && idHasPrefix(all[i].ID, path[:n]) {
				return &all[i]
			}
		}
	}
	return nil
}

// surfaceChild is one direct child of a command group: either a leaf command or
// a nested subgroup (which has no surface entry of its own).
type surfaceChild struct {
	Name    string
	Summary string
	IsGroup bool
}

// surfaceChildren returns the direct children of the command group named by
// prefix, in surface order: every leaf one segment deeper, and every nested
// subgroup. A subgroup's Summary is the synthesized list of its own children so
// a parent's help stays self-describing without a separate group entry.
func surfaceChildren(prefix ...string) []surfaceChild {
	all := commandSurface()
	var out []surfaceChild
	seen := map[string]bool{}
	for i := range all {
		if all[i].Hidden {
			continue // a hidden command is not listed as a child of its group
		}
		id := all[i].ID
		if len(id) <= len(prefix) || !idHasPrefix(id, prefix) {
			continue
		}
		name := id[len(prefix)]
		if seen[name] {
			continue
		}
		seen[name] = true
		if len(id) == len(prefix)+1 {
			out = append(out, surfaceChild{Name: name, Summary: all[i].Summary})
			continue
		}
		sub := surfaceChildren(append(append([]string{}, prefix...), name)...)
		names := make([]string, len(sub))
		for j, s := range sub {
			names[j] = s.Name
		}
		out = append(out, surfaceChild{Name: name, Summary: "subcommands: " + strings.Join(names, ", "), IsGroup: true})
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
