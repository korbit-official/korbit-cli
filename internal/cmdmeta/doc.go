// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Package cmdmeta is the command-surface vocabulary AND the per-value validation
// engine over it, shared by the operation catalog and the command descriptors:
// value kinds, the flag and positional shapes, sections, response-field hints,
// auth, the safety classification, and NormalizeValue/CoerceValue (validate one
// value against its kind). Its only internal dependency is internal/output (the
// error taxonomy, a stdlib-only leaf), so any layer can describe AND validate a
// command surface without pulling in a routing or execution dependency. The
// cross-field rules that span several params of one operation live with the
// operations that own them (internal/ops), not here.
package cmdmeta
