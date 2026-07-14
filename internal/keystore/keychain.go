// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"log/slog"
	"sync"

	"github.com/korbit-official/korbit-cli/internal/logging"
	"github.com/korbit-official/korbit-cli/internal/output"
)

// keychainService is the service name under which every key's secret is stored
// in the OS keychain. Each key occupies one generic-password item keyed by this
// service plus the key name as the account.
const keychainService = "korbit-cli"

// probeAccount is a sentinel account used only by the read-only reachability
// probe. It is never written, so looking it up returns "not found" on a healthy
// keychain without prompting or mutating anything.
const probeAccount = "__korbit_cli_probe__"

// keychainProvider is the OS-keychain operation set the "keychain" backend
// needs. The platform build files install the real provider in their init():
// the macOS one calls Security.framework directly (so items bind to this
// binary's code signature, not a helper tool's), and the Linux/Windows one uses
// the OS keyring library. Tests swap in an in-memory provider via MockKeychain.
//
// get returns (secret, found, err): found is false for a plain miss, and a
// non-nil err is reserved for a real backend failure — the same "miss is not an
// error" contract the Keystore interface exposes.
type keychainProvider interface {
	set(account, secret string) error
	get(account string) (secret string, found bool, err error)
	del(account string) error
	// probe is the read-only reachability check behind ProbeKeyring. It must
	// not write and must not trigger an interactive OS prompt. A nil return
	// means the keychain is usable.
	probe() error
}

// keychain is the active provider, installed by the platform build file's
// init() and replaceable by the test mocks. It is process-global to mirror the
// OS keychain itself being a single per-user store.
var keychain keychainProvider

// KeyringKeystore stores private keys in the OS keychain (macOS Keychain, Linux
// Secret Service, Windows Credential Manager), giving OS-enforced at-rest
// protection. On macOS the item is created through Security.framework by this
// signed binary, so its access-control list binds to this tool's code
// signature: this CLI (and same-Developer-ID builds) reads it without a prompt,
// while any other program triggers the system's keychain-consent dialog.
type KeyringKeystore struct {
	// Log is an optional operational logger (nil = silent). It records each
	// Set/Get/Delete against the OS keychain — the service + account IDENTIFIER
	// and, on failure, the provider's error (which carries the macOS OSStatus,
	// e.g. errSecItemNotFound / a consent denial) — but NEVER the returned secret
	// bytes. Set by keystore.ByName at construction; tests may set it directly.
	Log *slog.Logger
}

// NewKeyring returns an OS-keychain-backed keystore.
func NewKeyring() *KeyringKeystore { return &KeyringKeystore{} }

func (k *KeyringKeystore) Backend() string { return "keychain" }

// log returns the wired logger or a no-op one, so call sites log unconditionally.
func (k *KeyringKeystore) log() *slog.Logger { return logging.Or(k.Log) }

// account maps a key name to the keychain account under which its secret is
// stored. The "key:" prefix namespaces this CLI's entries within the service,
// and is the account the portable OS-keyring library uses too — so a key stored
// by either the native macOS path or the go-keyring path is found by the other,
// on every platform.
func account(name string) string { return "key:" + name }

func (k *KeyringKeystore) Set(name, secret string) error {
	acct := account(name)
	k.log().Debug("keychain set", "backend", "keychain", "service", keychainService, "account", acct)
	if err := keychain.set(acct, secret); err != nil {
		k.log().Warn("keychain set failed", "backend", "keychain", "service", keychainService, "account", acct, "err", err)
		return output.Configf("failed to store the key in the OS keychain: %v", err)
	}
	return nil
}

func (k *KeyringKeystore) Get(name string) (string, error) {
	acct := account(name)
	k.log().Debug("keychain get", "backend", "keychain", "service", keychainService, "account", acct)
	secret, found, err := keychain.get(acct)
	if err != nil {
		k.log().Warn("keychain get failed", "backend", "keychain", "service", keychainService, "account", acct, "err", err)
		return "", output.Configf("failed to read the key from the OS keychain: %v", err)
	}
	if !found {
		k.log().Debug("keychain get: not found", "backend", "keychain", "service", keychainService, "account", acct)
		return "", nil
	}
	// Found: log only that an item was returned, never its bytes.
	k.log().Debug("keychain get: found", "backend", "keychain", "service", keychainService, "account", acct)
	return secret, nil
}

func (k *KeyringKeystore) Delete(name string) error {
	acct := account(name)
	k.log().Debug("keychain delete", "backend", "keychain", "service", keychainService, "account", acct)
	if err := keychain.del(acct); err != nil {
		k.log().Warn("keychain delete failed", "backend", "keychain", "service", keychainService, "account", acct, "err", err)
		return output.Configf("failed to delete the key from the OS keychain: %v", err)
	}
	return nil
}

// ProbeKeyring checks that the OS keychain is reachable, without prompting and
// without writing anything: it issues a read for a sentinel account. A "not
// found" result means the keychain is present and working (the entry simply
// doesn't exist); any other error means the backend itself is unavailable (no
// Secret Service / DBus on a headless Linux box, a locked store, etc.).
// Actually storing a key still happens (and is re-verified) during migration,
// where a prompt or write failure surfaces loudly — so this probe never
// gratuitously pops a keychain dialog.
//
// "Available" therefore means *reachable for read*, not *guaranteed writable*:
// a keychain that reads fine but rejects a write still passes the probe, and the
// write failure is caught and rolled back by migration instead. That keeps
// `keystore status`/`doctor` prompt-free.
func ProbeKeyring() error {
	if err := keychain.probe(); err != nil {
		return output.Configf(
			"the OS keychain is not available on this machine (%v) — keep the default `file` keystore, or run this on a host with a working keychain", err)
	}
	return nil
}

// memKeychain is an in-memory keychainProvider for tests, swapped in by
// MockKeychain / MockKeychainUnavailable so the suite never touches a real OS
// keychain (and CI hosts with no keyring still run green).
type memKeychain struct {
	mu       sync.Mutex
	entries  map[string]string
	probeErr error // non-nil makes probe() (and thus the backend) report unavailable
}

func (k *memKeychain) set(account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries[account] = secret
	return nil
}

func (k *memKeychain) get(account string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	secret, ok := k.entries[account]
	return secret, ok, nil
}

func (k *memKeychain) del(account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, account)
	return nil
}

func (k *memKeychain) probe() error { return k.probeErr }

// MockKeychain replaces the OS keychain with a fresh in-memory store for the
// duration of a test. It is the analogue of the real backend being present and
// empty. Call it before exercising the "keychain" backend so the test never
// reads or writes the developer's real keychain.
func MockKeychain() { keychain = &memKeychain{entries: map[string]string{}} }

// MockKeychainUnavailable replaces the OS keychain with an in-memory store whose
// reachability probe fails with err, simulating a host where the keychain
// backend cannot be used (e.g. a headless Linux box with no Secret Service).
func MockKeychainUnavailable(err error) {
	keychain = &memKeychain{entries: map[string]string{}, probeErr: err}
}
