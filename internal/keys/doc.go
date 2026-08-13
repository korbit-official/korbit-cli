// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package keys manages named API keys: the registry (keys.json) and the
// lifecycle commands over it (add/bind/use/remove/rename/set-base-url/resolve).
//
// # The two files
//
// Key management is split between a registry and vaults, with one rule:
//
//   - keys.json (THIS package) is the registry — the single source of truth
//     for which keys exist and everything non-secret about them: type,
//     keystore backend, public key, the bound API key id, the default-key
//     selection, an optional per-key base URL. No secret ever lives here.
//   - A keystore backend (package keystore) is a vault — a dumb name→secret
//     store. keystore.json is just the FILE backend's vault; when a key uses
//     the keychain backend its secret lives in the OS keyring instead, under
//     the same name. Vaults hold no metadata and cannot enumerate keys.
//
// Everything routes through the registry: to enumerate keys, to find where a
// key's secret lives, to decide how it signs. Nothing ever scans a vault.
//
// # The two per-key axes: `type` and `keystore`
//
// Every record carries two orthogonal fields:
//
//   - type — the signing scheme: what kind of secret the vault holds and how
//     requests are signed with it ("ed25519" or "hmac-sha256").
//   - keystore — which backend holds this key's secret. Per key so that keys
//     with different security needs coexist: an order-placing key in the OS
//     keychain, read-only keys in the file vault. config.json's `keystore` is
//     only the default for NEWLY CREATED keys; changing it never re-points an
//     existing record. Records are re-pointed only by `keystore migrate`,
//     which moves and verifies the material first (Manager.SetKeystoreBackend
//     is its commit hook, not a casual setter).
//
// Both fields are MANDATORY — a record missing either fails to load.
// Their VALUES are tolerant on load and strict at use: a record written by a
// newer CLI (unknown type or backend name) still LISTS fine, and only an
// operation that must interpret it fails — signing an unknown type, or
// migrating a key out of an unknown backend (which this build can't read; a
// `keystore migrate --all` skips it and moves the rest, naming it errors).
// One forward-version key must never brick the registry for the rest.
//
// # Invariants
//
//   - A key's secret is expected ONLY in the backend its record names
//     (`keystore migrate --keep-source` copies are explicitly
//     non-authoritative). Resolve reads exactly there — no fallback probing of
//     other backends.
//   - Keys may belong to different Korbit accounts, so nothing here ever falls
//     back from one key to another: the default is set only explicitly (or on
//     first creation), and removing the default clears it rather than silently
//     re-pointing at another account. This holds for forced removal too (see
//     below) — a stranded default is unset, never re-pointed.
//   - Remove deletes the secret then the record on the happy path; a key whose
//     backend is unknown to this build or unavailable (or whose Delete errors)
//     can't have its secret deleted, so a plain remove fails loudly and points
//     at `key remove --force`. Force removes the record anyway (best-effort
//     secret delete) so a forward-version or headless-stranded key can never
//     squat in the registry with no escape hatch; it reports the orphaned
//     secret and the backend it was left in (RemoveResult.SecretRemoved /
//     Warning) rather than pretending the secret is gone.
//   - keys.json is written atomically (temp file + rename): it is the commit
//     point of a keystore migration, so a crash must not truncate it.
//   - Concurrency contract: every MUTATING method (Add, Bind, Use,
//     SetBaseURL, ClearBaseURL, SetKeystoreBackend, Remove, Rename) holds
//     an exclusive cross-process advisory lock (keys.json.lock, via
//     internal/fslock) across its whole load → modify → atomic-rename cycle, so
//     two concurrent korbit-cli processes (a long-running monitor bot plus
//     ad-hoc commands) can't last-writer-wins each other's update. READ-ONLY
//     methods (List, Show, Names, Resolve, DefaultKeyName, BaseURLOf,
//     MetaBaseURL) are deliberately LOCK-FREE: the atomic rename guarantees
//     they observe a complete, self-consistent snapshot.
//   - Read cache: the read-only metadata peeks (List, Names, DefaultKeyName and
//     the Meta* / MetaForSelection accessors) parse keys.json once per Manager
//     and reuse it, so a single command — or a long-running mcp/tui/monitor
//     session holding one Manager — doesn't re-read the file on every peek.
//     Mutations bypass the cache (they load fresh under the registry lock) and
//     invalidate it on save; Invalidate() drops it after an out-of-band
//     in-process change (the mcp setup tool calls it). The trade-off:
//     within one process a peek may observe an earlier — still complete and
//     self-consistent — snapshot than a concurrent OTHER process's write, until
//     the next invalidation. That is fine for these best-effort metadata peeks,
//     and a long-running session resolves its signer once anyway.
//   - Lock ordering — registry → vault, NEVER the reverse. The mutating
//     methods that drive the file vault (Add, Remove, Rename) hold the
//     registry lock (keys.json.lock) WHILE calling keystore.FileKeystore.Set/Delete,
//     which take the SEPARATE vault lock (keystore.json.lock). That nesting is
//     deadlock-free only because the two are different lock files (flock and
//     LockFileEx contend even between distinct fds within one process, so a
//     single shared lock would self-deadlock). Nothing in the vault layer may
//     ever acquire this registry lock.
//   - keystore.Migrate is NOT whole-run atomic: it is a sequence of
//     individually-locked steps (each dst.Set takes the vault lock, each commit
//     via SetKeystoreBackend takes the registry lock, both briefly). What is
//     preserved is per-key consistency — the copy → verify → commit → delete
//     contract — not atomicity across the whole run.
//   - Resolved exposes NO raw key material: Resolve parses the stored PEM into a
//     signer eagerly and keeps it behind the unexported Resolved.signer field;
//     the only way to obtain it is Resolved.Signer(), the single seam between
//     stored material and a usable signer. A PEM that fails to parse is reported
//     as a config problem (ConfigError, exit 4) by Resolve, not deferred to a
//     caller's UsageError. The two-value Signer() signature lets the per-`type`
//     switch resolve stored material into a signer — failing cleanly on a type
//     it can't build — so no consumer ever holds the secret.
//     Resolved is also fmt-safe: it implements fmt.Formatter to redact the
//     signer under every verb, because unexporting alone does not stop fmt from
//     dumping unexported fields (and an ed25519 key) via reflection under
//     %+v/%#v.
package keys
