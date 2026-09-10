// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/apiclient"
	"github.com/digitalx-official/digitalx-cli/internal/cmdmeta"
)

// OpMeta is an operation's presentation + validation metadata; the catalog is
// the set of OpMetas.
type OpMeta struct {
	ID          []string // command path segments, e.g. {"order","place"}
	Method      string   // REST method the operation maps to: GET | POST | DELETE
	Path        string   // REST path the operation maps to, e.g. /v2/orders
	Section     cmdmeta.Section
	Summary     string
	Params      []cmdmeta.Param
	Positionals []cmdmeta.Positional
	Notes       []string
	Examples    []string
	// ExperimentalNotes / ExperimentalExamples document an opt-in, not-yet-stable
	// feature; plain `--help` hides them and --enable-experimental reveals them
	// (the machine catalog always includes them). No endpoint operation is
	// experimental today — the fields exist so the unified surface stays symmetric
	// with the builtin spec.
	ExperimentalNotes    []string
	ExperimentalExamples []string
	Response             []cmdmeta.ResponseField
	Auth                 *cmdmeta.Auth // nil = public
	Safety               cmdmeta.Safety
	Destructive          bool // MCP destructive hint (money-movers + cancels)
	// CrossValidate applies cross-field rules after per-value normalization; nil
	// = none. values is keyed by API parameter name (post-normalization).
	CrossValidate func(values map[string]string) error
}

// Key returns the space-joined command key, e.g. "order place".
func (m OpMeta) Key() string { return strings.Join(m.ID, " ") }

// RunInput is the validated, normalized input a frontend hands an operation.
type RunInput struct {
	Values   map[string]string // validated values keyed by API name
	Controls Controls
	// KeyName / APIKeyID identify the signing key for the operations ledger (the
	// public api-key id; never a secret). "" for a public operation. They feed
	// only the journal — signing credentials live on the wire client behind Raw.
	KeyName  string
	APIKeyID string
}

// Controls carries per-invocation behavior toggles a frontend selects.
type Controls struct {
	SkipReconcile bool   // honored only by the place operation; ignored elsewhere
	Surface       string // the frontend identity for the operations ledger: cli|monitor|mcp
}

// Operation is one catalog entry: its metadata and its behavior.
type Operation interface {
	Meta() OpMeta
	Run(ctx context.Context, a *API, in RunInput) (Result, error)
}

// policyFor derives the L1 call policy from an operation's Safety: a read or
// idempotent write opts into the bounded retry ladder within the API's budget; a
// money-moving write is single-shot (the zero Policy).
func policyFor(safety cmdmeta.Safety, a *API) apiclient.Policy {
	switch safety {
	case cmdmeta.SafetyReadOnly, cmdmeta.SafetyIdempotent:
		return apiclient.Policy{Idempotent: true, BudgetMs: a.RetryBudgetMs}
	default:
		return apiclient.Policy{}
	}
}
