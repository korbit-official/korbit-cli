// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/envalias"
	"github.com/digitalx-official/digitalx-cli/internal/output"
)

// Environment variables that choose the signing credential for an invocation.
const (
	// EnvKeyName selects a STORED key by its local name — the same selection the
	// --key flag makes. It is mutually exclusive with the inline-material vars.
	EnvKeyName = "DIGITALX_CLI_KEY"

	// EnvAPIKeyID, EnvAPIKeySecret and EnvAPIKeyType provide a credential INLINE,
	// with no keystore entry: the portal-issued api-key id, the secret material
	// (an ED25519 PKCS#8 PEM, or the HMAC-SHA256 shared secret), and the signing
	// scheme. All three are required together, and the whole group is mutually
	// exclusive with a stored-key selection (--key / DIGITALX_CLI_KEY).
	EnvAPIKeyID     = "DIGITALX_CLI_API_KEY_ID"
	EnvAPIKeySecret = "DIGITALX_CLI_API_KEY_SECRET"
	EnvAPIKeyType   = "DIGITALX_CLI_API_KEY_TYPE"
)

// InlineDisplayName is the Resolved.Name reported for an inline credential (it
// has no stored local name). It is what the "signing as key …" disclosure prints.
//
// It is DISPLAY-ONLY: never compare a name against it to detect inline mode.
// The supported signal is Selection.Inline / Resolved.Inline.
const InlineDisplayName = "(environment)"

// Selection is how a credential was chosen for one invocation: either inline
// material from the environment, or a stored key by name (Name "" => the default
// key). The two are mutually exclusive by construction — Select enforces it — so
// ResolveSelection has a single, unambiguous source.
type Selection struct {
	// Inline is true when DIGITALX_CLI_API_KEY_* supplies the credential directly;
	// no keystore is consulted and there is no per-key stored metadata (so the
	// per-key base-URL and defaultAccountSeq tiers do not apply to it).
	Inline bool
	// Name is the stored-key name to resolve when !Inline ("" => the default key).
	Name string
}

// InlineCredsPresent reports whether any inline-credential env var
// (DIGITALX_CLI_API_KEY_*) is set — i.e. the invocation is in inline-credential
// mode rather than using a stored key. Commands that manage stored keys (`setup`)
// use it to refuse rather than act on a keystore that is being bypassed.
func InlineCredsPresent(getenv func(string) string) bool {
	return envalias.Lookup(getenv, EnvAPIKeyID) != "" || envalias.Lookup(getenv, EnvAPIKeySecret) != "" || envalias.Lookup(getenv, EnvAPIKeyType) != ""
}

// Select determines the credential plan from the --key flag value and the
// environment, enforcing that inline env material (DIGITALX_CLI_API_KEY_*) and a
// stored-key selection (--key / DIGITALX_CLI_KEY) are MUTUALLY EXCLUSIVE. It reads
// no secret and never touches the keystore, so it is safe to call early — before
// metadata-only per-key peeks such as base URL and defaultAccountSeq — and fails
// fast on a contradictory invocation. It is the single front door every signing
// command uses, so key selection behaves identically across the program.
func Select(flagKey string, getenv func(string) string) (Selection, error) {
	inline := InlineCredsPresent(getenv)
	named := flagKey != "" || envalias.Lookup(getenv, EnvKeyName) != ""
	if inline && named {
		return Selection{}, output.Usagef(
			"choose ONE way to supply the key: a stored key (--key / %s) OR inline material (%s + %s + %s) — not both",
			EnvKeyName, EnvAPIKeyID, EnvAPIKeySecret, EnvAPIKeyType)
	}
	if inline {
		return Selection{Inline: true}, nil
	}
	name := flagKey
	if name == "" {
		name = envalias.Lookup(getenv, EnvKeyName)
	}
	return Selection{Name: name}, nil
}

// ResolveSelection resolves a Selection to a signing credential. The stored path
// defers to Resolve (keystore + registry); the inline path builds the credential
// straight from the DIGITALX_CLI_API_KEY_* vars without any keystore access.
func (m *Manager) ResolveSelection(sel Selection, getenv func(string) string) (Resolved, error) {
	if sel.Inline {
		return resolveInline(getenv)
	}
	return m.Resolve(sel.Name)
}

// resolveInline builds a Resolved from the inline-credential env vars. All three
// (id, secret, type) are required; the type is EXPLICIT — never detected from the
// material — so the scheme is never guessed. The secret is read but never echoed.
func resolveInline(getenv func(string) string) (Resolved, error) {
	id := strings.TrimSpace(envalias.Lookup(getenv, EnvAPIKeyID))
	secret := envalias.Lookup(getenv, EnvAPIKeySecret)
	keyType := strings.TrimSpace(envalias.Lookup(getenv, EnvAPIKeyType))
	switch {
	case id == "":
		return Resolved{}, output.Configf("%s is required when supplying a key inline", EnvAPIKeyID)
	case secret == "":
		return Resolved{}, output.Configf("%s is required when supplying a key inline", EnvAPIKeySecret)
	case keyType == "":
		return Resolved{}, output.Configf(
			"%s is required when supplying a key inline (%s or %s)", EnvAPIKeyType, TypeEd25519, TypeHMACSHA256)
	}
	signer, err := signerFor(InlineDisplayName, keyType, secret)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Name: InlineDisplayName, Type: keyType, APIKeyID: id, Inline: true, signer: signer}, nil
}
