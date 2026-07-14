// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package cmdmeta

import "regexp"

// ValueKind is the type of a flag or positional value. It drives validation.
type ValueKind string

const (
	KindString   ValueKind = "string"
	KindInt      ValueKind = "int"
	KindDecimal  ValueKind = "decimal"
	KindEnum     ValueKind = "enum"
	KindSymbol   ValueKind = "symbol"
	KindSymbols  ValueKind = "symbols"
	KindCurrency ValueKind = "currency"
	KindCSV      ValueKind = "csv"
	KindMs       ValueKind = "ms"
	KindDuration ValueKind = "duration"
	KindFlag     ValueKind = "flag"
)

// Param is a single flag a command accepts.
type Param struct {
	// Flag is the CLI flag name without leading dashes (kebab-case).
	Flag string
	// API is the Korbit parameter name the value is sent as.
	API  string
	Kind ValueKind
	Desc string

	Required    bool
	EnumValues  []string
	Min         *int
	Max         *int
	Pattern     *regexp.Regexp
	PatternDesc string
	Default     string
	// Experimental marks a flag belonging to an opt-in, not-yet-stable feature
	// (gated behind --enable-experimental). Plain `--help` hides it; help shown
	// with --enable-experimental reveals it. The machine catalog always publishes
	// it (with this bit set) so agents can still discover it.
	Experimental bool
}

// Positional is a positional argument.
type Positional struct {
	Name     string
	API      string
	Kind     ValueKind
	Desc     string
	Required bool
	Variadic bool
}

// Section groups commands in help and the catalog.
type Section string

const (
	SectionMarket  Section = "market"
	SectionOrders  Section = "orders"
	SectionAccount Section = "account"
	SectionFunding Section = "funding"
	SectionKeys    Section = "keys"
	SectionMeta    Section = "meta"
)

// ResponseField is one machine-readable success-response field hint for an
// endpoint: top-level fields, and for an array response the element's fields.
type ResponseField struct {
	Name string
	Type string
	Desc string
}

// Auth describes the signing requirement of an endpoint. A nil *Auth means the
// endpoint is public. Permission == "" means signed with any key; a non-empty
// Permission names the API permission the key must hold.
type Auth struct {
	Permission string
}

// Safety classifies an operation's retry/idempotency posture, the basis for the
// call policy the operation layer derives.
type Safety string

const (
	// SafetyReadOnly is a read (GET): retry-safe, no side effect.
	SafetyReadOnly Safety = "readOnly"
	// SafetyIdempotent is a non-GET write that is safe to send more than once
	// with no extra effect (a cancel, or an address generate that returns the
	// existing address).
	SafetyIdempotent Safety = "idempotent"
	// SafetyNonIdempotent is a money-moving write with no natural idempotency: it
	// is never blindly resent.
	SafetyNonIdempotent Safety = "nonIdempotent"
)
