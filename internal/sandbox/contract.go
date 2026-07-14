// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

// This file centralizes the facts the CLI consumes from the sandbox bundle's
// interface — pidfile shape, listening-line format, stderr prefixes, and status
// JSON. Keeping them in one place means a change to the bundle's contract is
// reconciled here, not chased through the package.
//
// The bundle exits with distinct codes per failure (precheck, db-not-initialized,
// already-running, schema-mismatch, …), but the manager spawns it detached and
// classifies a startup failure from the run.log tail via the stable stderr
// prefixes below — which work even when the detached child's exit status isn't
// observable — so the CLI does not mirror the numeric codes here. The sandbox's
// own exitcodes module stays the single source of truth for the numbers.

// Stable stderr prefixes the bundle prints. The CLI scans run.log for them to
// classify a startup failure and surface a precise message.
const (
	// PrecheckPrefix prefixes a fatal runtime-precheck line (it carries the exact
	// upgrade command, surfaced verbatim rather than second-guessed by the CLI).
	PrecheckPrefix = "SANDBOX_PRECHECK fatal:"
	// NotInitializedSubstr appears in the "db is not initialized" message.
	NotInitializedSubstr = "is not initialized"
	// AlreadyRunningSubstr appears in the "may already be running" message.
	AlreadyRunningSubstr = "may already be running"
	// SchemaMismatchSubstr appears in the schema-version-mismatch message.
	SchemaMismatchSubstr = "has schema version"
	// VersionTooOldPrefix prefixes the refusal the bundle prints when it is older
	// than MinVersionEnv requires. It is checked before config load, so on that
	// path the bundle's only output is its version (stdout) and this line
	// (stderr). The manager detects the refusal by scanning for this prefix (the
	// same output-classification approach as the other startup failures), so it
	// still works for the detached `run` whose numeric exit it captures to the log.
	VersionTooOldPrefix = "SANDBOX_VERSION_TOO_OLD:"
)

// Environment variables the manager sets on the bundle for a managed run.
const (
	// sandboxEnvPrefix is the namespace for every bundle config knob. The CLI owns
	// it end to end: inherited KORBIT_SANDBOX_* vars are stripped from the child
	// environment (see withRuntimeEnv) so only the values the manager sets reach
	// the bundle. Every env name below carries this prefix, which also keeps it
	// inside the bundle run's --allow-env allowlist.
	sandboxEnvPrefix = "KORBIT_SANDBOX_"
	// MinVersionEnv tells the bundle the lowest version this CLI supports; an
	// older bundle refuses to start (printing its version + VersionTooOldPrefix),
	// so the manager can update + retry. Set on the start invocations (init-db /
	// run) unless the version check is skipped. The name's KORBIT_SANDBOX_ prefix
	// keeps it inside the bundle run's --allow-env allowlist.
	MinVersionEnv = "KORBIT_SANDBOX_MIN_VERSION"
	// LicenseCmdEnv overrides the command the bundle's banner footer names for the
	// full terms, so a re-surfaced banner points at `<prog> sandbox license`
	// instead of the standalone bundle invocation. Same allowlisted prefix.
	LicenseCmdEnv = "KORBIT_SANDBOX_LICENSE_CMD"
)

// Pidfile is the JSON the bundle writes next to its db file (at <db>-pid) once
// it binds, and removes on a clean exit. The CLI reads the actual bound port
// back from here (the single source of truth for the summary and the key pin).
type Pidfile struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// statusUser / statusKey / statusDoc mirror the shape of `status --json` that
// the CLI needs: per user, the keys with their api-key id, type, and (for an
// Ed25519 key) the PKCS#8 private PEM in `secret`, plus each market pair's
// market source (the paper-trading mode check). Only the fields the CLI reads
// are modeled; unknown fields are ignored.
type statusDoc struct {
	Users   []statusUser  `json:"users"`
	Markets statusMarkets `json:"markets"`
}

type statusMarkets struct {
	// InitializedSource is the market source the database was initialized with
	// ("walk" or "live"; empty on a bundle that does not report it). A database
	// initialized with `init-db --source live` is a paper-trading database even
	// when some pairs stayed on the simulated walk (production reported them
	// non-launched at initialization time).
	InitializedSource string       `json:"initializedSource"`
	Pairs             []statusPair `json:"pairs"`
}

type statusPair struct {
	Symbol string `json:"symbol"`
	// MarketSource is "walk" (simulated market) or "live" (paper trading:
	// mirrored production market data).
	MarketSource string `json:"marketSource"`
}

type statusUser struct {
	Keys []statusKey `json:"keys"`
}

type statusKey struct {
	APIKey string `json:"apiKey"`
	Type   string `json:"type"`
	Secret string `json:"secret"`
}
