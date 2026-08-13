// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/korbit-official/korbit-cli/internal/accountseq"
	"github.com/korbit-official/korbit-cli/internal/fslock"
	"github.com/korbit-official/korbit-cli/internal/keystore"
	"github.com/korbit-official/korbit-cli/internal/korbit"
	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
	"github.com/korbit-official/korbit-cli/internal/progname"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// TypeEd25519 and TypeHMACSHA256 are the signing schemes this build can sign
// with. The registry still loads records of OTHER types (so one future-typed key
// can't brick the file for the rest); those are rejected at use time instead.
//
//   - ed25519 — the user generates the keypair and registers the public key; the
//     stored secret is a PKCS#8 PEM, and the signature is base64.
//   - hmac-sha256 — Korbit issues the api-key id AND a shared secret together; the
//     stored secret is that opaque string, and the signature is hex. It has no
//     public key and is not generated locally.
const (
	TypeEd25519    = "ed25519"
	TypeHMACSHA256 = "hmac-sha256"
)

// keysVersion is the keys.json schema version this build reads and writes.
const keysVersion = 1

// SandboxAPIKeyPrefix is the prefix the local API sandbox gives every api-key
// id it issues (e.g. SANDBOX_ED25519_KEY_…). A real Korbit api-key id never
// matches it, so a key's bound id is the single, schema-free source of truth for
// "is this a sandbox key" — see IsSandbox. It is deliberately NOT the loopback
// base URL: a user may legitimately point a real key at a local proxy, so the
// endpoint says nothing about a key's origin; the api-key id does.
const SandboxAPIKeyPrefix = "SANDBOX_"

// IsSandbox reports whether a record is a sandbox key: its bound api-key id
// carries SandboxAPIKeyPrefix. An unbound key is never a sandbox key (there is
// no id to judge). This one predicate is the ONLY place the distinction is
// decided — every "treat sandbox keys specially" rule (never the default,
// `key use` refuses them, setup/doctor skip them, `key list` tags them) keys off
// it, so there is no scattered special-casing and no key "class" field.
func (r Record) IsSandbox() bool {
	return r.APIKeyID != nil && strings.HasPrefix(*r.APIKeyID, SandboxAPIKeyPrefix)
}

// SandboxNameToken is the substring every sandbox key's local name must carry.
// Sandbox keys are explicit-only (never the default — see IsSandbox), so a name
// that advertises the sandbox keeps every sandbox command self-evident on the
// command line: you select one with `--key <name>`, and the word "sandbox" is
// right there in the invocation. A real key is never forced to carry it.
const SandboxNameToken = "sandbox"

// nameAdvertisesSandbox reports whether a local key name carries SandboxNameToken
// (case-insensitive), so `--key MySandbox` satisfies the rule too.
func nameAdvertisesSandbox(name string) bool {
	return strings.Contains(strings.ToLower(name), SandboxNameToken)
}

// AssertSandboxKeyName checks that name is acceptable for a sandbox key — it must
// carry SandboxNameToken. The sandbox lifecycle calls this before it spawns a
// server, so a bad --key-name fails fast instead of after a successful start; the
// bind seams (AddBound/Bind/Rename) enforce the same rule as the persisted
// invariant, so a non-conforming sandbox key can never reach keys.json.
func AssertSandboxKeyName(name string) error {
	if !nameAdvertisesSandbox(name) {
		return output.Usagef(
			"a sandbox key's name must contain %q so its use stays explicit on the command line (e.g. `--key sandbox`); %q does not — choose a name containing %q", SandboxNameToken, name, SandboxNameToken)
	}
	return nil
}

// assertSandboxNaming refuses to bind a sandbox api-key id (one carrying
// SandboxAPIKeyPrefix) to a local name that does not advertise the sandbox. A
// non-sandbox id is always allowed under any name.
func assertSandboxNaming(name, apiKeyID string) error {
	if strings.HasPrefix(apiKeyID, SandboxAPIKeyPrefix) {
		return AssertSandboxKeyName(name)
	}
	return nil
}

// assertNotKeyMaterial refuses an api-key id that is actually ED25519 key
// material rather than the opaque id the portal issues. Both mistakes are easy to
// make because the CLI prints the keypair right beside the registration step:
//   - A public key binds a value the server rejects on every signed call with
//     KEY_NOT_FOUND. Catch it here with the fix rather than at use time.
//   - A private key is worse — it would write a secret into non-secret metadata
//     (keys.json). The message must never echo the pasted value.
func assertNotKeyMaterial(apiKeyID string) error {
	if korbit.LooksLikeEd25519PrivateKey(apiKeyID) {
		return output.Usagef(
			"that value is an ED25519 PRIVATE key — never use private key material as an API key id (it is secret). Register the corresponding public key at https://developers.korbit.co.kr, then paste the KEY ID the portal issues")
	}
	if korbit.LooksLikeEd25519PublicKey(apiKeyID) {
		return output.Usagef(
			"that value is an ED25519 public key, not an API key id — after you register the public key at https://developers.korbit.co.kr, paste the KEY ID the portal issues, not the public key itself")
	}
	return nil
}

// Record is the persisted metadata for one key. Type and Keystore are
// mandatory — a record without them fails to load (see doc.go for the two-axis
// model they encode).
type Record struct {
	// Type is the signing scheme — what kind of secret the keystore holds and
	// how requests are signed with it ("ed25519" or "hmac-sha256"). Unknown
	// values load fine and fail at use time.
	Type string `json:"type"`
	// Keystore names the backend holding this key's private material ("file" |
	// "keychain"). Per key on purpose: keys with different security needs live
	// in different backends. Changed only by `keystore migrate`.
	Keystore  string  `json:"keystore"`
	APIKeyID  *string `json:"apiKeyId"` // portal-issued X-KAPI-KEY id; null until bound
	PublicKey string  `json:"publicKey"`
	CreatedAt int64   `json:"createdAt"`
	// BaseURL optionally pins this key to a specific API endpoint, for the rare
	// case where a key's account is served from an alternate Korbit API host
	// rather than the default. Empty means "use the resolved default". It is
	// non-secret metadata, so it lives here rather than in the keystore.
	BaseURL string `json:"baseUrl,omitempty"`
	// WSBaseURL optionally pins this key to a specific WebSocket endpoint
	// (scheme ws/wss), the companion of BaseURL for the streaming `monitor`
	// command. Empty means "derive it from BaseURL". Non-secret metadata.
	WSBaseURL string `json:"wsBaseUrl,omitempty"`
	// DefaultAccountSeq optionally sets the sub-account sequence number that
	// accountseq resolution falls back to when the user omits --account-seq.
	// Nil means "not configured" (fall through to main account 1).
	DefaultAccountSeq *int `json:"defaultAccountSeq,omitempty"`
}

type keysFile struct {
	Version    int               `json:"version"`
	DefaultKey *string           `json:"defaultKey"`
	Keys       map[string]Record `json:"keys"`
}

