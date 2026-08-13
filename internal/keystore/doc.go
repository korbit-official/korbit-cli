// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package keystore stores private key material, addressed by key name.
//
// # Role in the key-management design
//
// Key management is split across two layers with one rule: keys.json (the
// registry, owned by package keys) is the single source of truth for WHICH
// keys exist and WHERE each key's secret lives; a keystore backend is a dumb
// vault that answers "what is the secret stored under this name". A backend
// holds no metadata, no default selection, and no routing information — it
// cannot, because not every backend is a file this CLI controls (the OS
// keyring has no enumeration API at all). Anything that needs to know what
// keys exist must ask the registry, never a vault.
//
// # Backends
//
// Two backends, constructed by name via ByName:
//
//   - "file" — keystore.json under the CLI home, each secret sealed with
//     AES-256-GCM. The encryption key is embedded in the binary BY DESIGN and
//     honestly documented as casual-disclosure protection only (see file.go).
//     It is the default for new keys because it needs no UI and works on
//     headless hosts.
//   - "keychain" — the OS-native keyring (macOS Keychain, Windows Credential
//     Manager, Linux Secret Service). Opt-in, OS-enforced at-rest protection,
//     may prompt interactively. macOS is served by a direct Security.framework
//     integration (keychain_darwin.go, via purego — no cgo, so cross-compiles
//     are unaffected) so the stored item binds to this signed binary's code
//     signature rather than a helper tool's; Windows and Linux use
//     zalando/go-keyring (keychain_other.go), which calls the Credential Manager
//     and the Secret Service natively in pure Go. The active provider is the
//     package var keychain, swapped to an in-memory fake by MockKeychain in
//     tests.
//
// The backend choice is PER KEY: each registry record names the backend that
// holds its secret, so e.g. an order-placing key can sit in the OS keychain
// while read-only keys stay in the file vault. config.json's "keystore" value
// is only the default applied to newly created keys — changing it never moves
// or re-routes an existing key.
//
// A backend stores opaque secret strings — an ED25519 private-key PEM or an
// HMAC shared secret. The vault does not know or care which; the registry's
// per-key "type" field owns what the secret means. That keeps Migrate
// type-agnostic.
//
// # Contracts to preserve
//
//   - Get returns "" for a plain miss; a non-nil error always means corruption
//     or a backend failure. Callers rely on this to tell "no such key" from
//     "the vault is broken".
//   - Concurrency contract (file backend): Set and Delete hold an exclusive
//     cross-process advisory lock (keystore.json.lock, via internal/fslock)
//     across their whole load → modify → atomic-rename cycle. Because every Set
//     rewrites the ENTIRE vault, two concurrent unlocked Sets would each load
//     the same snapshot and the second rename would silently drop the first's
//     key — losing freshly-added PRIVATE material while its registry record
//     survives. Get is LOCK-FREE: the atomic rename guarantees a reader sees a
//     complete, consistent file. The keychain backend needs no lock — the OS
//     keyring is its own serialization domain.
//   - Lock ordering — registry → vault, NEVER the reverse. The vault lock is
//     acquired AFTER the registry lock when a keys.Manager mutation
//     (Add/Remove/Rename) drives Set/Delete while holding keys.json.lock. The
//     two are DIFFERENT lock files, which is the only reason that nesting is
//     deadlock-free; this layer must NEVER acquire the registry lock.
//   - Migrate is NOT whole-run atomic. It is a sequence of individually-locked
//     steps (each dst.Set takes the vault lock, each commit takes the registry
//     lock, both briefly). Per-key consistency — the copy → verify → commit →
//     delete ordering below — is what is preserved, not atomicity across the
//     whole run.
//   - Available (and the keychain ProbeKeyring) is a READ-ONLY reachability
//     probe: it must never write, and must never trigger an interactive OS
//     prompt — `keystore status` and doctor call it freely.
//   - Migrate is the only code that moves secrets between backends. Its
//     per-key ordering is the safety contract: copy into the target → verify
//     the read-back → commit (the caller stamps the key's registry record) →
//     only then delete from the source. A failure rolls the in-flight key's
//     target entry back and aborts, so every key is always in exactly one
//     consistent state; keys committed before the failure stay migrated
//     (re-running continues where it stopped).
//   - Secrets never appear in errors, logs, or reports — Migrate's report
//     carries key names only.
//
// To add a backend, add ONE entry to the internal registry in keystore.go (a
// constructor and a read-only availability probe — a nil probe means
// always-available, as for "file") and the matching name to config.Backends.
// ByName and Available are plain lookups into that registry, so there are no
// twin switches to keep in sync, and keystore_test.go's drift guard fails
// loudly if the registry and config.Backends disagree — a backend added to one
// but not the other never ships. Two assumptions here bound that: Migrate
// assumes Set works on every backend, and the registry hands callers a PEM. A
// backend that can't accept imported material (a non-exportable hardware
// credential) or that signs on-device rather than divulging a secret breaks one
// of those — changing it means changing Migrate or the registry's signer
// handoff.
package keystore
