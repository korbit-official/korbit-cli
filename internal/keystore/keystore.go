// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// Keystore stores secret strings (private key material) under a key name. Get
// returns "" when no entry exists; a non-nil error signals corruption or a
// backend failure, never a plain miss.
type Keystore interface {
	// Backend names the backend ("file" | "keychain"), surfaced in diagnostics.
	Backend() string
	Set(name, secret string) error
	Get(name string) (string, error)
	Delete(name string) error
}

// backend is one entry in the single internal registry of keystore backends.
// Using one map for both ByName (construction) and Available (probing) is the
// package's drift defense: a new backend is added in exactly ONE place here,
// plus its name in config.Backends, and nowhere else.
type backend struct {
	// new constructs the backend for the given CLI home. The home is only
	// meaningful to file-style backends; backends that ignore it (the OS
	// keyring) simply don't read it.
	new func(home string) Keystore
	// probe is the read-only reachability check behind Available. A nil probe
	// means "always available" (the file backend), which is distinct from a
	// probe that returns nil.
	probe func() error
}

// registry is the single source of truth for which backends this package can
// construct and probe. Its key set MUST equal config.Backends exactly — the CLI
// layer validates names against config.Backends (config.ValidBackend) while this
// package constructs and probes them, so a name in one list but not the other is
// a latent bug (constructible-but-CLI-rejected, or config-listed-but-permanently
// "unknown"). keystore_test.go's drift guard ties the two lists together and
// fails loudly if they diverge; config imports nothing from keystore, so this
// dependency direction is safe.
//
// To add a backend: add one entry here AND its name to config.Backends. The
// drift guard catches a miss on either side.
var registry = map[string]backend{
	"file": {
		new:   func(home string) Keystore { return NewFile(home) },
		probe: nil, // the file backend is always available
	},
	"keychain": {
		new:   func(home string) Keystore { return NewKeyring() },
		probe: ProbeKeyring,
	},
}

// errUnknownBackend is the sentinel wrapped by the "unknown keystore backend"
// error so callers (and the drift guard) can tell a NAME we don't recognize
// apart from a backend that is recognized but UNAVAILABLE on this machine —
// both surface as ConfigError with the same exit code, so the type alone can't
// distinguish them. The user-facing message is "unknown keystore backend %q";
// the wrapped sentinel lets errors.Is distinguish it from an availability error.
var errUnknownBackend = errors.New("unknown keystore backend")

// unknownBackend builds the user-facing "unknown keystore backend %q"
// ConfigError while wrapping errUnknownBackend, so errors.Is(err,
// errUnknownBackend) is true while the message text stays "unknown keystore
// backend %q" for the callers and tests that match on it.
func unknownBackend(name string) error {
	return &output.ConfigError{
		Message: fmt.Sprintf("unknown keystore backend %q", name),
		Cause:   errUnknownBackend,
	}
}

// ByName builds the keystore for an explicit backend name ("file" | "keychain").
// Key records select their backend by name, so this is the constructor the
// registry resolves through; migration also uses it to address the source and
// target backends at once. An unknown name is a ConfigError.
//
// log (nil = silent) is attached to the constructed backend so its per-key
// Set/Get/Delete and the file vault's lock/read/write steps are traceable under
// --debug. It is threaded here, at the single construction point, so every
// backend a Manager builds is instrumented automatically.
func ByName(name, home string, log *slog.Logger) (Keystore, error) {
	b, ok := registry[name]
	if !ok {
		return nil, unknownBackend(name)
	}
	ks := b.new(home)
	attachLogger(ks, log)
	return ks, nil
}

// attachLogger wires log into a freshly constructed backend so its operational
// diagnostics route through the one logger. It is the only place a backend's
// Log field is set, keeping the registry constructors logger-agnostic.
func attachLogger(ks Keystore, log *slog.Logger) {
	switch b := ks.(type) {
	case *FileKeystore:
		b.Log = log
	case *KeyringKeystore:
		b.Log = log
	}
}

// Available reports whether the named backend can be used on this machine. The
// file backend is always available; the keychain backend depends on the OS
// keyring being reachable (e.g. a headless Linux box with no Secret Service has
// none), so it is checked with a read-only reachability probe. A nil return
// means usable; a ConfigError explains why not.
//
// The two ConfigError cases are deliberately distinguishable: an unknown name
// wraps errUnknownBackend (errors.Is), while a recognized-but-unreachable
// backend returns the probe's own ConfigError. A CI box with no keyring
// legitimately hits the latter — the drift guard must not confuse it with the
// former.
func Available(name string, log *slog.Logger) error {
	l := logging.Or(log)
	b, ok := registry[name]
	if !ok {
		l.Debug("keystore availability probe: unknown backend", "backend", name)
		return unknownBackend(name)
	}
	if b.probe == nil {
		l.Debug("keystore backend always available", "backend", name)
		return nil
	}
	if err := b.probe(); err != nil {
		// Recoverable/diagnostic: the backend is recognized but not reachable here
		// (e.g. keychain on a headless host). The caller decides whether to fall
		// back; surface why at Warn so a --debug run explains it.
		l.Warn("keystore backend unavailable", "backend", name, "err", err)
		return err
	}
	l.Debug("keystore backend available", "backend", name)
	return nil
}