// Summary is the public view of a key (no private material).
type Summary struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Keystore  string  `json:"keystore"`
	APIKeyID  *string `json:"apiKeyId"`
	Bound     bool    `json:"bound"`
	IsDefault bool    `json:"isDefault"`
	// IsSandbox is true when this key's bound api-key id carries the sandbox
	// prefix (Record.IsSandbox) — i.e. it signs against the local API sandbox,
	// never production. `key list` surfaces it as a [sandbox] tag.
	IsSandbox         bool   `json:"isSandbox"`
	CreatedAt         int64  `json:"createdAt"`
	BaseURL           string `json:"baseUrl,omitempty"`           // per-key API endpoint override; empty = default
	WSBaseURL         string `json:"wsBaseUrl,omitempty"`         // per-key WebSocket endpoint override; empty = derived from baseUrl
	DefaultAccountSeq *int   `json:"defaultAccountSeq,omitempty"` // per-key sub-account default; nil = not configured
}

// SummaryWithPublic adds the public key for `key show`.
type SummaryWithPublic struct {
	Summary
	PublicKey string `json:"publicKey"`
}

// Resolved is the credential needed to sign a request. It carries no exported
// secret: the stored material is parsed into a signer eagerly (in Resolve or
// resolveInline) and kept behind the unexported signer field, reachable only
// through Signer(). Unexporting alone does NOT make the struct safe to print —
// fmt reflects unexported fields — so Resolved also implements fmt.Formatter
// (see Format) to redact every verb. The signer itself (korbit.Signer) also
// self-redacts, so the struct is safe even if Format is ever bypassed.
type Resolved struct {
	Name     string
	Type     string
	Keystore string
	APIKeyID string
	// Inline is true when the credential came from inline environment material
	// (KORBIT_CLI_API_KEY_*) rather than a stored key, mirroring Selection.Inline.
	// It is the supported signal for "is this an inline credential?" — callers
	// MUST branch on it, never compare Name against InlineDisplayName (a display
	// string, not a sentinel).
	Inline bool
	// signer is built from the stored material for this key's type. It is
	// unexported AND screened by Format so no fmt path can leak it; callers reach
	// it only via Signer().
	signer korbit.Signer
}

// Signer returns the signer to hand to the signing layer (korbit). It is the
// single seam between stored key material and a usable signer: the per-`type`
// switch lives in signerFor, which Resolve and resolveInline call to build this
// value. New code MUST obtain its signer through this method and never hold raw
// key material.
func (r Resolved) Signer() (korbit.Signer, error) {
	if r.signer == nil {
		// Resolve/resolveInline only return a Resolved with a built signer; a nil
		// one means a zero-value struct was used directly, which is a programming
		// error rather than a config problem.
		return nil, output.Configf("key %q has no usable signer", r.Name)
	}
	return r.signer, nil
}

// signerFor builds the signer for a key of the given type from its stored
// secret. It is the ONE place a key's `type` is turned into a usable signer —
// both the keystore path (Resolve) and the inline-env path (resolveInline) go
// through it, so a new scheme is added here once. A type this build doesn't sign
// with, or material that doesn't fit its type, is a ConfigError (the vault/env
// holds the wrong thing for this key). The ed25519 parser's messages never
// include key bytes, so quoting them cannot leak material; an hmac secret is
// opaque and is never echoed.
func signerFor(name, keyType, secret string) (korbit.Signer, error) {
	switch keyType {
	case TypeEd25519:
		priv, err := korbit.ParsePrivatePEM(secret)
		if err != nil {
			return nil, output.Configf(
				"key %q is not a valid ED25519 private-key PEM (%v)", name, err)
		}
		return korbit.NewEd25519Signer(priv), nil
	case TypeHMACSHA256:
		if strings.TrimSpace(secret) == "" {
			return nil, output.Configf("key %q has an empty HMAC-SHA256 secret", name)
		}
		return korbit.NewHMACSHA256Signer([]byte(secret)), nil
	default:
		return nil, output.Configf(
			"key %q has type %q, which this build cannot sign with (supported: %s, %s) — use a newer CLI for this key", name, keyType, TypeEd25519, TypeHMACSHA256)
	}
}

// redactedSigner is what every fmt verb renders the secret as.
const redactedSigner = "REDACTED"

// Format makes Resolved safe to print. Because fmt reaches unexported fields by
// reflection, neither unexporting `signer` nor a plain String() stops a %+v or
// %#v from dumping the raw key bytes — only a Formatter intercepts EVERY verb.
// We render the non-secret fields and a redaction placeholder for the signer,
// matching the struct/Go-syntax shapes fmt would otherwise produce so the output
// still reads naturally. This is the leak-hardening seam for the credential.
func (r Resolved) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v':
		switch {
		case f.Flag('#'):
			fmt.Fprintf(f, "keys.Resolved{Name:%q, Type:%q, Keystore:%q, APIKeyID:%q, Inline:%v, signer:%s}",
				r.Name, r.Type, r.Keystore, r.APIKeyID, r.Inline, redactedSigner)
		case f.Flag('+'):
			fmt.Fprintf(f, "{Name:%s Type:%s Keystore:%s APIKeyID:%s Inline:%v signer:%s}",
				r.Name, r.Type, r.Keystore, r.APIKeyID, r.Inline, redactedSigner)
		default:
			fmt.Fprintf(f, "{%s %s %s %s %s}",
				r.Name, r.Type, r.Keystore, r.APIKeyID, redactedSigner)
		}
	case 's':
		fmt.Fprintf(f, "{%s %s %s %s %s}",
			r.Name, r.Type, r.Keystore, r.APIKeyID, redactedSigner)
	default:
		// Any other verb (e.g. %q) — fall back to the default %v shape rather
		// than risk a reflection path touching the signer.
		fmt.Fprintf(f, "{%s %s %s %s %s}",
			r.Name, r.Type, r.Keystore, r.APIKeyID, redactedSigner)
	}
}

// NewKey is the result of add/setup.
type NewKey struct {
	Name      string
	Type      string
	Keystore  string
	PublicKey string
	IsDefault bool
}

// OpenFunc builds the keystore for a backend name (keystore.ByName in
// production; tests inject mock backends).
type OpenFunc func(backend string) (keystore.Keystore, error)

// Manager owns keys.json and resolves each key's keystore backend through it.
type Manager struct {
	home           string
	path           string
	defaultBackend string
	open           OpenFunc
	probe          func(backend string) error
	now            func() int64

	// Log is an optional operational logger (nil = silent) for the registry's
	// load/lock/save steps and lifecycle milestones (key created, default set,
	// key removed/renamed/migrated). It logs the resolved key name + backend +
	// api-key-id (all non-secret) — NEVER the secret. Set by NewManager from its
	// log argument; NewManagerWithBackends leaves it nil unless a caller sets it.
	Log *slog.Logger

	// cacheMu guards an in-process parse of keys.json reused by the read-only
	// metadata peeks, so a single command (or a long-running mcp/tui/monitor
	// session, which holds one Manager) parses the file once instead of on
	// every peek. Mutations bypass it (they read fresh under the registry lock)
	// and invalidate it on save; Invalidate refreshes it after an out-of-band
	// in-process change. nil = not yet read / invalidated.
	cacheMu sync.Mutex
	cached  *keysFile
}

// NewManager builds a key manager over the real keystore backends.
// RegistryPath is the key registry path under home (keys.json).
func RegistryPath(home string) string { return filepath.Join(home, "keys.json") }

// defaultBackend (config.json `keystore`) applies only to keys created from
// now on — existing keys carry their own backend in keys.json. now is
// injectable for tests. log (nil = silent) is threaded into the constructed
// keystore backends (so their per-key Set/Get/Delete and the file vault's
// lock/read/write are traceable) AND stored on the Manager for its own registry
// diagnostics.
func NewManager(home, defaultBackend string, now func() int64, log *slog.Logger) *Manager {
	m := NewManagerWithBackends(home, defaultBackend,
		func(b string) (keystore.Keystore, error) { return keystore.ByName(b, home, log) },
		func(b string) error { return keystore.Available(b, log) }, now)
	m.Log = log
	return m
}

// NewManagerWithBackends is NewManager with the backend constructor and the
// availability probe injectable, so tests can run against mock keystores
// (platform keyrings aren't usable in unit tests).
func NewManagerWithBackends(home, defaultBackend string, open OpenFunc, probe func(string) error, now func() int64) *Manager {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return &Manager{
		home:           home,
		path:           RegistryPath(home),
		defaultBackend: defaultBackend,
		open:           open,
		probe:          probe,
		now:            now,
	}
}

// DefaultBackend reports the keystore backend new keys go to.
func (m *Manager) DefaultBackend() string { return m.defaultBackend }

// log returns the wired logger or a no-op one, so call sites log unconditionally.
func (m *Manager) log() *slog.Logger { return logging.Or(m.Log) }

func assertName(name string) error {
	if !nameRE.MatchString(name) {
		return output.Usagef(
			`invalid key name %q — use 1-64 characters: letters, digits, dot, underscore, hyphen (starting with a letter or digit)`, name)
	}
	return nil
}

// assertSupportedType gates the operations that interpret a key's secret
// (signing). Records of unknown types stay listable and migratable — only use
// fails.
func assertSupportedType(name string, rec Record) error {
	if rec.Type != TypeEd25519 && rec.Type != TypeHMACSHA256 {
		return output.Configf(
			"key %q has type %q, which this build cannot sign with (supported: %s, %s) — use a newer CLI for this key", name, rec.Type, TypeEd25519, TypeHMACSHA256)
	}
	return nil
}

// lock takes the exclusive cross-process REGISTRY lock (keys.json.lock),
// serializing every mutating method's whole load → modify → atomic-rename
// cycle against other korbit-cli processes (a long-running monitor bot plus
// ad-hoc commands). Without it, two concurrent mutations each load the same
// snapshot and the second rename silently discards the first's change. Read-only
// methods do NOT take it: the atomic rename guarantees they observe a complete,
// self-consistent snapshot. The returned func MUST be deferred.
//
// LOCK ORDERING: this is the registry lock, acquired BEFORE any vault lock. A
// mutating method that also drives the file vault (Add, Remove, Rename) holds
// this lock while keystore.FileKeystore.Set/Delete take the SEPARATE vault lock
// (keystore.json.lock). That nesting is deadlock-free only because they are
// different lock files and the order is always registry → vault, never the
// reverse — see doc.go.
func (m *Manager) lock() (func(), error) {
	lockPath := m.path + ".lock"
	m.log().Debug("registry lock: acquiring", "lock", lockPath)
	start := time.Now()
	unlock, err := fslock.Lock(lockPath)
	if err != nil {
		m.log().Warn("registry lock: acquire failed", "lock", lockPath, "err", err)
		return nil, err
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		// A non-trivial wait means another korbit-cli process held the registry
		// lock — high-value contention signal.
		m.log().Warn("registry lock: acquired after contention", "lock", lockPath, "waitedMs", waited.Milliseconds())
	} else {
		m.log().Debug("registry lock: acquired", "lock", lockPath)
	}
	return func() {
		unlock()
		m.log().Debug("registry lock: released", "lock", lockPath)
	}, nil
}

func (m *Manager) load() (keysFile, error) {
	raw, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		m.log().Debug("registry load: not present, empty", "path", m.path)
		return keysFile{Version: keysVersion, Keys: map[string]Record{}}, nil
	}
	if err != nil {
		m.log().Warn("registry load: read failed", "path", m.path, "err", err)
		return keysFile{}, err
	}
	var file keysFile
	if err := json.Unmarshal(raw, &file); err != nil {
		m.log().Warn("registry load: not valid JSON", "path", m.path)
		return keysFile{}, output.Configf("%s is corrupted (not valid JSON)", m.path)
	}
	if file.Version != keysVersion || file.Keys == nil {
		m.log().Warn("registry load: unsupported format", "path", m.path, "version", file.Version)
		return keysFile{}, output.Configf("%s has an unsupported format (expected version %d)", m.path, keysVersion)
	}
	// type and keystore are mandatory per record. Their VALUES are deliberately
	// not validated here: a record written by a newer CLI (an unknown type or
	// backend) must still load so the rest of the file stays usable — it fails
	// at use time instead, for that key only.
	for name, rec := range file.Keys {
		if rec.Type == "" || rec.Keystore == "" {
			m.log().Warn("registry load: record missing type/keystore", "path", m.path, "key", name)
			return keysFile{}, output.Configf(`%s: key %q must have "type" and "keystore" fields`, m.path, name)
		}
		// defaultAccountSeq, when present, is a hard wire constraint (>= 1), not a
		// forward-compat value — reject an out-of-range one at load so it can't
		// surface later as a misattributed --account-seq error.
		if rec.DefaultAccountSeq != nil && *rec.DefaultAccountSeq < accountseq.Main {
			m.log().Warn("registry load: invalid defaultAccountSeq", "path", m.path, "key", name, "defaultAccountSeq", *rec.DefaultAccountSeq)
			return keysFile{}, output.Configf(`%s: key %q has an invalid "defaultAccountSeq" %d (must be >= %d)`, m.path, name, *rec.DefaultAccountSeq, accountseq.Main)
		}
		// Tolerant-on-load: a record written by a newer CLI (unknown type or
		// backend) still loads so the rest of the registry stays usable; it fails
		// only at use time, for that key alone. Surface it at Debug so a --debug run
		// explains why a later sign/migrate of this one key errors.
		if rec.Type != TypeEd25519 && rec.Type != TypeHMACSHA256 {
			m.log().Debug("registry load: record has unknown type (loads, fails at use)", "key", name, "type", rec.Type, "backend", rec.Keystore)
		}
	}
	m.log().Debug("registry load: parsed", "path", m.path, "records", len(file.Keys))
	return file, nil
}

// loadCached returns a parse of keys.json reused across calls, for the
// read-only metadata peeks. It reads disk once per Manager and refreshes only
// when a mutation invalidates the cache (save / Invalidate). Mutating methods
// must NOT use it — they read fresh on-disk state under the registry lock.
//
// The returned keysFile shares its Keys map with the cache; callers here only
// read it, and mutations never touch the cached map (they parse anew via load),
// so the cached snapshot is effectively immutable while live.
func (m *Manager) loadCached() (keysFile, error) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.cached != nil {
		return *m.cached, nil
	}
	file, err := m.load()
	if err != nil {
		return keysFile{}, err
	}
	m.cached = &file
	return file, nil
}

// Invalidate drops the cached keys.json so the next read-only peek re-parses
// disk. save calls it after every successful write; a long-running server that
// changes keys through a separate path (e.g. the mcp setup tool)
// calls it so subsequent peeks see the change without a restart.
func (m *Manager) Invalidate() {
	m.cacheMu.Lock()
	m.cached = nil
	m.cacheMu.Unlock()
}

// save writes keys.json atomically (temp file + rename). This file is the
// commit point of a keystore migration — flipping a record's `keystore` field
// is the moment the key officially lives in the new backend — so a crash
// mid-write must not be able to truncate it.
func (m *Manager) save(file keysFile) error {
	if err := os.MkdirAll(m.home, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	m.log().Debug("registry write: temp written, renaming", "tmp", tmp, "path", m.path, "records", len(file.Keys))
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.log().Debug("registry write: committed", "path", m.path)
	m.Invalidate()
	return nil
}

func must(file keysFile, name string) (Record, error) {
	rec, ok := file.Keys[name]
	if !ok {
		return Record{}, output.Usagef("unknown key %q — run `%s key list`", name, progname.Name())
	}
	return rec, nil
}

// storeFor opens the keystore backend a key's record points at.
func (m *Manager) storeFor(name string, rec Record) (keystore.Keystore, error) {
	st, err := m.open(rec.Keystore)
	if err != nil {
		return nil, output.Configf("key %q: %v", name, err)
	}
	return st, nil
}

// Add creates a new named key. If privatePEM is non-empty it imports that key;
// otherwise it generates a fresh keypair. backend selects the keystore for the
// new key's private material; "" uses the default (config.json `keystore`).
// The first key created becomes the default key automatically.
func (m *Manager) Add(name, privatePEM, backend string) (NewKey, error) {
	if err := assertName(name); err != nil {
		return NewKey{}, err
	}
	if backend == "" {
		backend = m.defaultBackend
	}
	// Fail fast — before generating or storing anything — when the chosen
	// backend can't be used here (e.g. keychain on a headless host).
	if err := m.probe(backend); err != nil {
		return NewKey{}, err
	}
	store, err := m.open(backend)
	if err != nil {
		return NewKey{}, err
	}
	unlock, err := m.lock()
	if err != nil {
		return NewKey{}, err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return NewKey{}, err
	}
	if _, exists := file.Keys[name]; exists {
		return NewKey{}, output.Usagef(
			"key %q already exists — remove it first (`%s key remove %s`) to replace it, or pick another name", name, progname.Name(), name)
	}

	var publicKey string
	if privatePEM != "" {
		if err := korbit.AssertEd25519PrivatePEM(privatePEM); err != nil {
			return NewKey{}, err
		}
		if publicKey, err = korbit.PublicPEMFromPrivate(privatePEM); err != nil {
			return NewKey{}, err
		}
	} else {
		kp, err := korbit.GenerateKeypair()
		if err != nil {
			return NewKey{}, err
		}
		privatePEM, publicKey = kp.PrivatePEM, kp.PublicPEM
	}

	if err := store.Set(name, privatePEM); err != nil {
		return NewKey{}, err
	}
	file.Keys[name] = Record{Type: TypeEd25519, Keystore: backend, PublicKey: publicKey, CreatedAt: m.now()}
	// Default this key when none is set yet. A key created here is never a
	// sandbox key (Add always leaves it unbound — the sandbox import uses
	// AddBound instead), so it is a safe default. Using "no default set" rather
	// than "len == 1" means a real key added *after* a non-default sandbox key
	// (which left DefaultKey nil) still correctly becomes the default.
	isDefault := file.DefaultKey == nil
	if isDefault {
		file.DefaultKey = &name
	}
	if err := m.save(file); err != nil {
		return NewKey{}, err
	}
	m.log().Info("key created", "key", name, "type", TypeEd25519, "backend", backend, "isDefault", isDefault)
	return NewKey{Name: name, Type: TypeEd25519, Keystore: backend, PublicKey: publicKey, IsDefault: isDefault}, nil
}

// AddBound creates a new named key that is already bound to apiKeyID, importing
// privatePEM (required — there is no fresh-keypair path here, since a bound key
// must match an already-issued api-key id). It exists so a key is born with its
// binding in place, which makes the default rule decidable at creation: the
// default becomes the first *non-sandbox* key, so a sandbox key (whose id
// carries SandboxAPIKeyPrefix) is NEVER even transiently the default.
//
// This is why the sandbox import must NOT use Add+Bind: Add defaults the first
// key before Bind attaches the id, so on a fresh install the sandbox key would
// briefly BE the default — defeating the "sandbox use is explicit-only" safety
// property. AddBound closes that hole by attaching the id atomically.
func (m *Manager) AddBound(name, privatePEM, apiKeyID, backend string) (NewKey, error) {
	if err := assertName(name); err != nil {
		return NewKey{}, err
	}
	id := strings.TrimSpace(apiKeyID)
	if id == "" {
		return NewKey{}, output.Usagef("--api-key must not be empty")
	}
	if err := assertNotKeyMaterial(id); err != nil {
		return NewKey{}, err
	}
	if err := assertSandboxNaming(name, id); err != nil {
		return NewKey{}, err
	}
	if privatePEM == "" {
		return NewKey{}, output.Usagef("AddBound requires private key material")
	}
	if backend == "" {
		backend = m.defaultBackend
	}
	if err := m.probe(backend); err != nil {
		return NewKey{}, err
	}
	store, err := m.open(backend)
	if err != nil {
		return NewKey{}, err
	}
	if err := korbit.AssertEd25519PrivatePEM(privatePEM); err != nil {
		return NewKey{}, err
	}
	publicKey, err := korbit.PublicPEMFromPrivate(privatePEM)
	if err != nil {
		return NewKey{}, err
	}
	unlock, err := m.lock()
	if err != nil {
		return NewKey{}, err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return NewKey{}, err
	}
	if _, exists := file.Keys[name]; exists {
		return NewKey{}, output.Usagef(
			"key %q already exists — remove it first (`%s key remove %s`) to replace it, or pick another name", name, progname.Name(), name)
	}
	if err := store.Set(name, privatePEM); err != nil {
		return NewKey{}, err
	}
	rec := Record{Type: TypeEd25519, Keystore: backend, APIKeyID: &id, PublicKey: publicKey, CreatedAt: m.now()}
	file.Keys[name] = rec
	// Default the first NON-sandbox key. The record is born bound, so IsSandbox
	// is decidable now: a sandbox key never becomes the default, a real key does.
	isDefault := file.DefaultKey == nil && !rec.IsSandbox()
	if isDefault {
		file.DefaultKey = &name
	}
	if err := m.save(file); err != nil {
		return NewKey{}, err
	}
	m.log().Info("key created (bound)", "key", name, "type", TypeEd25519, "backend", backend, "apiKeyId", id, "isDefault", isDefault)
	return NewKey{Name: name, Type: TypeEd25519, Keystore: backend, PublicKey: publicKey, IsDefault: isDefault}, nil
}

// AddHMAC creates a new named HMAC-SHA256 key, already bound to apiKeyID. An
// HMAC key has no keypair and no public key: Korbit issues the api-key id and the
// shared secret together, so the key is born bound (there is nothing to register)
// — the same reason AddBound exists for an imported ED25519 key. secret is the
// opaque shared secret; it is stored verbatim in the chosen backend. backend ""
// uses the default for new keys.
//
// Like AddBound it never becomes the default if it is a sandbox key, and a
// sandbox api-key id requires a sandbox-advertising name.
func (m *Manager) AddHMAC(name, secret, apiKeyID, backend string) (NewKey, error) {
	if err := assertName(name); err != nil {
		return NewKey{}, err
	}
	id := strings.TrimSpace(apiKeyID)
	if id == "" {
		return NewKey{}, output.Usagef("--api-key must not be empty")
	}
	if err := assertNotKeyMaterial(id); err != nil {
		return NewKey{}, err
	}
	if err := assertSandboxNaming(name, id); err != nil {
		return NewKey{}, err
	}
	if strings.TrimSpace(secret) == "" {
		return NewKey{}, output.Usagef("the HMAC-SHA256 secret must not be empty")
	}
	if backend == "" {
		backend = m.defaultBackend
	}
	if err := m.probe(backend); err != nil {
		return NewKey{}, err
	}
	store, err := m.open(backend)
	if err != nil {
		return NewKey{}, err
	}
	unlock, err := m.lock()
	if err != nil {
		return NewKey{}, err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return NewKey{}, err
	}
	if _, exists := file.Keys[name]; exists {
		return NewKey{}, output.Usagef(
			"key %q already exists — pick another name, or remove it first with `%s key remove %s`", name, progname.Name(), name)
	}
	if err := store.Set(name, secret); err != nil {
		return NewKey{}, err
	}
	rec := Record{Type: TypeHMACSHA256, Keystore: backend, APIKeyID: &id, CreatedAt: m.now()}
	file.Keys[name] = rec
	isDefault := file.DefaultKey == nil && !rec.IsSandbox()
	if isDefault {
		file.DefaultKey = &name
	}
	if err := m.save(file); err != nil {
		return NewKey{}, err
	}
	m.log().Info("key created (bound)", "key", name, "type", TypeHMACSHA256, "backend", backend, "apiKeyId", id, "isDefault", isDefault)
	return NewKey{Name: name, Type: TypeHMACSHA256, Keystore: backend, IsDefault: isDefault}, nil
}

// Bind attaches the portal-issued API key id to a named key.
func (m *Manager) Bind(name, apiKeyID string) error {
	id := strings.TrimSpace(apiKeyID)
	if id == "" {
		return output.Usagef("--api-key must not be empty")
	}
	if err := assertNotKeyMaterial(id); err != nil {
		return err
	}
	if err := assertSandboxNaming(name, id); err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	if rec.Type != TypeEd25519 {
		if rec.Type == TypeHMACSHA256 {
			return output.Usagef(
				"key bind is only for %s keys; key %q is an %s key, whose api-key id and shared secret are issued as a pair — replace it with `%s key remove %s` then `%s key add %s --type %s --api-key <KEY_ID> --secret-file <file>`",
				TypeEd25519, name, TypeHMACSHA256, progname.Name(), name, progname.Name(), name, TypeHMACSHA256)
		}
		return output.Configf(
			"key bind is only for %s keys; key %q has type %q, which this build cannot bind — use a newer CLI for this key",
			TypeEd25519, name, rec.Type)
	}
	rec.APIKeyID = &id
	file.Keys[name] = rec
	if err := m.save(file); err != nil {
		return err
	}
	m.log().Info("key bound to api-key id", "key", name, "apiKeyId", id)
	return nil
}

// Use sets the default key.
func (m *Manager) Use(name string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	// A sandbox key must never be made the default — sandbox use is explicit-only
	// (a bare command would otherwise silently sign against the local mock). The
	// non-default import already keeps it out of the default slot; this stops a
	// user from putting it there by hand too.
	if rec.IsSandbox() {
		return output.Usagef(
			"key %q is a sandbox key and can't be the default — pass --key %s explicitly when you want it", name, name)
	}
	file.DefaultKey = &name
	if err := m.save(file); err != nil {
		return err
	}
	m.log().Info("default key set", "key", name)
	return nil
}

// ValidateBaseURL reports whether raw is a well-formed REST base URL, for
// callers that pre-check --base-url before creating a key (so an invalid value
// fails before any key is written, not after).
func ValidateBaseURL(raw string) error {
	_, err := validateBaseURL(raw)
	return err
}

// ValidateWSBaseURL reports whether raw is a well-formed WebSocket base URL.
func ValidateWSBaseURL(raw string) error {
	_, err := validateWSBaseURL(raw)
	return err
}

// validateBaseURL accepts a well-formed http(s) URL and returns it with any
// trailing slash trimmed. The stricter "don't sign over plaintext http to a
// non-local host" rule is a use-time check applied when the key is actually
// used, not here at set time.
func validateBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", output.Usagef("base URL must be an http(s) URL (got %q)", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// validateWSBaseURL accepts a well-formed ws(s) URL and returns it with any
// trailing slash trimmed. Like validateBaseURL, the "don't sign over plaintext
// ws to a non-local host" rule is a use-time check, not enforced here.
func validateWSBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return "", output.Usagef("WebSocket base URL must be a ws(s) URL (got %q)", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// SetBaseURL pins a key to a specific API endpoint (validated http(s) URL) and
// its companion WebSocket endpoint (validated ws(s) URL). An empty wsRaw leaves
// the key with no stored WebSocket override, so it is derived from the REST base
// URL at use time. The caller (the set-base-url command) supplies wsRaw either
// from an explicit override or by deriving it from raw.
func (m *Manager) SetBaseURL(name, raw, wsRaw string) error {
	clean, err := validateBaseURL(raw)
	if err != nil {
		return err
	}
	var cleanWS string
	if wsRaw != "" {
		if cleanWS, err = validateWSBaseURL(wsRaw); err != nil {
			return err
		}
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	rec.BaseURL = clean
	rec.WSBaseURL = cleanWS
	file.Keys[name] = rec
	return m.save(file)
}

// ClearBaseURL removes a key's endpoint overrides (both REST and WebSocket),
// reverting it to the default.
func (m *Manager) ClearBaseURL(name string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	rec.BaseURL = ""
	rec.WSBaseURL = ""
	file.Keys[name] = rec
	return m.save(file)
}

// BaseURLOf returns the endpoint override stored for an exact key name (""
// when none), erroring only if the key is unknown or keys.json is unreadable.
func (m *Manager) BaseURLOf(name string) (string, error) {
	file, err := m.load()
	if err != nil {
		return "", err
	}
	rec, err := must(file, name)
	if err != nil {
		return "", err
	}
	return rec.BaseURL, nil
}

// KeyMeta is the non-secret, per-key metadata peeked before signing: the
// endpoint overrides and the default sub-account. The zero value means nothing
// is configured — also what an inline credential (no stored metadata) yields.
type KeyMeta struct {
	BaseURL           string // per-key REST endpoint override; "" = none
	WSBaseURL         string // per-key WebSocket endpoint override; "" = derive
	DefaultAccountSeq string // per-key sub-account default as a decimal string; "" = not configured
}

// metaFor resolves the record for the explicit name (else the default key) from
// a cached keys.json read and returns its non-secret metadata. Best-effort and
// never errors: an unknown/missing key, an unreadable file, or a nil Manager
// yields the zero KeyMeta, so the caller's later Resolve produces the canonical
// error.
func (m *Manager) metaFor(explicit string) KeyMeta {
	if m == nil {
		return KeyMeta{}
	}
	file, err := m.loadCached()
	if err != nil {
		return KeyMeta{}
	}
	name := explicit
	if name == "" && file.DefaultKey != nil {
		name = *file.DefaultKey
	}
	if name == "" {
		return KeyMeta{}
	}
	rec, ok := file.Keys[name]
	if !ok {
		return KeyMeta{}
	}
	out := KeyMeta{BaseURL: rec.BaseURL, WSBaseURL: rec.WSBaseURL}
	if rec.DefaultAccountSeq != nil {
		out.DefaultAccountSeq = strconv.Itoa(*rec.DefaultAccountSeq)
	}
	return out
}

// MetaForSelection returns the per-key metadata for a key selection in one peek,
// honoring the inline rule: an inline (KORBIT_CLI_API_KEY_*) credential has no
// stored metadata, so it yields the zero KeyMeta and the per-key base-URL /
// defaultAccountSeq tiers are skipped. It is the single front door for the
// command paths (endpoint/tui/monitor) that need several of these at once.
func (m *Manager) MetaForSelection(sel Selection) KeyMeta {
	if sel.Inline {
		return KeyMeta{}
	}
	return m.metaFor(sel.Name)
}

// MetaBaseURL is a best-effort, metadata-only lookup of the endpoint override
// for the key that would be used (the explicit name, else the default). It never
// touches the keystore and never errors: an unknown/missing key or unreadable
// file yields "", so the caller's later Resolve produces the canonical error.
func (m *Manager) MetaBaseURL(explicit string) string { return m.metaFor(explicit).BaseURL }

// MetaWSBaseURL is the WebSocket counterpart of MetaBaseURL: "" when there is no
// stored override (the caller then derives it from the REST base URL).
func (m *Manager) MetaWSBaseURL(explicit string) string { return m.metaFor(explicit).WSBaseURL }

// SetDefaultAccountSeq stores a per-key default sub-account sequence number.
// accountSeq must be >= 1; pass 0 to clear the override (revert to main account).
func (m *Manager) SetDefaultAccountSeq(name string, accountSeq int) error {
	if accountSeq < 0 {
		return output.Usagef("accountSeq must be >= 1")
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	if accountSeq == 0 {
		rec.DefaultAccountSeq = nil
	} else {
		rec.DefaultAccountSeq = &accountSeq
	}
	file.Keys[name] = rec
	return m.save(file)
}

// MetaDefaultAccountSeq is a best-effort, metadata-only lookup of the per-key
// default sub-account sequence for the key that would be used (the explicit
// name, else the default). Returns "" when not configured; the caller passes
// the value into accountseq.Inputs.Default.
func (m *Manager) MetaDefaultAccountSeq(explicit string) string {
	return m.metaFor(explicit).DefaultAccountSeq
}

// SetKeystoreBackend re-points a key's record at another keystore backend. It
// is the COMMIT step of `keystore migrate` — the engine calls it only after
// the key's material is verified in the target — and must not be used as a
// casual setter: flipping the record without moving the material orphans the
// key.
func (m *Manager) SetKeystoreBackend(name, backend string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return err
	}
	rec, err := must(file, name)
	if err != nil {
		return err
	}
	prev := rec.Keystore
	rec.Keystore = backend
	file.Keys[name] = rec
	if err := m.save(file); err != nil {
		return err
	}
	// This is the commit hook of a keystore migration — the moment the record
	// officially points at the new backend.
	m.log().Info("key keystore backend re-pointed (migration commit)", "key", name, "from", prev, "to", backend)
	return nil
}

// Names returns every key name, sorted. It is metadata-only (reads keys.json,
// never a keystore).
func (m *Manager) Names() ([]string, error) {
	file, err := m.loadCached()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(file.Keys))
	for name := range file.Keys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// DefaultKeyName returns the default key name, or "" if none.
func (m *Manager) DefaultKeyName() (string, error) {
	file, err := m.loadCached()
	if err != nil {
		return "", err
	}
	if file.DefaultKey == nil {
		return "", nil
	}
	return *file.DefaultKey, nil
}

func summarize(name string, rec Record, def *string) Summary {
	return Summary{
		Name:              name,
		Type:              rec.Type,
		Keystore:          rec.Keystore,
		APIKeyID:          rec.APIKeyID,
		Bound:             rec.APIKeyID != nil,
		IsDefault:         def != nil && *def == name,
		IsSandbox:         rec.IsSandbox(),
		CreatedAt:         rec.CreatedAt,
		BaseURL:           rec.BaseURL,
		WSBaseURL:         rec.WSBaseURL,
		DefaultAccountSeq: rec.DefaultAccountSeq,
	}
}

// List returns all keys, sorted by name.
func (m *Manager) List() ([]Summary, error) {
	file, err := m.loadCached()
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(file.Keys))
	for name, rec := range file.Keys {
		out = append(out, summarize(name, rec, file.DefaultKey))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Show returns one key's summary plus its public key.
func (m *Manager) Show(name string) (SummaryWithPublic, error) {
	file, err := m.load()
	if err != nil {
		return SummaryWithPublic{}, err
	}
	rec, err := must(file, name)
	if err != nil {
		return SummaryWithPublic{}, err
	}
	return SummaryWithPublic{Summary: summarize(name, rec, file.DefaultKey), PublicKey: rec.PublicKey}, nil
}

// RemoveResult reports what happened to the default after a removal.
type RemoveResult struct {
	Removed    string  `json:"removed"`
	DefaultKey *string `json:"defaultKey"`
	// SecretRemoved reports whether the private key material was actually deleted
	// from its backend. It is false only on a forced removal that had to leave the
	// secret behind (unknown/unavailable backend, or a Delete that errored); a
	// normal removal always deletes the secret or fails. The field is additive —
	// a value of false with no Warning never occurs.
	SecretRemoved bool `json:"secretRemoved"`
	// Warning, when set, explains that the record was removed but its secret was
	// left in its backend (only on a forced removal). Empty on a clean removal.
	Warning string `json:"warning,omitempty"`
}

// Remove deletes a key's record and, normally, its private key from the key's
// own backend. If the removed key was the default, the default becomes unset (no
// silent fallback — the default is never re-pointed at another, possibly
// different-account, key).
//
// The secret is deleted BEFORE the record is dropped only on the happy path; the
// ordering matters because the record is the single source of truth for where a
// key's secret lives, so the record must outlive any failed delete that a user
// might want to retry. That makes a stranded key — one whose record names a
// backend this build doesn't know (written by a newer CLI), or whose backend is
// unavailable (the record points at the OS keychain on a headless host), or
// whose Delete errors — impossible to remove without an escape hatch: the record
// squats in `key list` forever and its name stays reserved.
//
// force is that escape hatch. Without it, an undeletable secret is a hard error
// that points the user at --force (the no-fallback invariant is never the
// blocker — only the secret is). With it, secret deletion is best-effort:
//   - backend opens and Delete succeeds → full removal, SecretRemoved true.
//   - backend unknown/unavailable, or Delete errors → the record is removed (and
//     the default unset if it was this key) anyway, SecretRemoved false, and
//     Warning names the backend the orphaned secret was left in.
//
// --force on a healthy key behaves exactly like a normal remove (secret deleted,
// SecretRemoved true, no warning).
func (m *Manager) Remove(name string, force bool) (RemoveResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return RemoveResult{}, err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return RemoveResult{}, err
	}
	rec, err := must(file, name)
	if err != nil {
		return RemoveResult{}, err
	}

	secretRemoved := true
	var warning string
	if store, openErr := m.storeFor(name, rec); openErr != nil {
		// The backend can't even be opened (unknown name from a newer CLI, or an
		// unavailable backend). Without force this is fatal; with force we strand
		// the secret deliberately and say so.
		if !force {
			return RemoveResult{}, output.Configf(
				"cannot remove key %q: its keystore backend (%s) can't be opened to delete the secret (%v) — re-run with --force to remove the record anyway (the secret, if any, stays in that backend)", name, rec.Keystore, openErr)
		}
		secretRemoved = false
		warning = fmt.Sprintf("key %q removed, but its secret could not be deleted from the %s keystore (%v) — if any material remains there, delete it manually", name, rec.Keystore, openErr)
	} else if delErr := store.Delete(name); delErr != nil {
		if !force {
			return RemoveResult{}, output.Configf(
				"cannot remove key %q: deleting its secret from the %s keystore failed (%v) — re-run with --force to remove the record anyway (the secret stays in that backend)", name, rec.Keystore, delErr)
		}
		secretRemoved = false
		warning = fmt.Sprintf("key %q removed, but its secret could not be deleted from the %s keystore (%v) — delete it there manually", name, rec.Keystore, delErr)
	}

	delete(file.Keys, name)
	if file.DefaultKey != nil && *file.DefaultKey == name {
		file.DefaultKey = nil
	}
	if err := m.save(file); err != nil {
		return RemoveResult{}, err
	}
	if warning != "" {
		// Forced removal that had to strand the secret in its backend.
		m.log().Warn("key removed, secret left in backend", "key", name, "backend", rec.Keystore, "force", force)
	}
	m.log().Info("key removed", "key", name, "backend", rec.Keystore, "secretRemoved", secretRemoved, "force", force)
	return RemoveResult{Removed: name, DefaultKey: file.DefaultKey, SecretRemoved: secretRemoved, Warning: warning}, nil
}

// RenameResult reports the outcome of a rename.
type RenameResult struct {
	OldName string `json:"oldName"`
	NewName string `json:"newName"`
	// IsDefault is true when the renamed key was the default and so the default
	// now follows it to the new name. This is the SAME key/account, so it is not
	// the forbidden silent re-point to a different account.
	IsDefault bool `json:"isDefault"`
	// SecretMoved reports whether private material was actually moved in the vault.
	// It is false only for an already-secretless record (renamed metadata-only).
	SecretMoved bool `json:"secretMoved"`
	// Warning, when set, reports that the rename committed but the OLD vault entry
	// could not be deleted afterwards (the secret was left under the old name).
	Warning string `json:"warning,omitempty"`
}

// Rename relabels a key: the registry record — its API-key binding, public key,
// keystore backend, per-key base URL, and creation time — is
// preserved under newName, and the private material is moved WITHIN the key's own
// backend. The keypair and the portal binding are unchanged, so no
// re-registration is needed (unlike remove + re-add, which destroys the keypair).
//
// The vault move follows the keystore.Migrate safety ordering — copy into the new
// name → verify the read-back → commit the registry rename (atomic keys.json
// write) → only then delete the old vault entry. A failure before the commit
// rolls back the in-flight copy and leaves the key untouched under its old name; a
// failure of the post-commit delete leaves an orphaned secret under the old name
// (reported via Warning) but the rename itself has succeeded.
//
// A key whose backend can't be opened or read here (unknown to this build, or an
// unavailable OS keychain) cannot be renamed — moving its secret would orphan it
// under the old name — so this fails loudly, the same stance as a non-forced
// Remove. There is deliberately no --force: use `key remove --force` for a
// stranded key.
func (m *Manager) Rename(oldName, newName string) (RenameResult, error) {
	if err := assertName(newName); err != nil {
		return RenameResult{}, err
	}
	if oldName == newName {
		return RenameResult{}, output.Usagef("the new name is the same as the current name %q", oldName)
	}
	unlock, err := m.lock()
	if err != nil {
		return RenameResult{}, err
	}
	defer unlock()
	file, err := m.load()
	if err != nil {
		return RenameResult{}, err
	}
	rec, err := must(file, oldName)
	if err != nil {
		return RenameResult{}, err
	}
	// A sandbox key must keep advertising the sandbox in its name (see
	// assertSandboxNaming) — renaming it to a name that drops the token would
	// let it be used without "sandbox" on the command line.
	if rec.IsSandbox() && !nameAdvertisesSandbox(newName) {
		return RenameResult{}, output.Usagef(
			"key %q is a sandbox key, so its name must contain %q to keep its use explicit on the command line; %q does not — choose a name containing %q", oldName, SandboxNameToken, newName, SandboxNameToken)
	}
	if _, exists := file.Keys[newName]; exists {
		return RenameResult{}, output.Usagef(
			"key %q already exists — pick another name, or remove it first with `%s key remove %s`", newName, progname.Name(), newName)
	}

	// Move the secret within the key's own backend, copy→verify BEFORE the
	// registry commit so a failure can't strand a half-renamed key. The backend
	// must be reachable: an unopenable/unreadable backend would leave the secret
	// orphaned under the old name.
	store, err := m.storeFor(oldName, rec)
	if err != nil {
		return RenameResult{}, output.Configf(
			"cannot rename key %q: its keystore backend (%s) can't be opened to move the secret (%v) — to discard a stranded key use `%s key remove %s --force`", oldName, rec.Keystore, err, progname.Name(), oldName)
	}
	if err := m.probe(rec.Keystore); err != nil {
		return RenameResult{}, output.Configf(
			"cannot rename key %q: its keystore backend (%s) is not available here (%v) — to discard a stranded key use `%s key remove %s --force`", oldName, rec.Keystore, err, progname.Name(), oldName)
	}
	pem, err := store.Get(oldName)
	if err != nil {
		return RenameResult{}, output.Configf(
			"cannot rename key %q: reading its secret from the %s keystore failed (%v)", oldName, rec.Keystore, err)
	}
	secretMoved := false
	if pem != "" {
		if err := store.Set(newName, pem); err != nil {
			return RenameResult{}, output.Configf(
				"cannot rename key %q: writing its secret under the new name failed (%v) — left unchanged", oldName, err)
		}
		// Verify the read-back before committing — the same guarantee Migrate gives.
		if got, gerr := store.Get(newName); gerr != nil || got != pem {
			_ = store.Delete(newName) // roll back the in-flight copy
			return RenameResult{}, output.Configf(
				"cannot rename key %q: its secret did not read back correctly under the new name — left unchanged", oldName)
		}
		secretMoved = true
	}

	// Commit: move the record to the new name and follow the default if it pointed
	// here (same key/account, so not the forbidden silent re-point).
	delete(file.Keys, oldName)
	file.Keys[newName] = rec
	isDefault := false
	if file.DefaultKey != nil && *file.DefaultKey == oldName {
		file.DefaultKey = &newName
		isDefault = true
	}
	if err := m.save(file); err != nil {
		if secretMoved {
			_ = store.Delete(newName) // roll back: no record now points at newName
		}
		return RenameResult{}, err
	}

	// Post-commit: drop the old vault entry. A failure here is non-fatal — the
	// rename already committed; the old secret is just orphaned under the old name.
	var warning string
	if secretMoved {
		if err := store.Delete(oldName); err != nil {
			m.log().Warn("rename committed, old secret left in backend", "from", oldName, "to", newName, "backend", rec.Keystore, "err", err)
			warning = fmt.Sprintf(
				"key renamed to %q, but its old secret could not be deleted from the %s keystore (%v) — remove the %q entry there manually", newName, rec.Keystore, err, oldName)
		}
	}
	m.log().Info("key renamed", "from", oldName, "to", newName, "backend", rec.Keystore, "secretMoved", secretMoved, "isDefault", isDefault)
	return RenameResult{OldName: oldName, NewName: newName, IsDefault: isDefault, SecretMoved: secretMoved, Warning: warning}, nil
}

// SignerFor builds the signer for a key from its stored private material, WITHOUT
// requiring an API key id binding. It backs the pre-bind check interactive setup
// runs: signing a candidate id's whoami before persisting the binding. Resolve
// is the bound-key path used for real calls; this is signing material only.
func (m *Manager) SignerFor(name string) (korbit.Signer, error) {
	file, err := m.load()
	if err != nil {
		return nil, err
	}
	rec, err := must(file, name)
	if err != nil {
		return nil, err
	}
	if err := assertSupportedType(name, rec); err != nil {
		return nil, err
	}
	store, err := m.storeFor(name, rec)
	if err != nil {
		return nil, err
	}
	secret, err := store.Get(name)
	if err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, output.Configf(
			"private key for %q was not found in the %s keystore — re-import the key", name, rec.Keystore)
	}
	return signerFor(name, rec.Type, secret)
}

// Resolve returns the credential to sign with: the explicit name if given, else
// the default. It fails loudly (never falls back) when anything is missing.
func (m *Manager) Resolve(explicit string) (Resolved, error) {
	file, err := m.load()
	if err != nil {
		return Resolved{}, err
	}
	name := explicit
	if name == "" && file.DefaultKey != nil {
		name = *file.DefaultKey
	}
	if name == "" {
		return Resolved{}, output.Configf(
			"no key selected — pass --key <name> or set a default with `%s key use <name>` (create one with `%s setup`)", progname.Name(), progname.Name())
	}
	rec, ok := file.Keys[name]
	if !ok {
		return Resolved{}, output.Configf("unknown key %q — run `%s key list`", name, progname.Name())
	}
	if err := assertSupportedType(name, rec); err != nil {
		return Resolved{}, err
	}
	if rec.APIKeyID == nil {
		return Resolved{}, output.Configf(
			"key %q has no API key id bound yet — register its public key at https://developers.korbit.co.kr, then run: %s key bind %s --api-key <KEY_ID>", name, progname.Name(), name)
	}
	store, err := m.storeFor(name, rec)
	if err != nil {
		return Resolved{}, err
	}
	secret, err := store.Get(name)
	if err != nil {
		return Resolved{}, err
	}
	if secret == "" {
		m.log().Warn("resolve: no material in record's backend", "key", name, "backend", rec.Keystore)
		return Resolved{}, output.Configf(
			"private key for %q was not found in the %s keystore its record points at — run `%s keystore status`, then `%s keystore migrate <backend-that-holds-it> %s` to re-point it (or re-import the key)", name, rec.Keystore, progname.Name(), progname.Name(), name)
	}
	// Build the signer HERE via the single per-type seam, so no exported key
	// material ever leaves this package. Material that doesn't fit the key's type
	// is a CONFIG problem (the vault holds garbage for this key), not a usage one
	// — signerFor returns a ConfigError (exit 4). Add the recovery hint the
	// missing-material message above uses.
	signer, err := signerFor(name, rec.Type, secret)
	if err != nil {
		return Resolved{}, output.Configf(
			"%v — re-import the key (`%s key remove %s` then re-add it)", err, progname.Name(), name)
	}
	m.log().Debug("resolve: signer built", "key", name, "type", rec.Type, "backend", rec.Keystore, "apiKeyId", *rec.APIKeyID)
	return Resolved{Name: name, Type: rec.Type, Keystore: rec.Keystore, APIKeyID: *rec.APIKeyID, signer: signer}, nil
}
