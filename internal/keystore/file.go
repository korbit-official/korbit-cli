// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/fslock"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/digitalx-official/digitalx-cli/internal/progname"
)

// The file backend protects keys from CASUAL disclosure (grep, log scrapers,
// careless backups) — not from an attacker who has both the keystore file and
// this CLI, because the encryption key below intentionally ships inside the
// CLI. For OS-enforced at-rest protection, switch to the keychain backend with
// `dgx-cli keystore migrate keychain` (which moves existing keys for you).
//
// The seed string is an OPAQUE v1 domain constant, not a name to keep in step
// with the product: it derives the AES key every existing keystore.json was
// encrypted under, so changing a single byte of it makes every stored key
// undecryptable. Leave it exactly as it is.
var obfuscationKey = sha256.Sum256([]byte("korbit-cli/keystore/v1:embedded-obfuscation-key:7f3a9d52c8e14b06"))

// FileKeystore encrypts each secret with AES-256-GCM and stores it in
// keystore.json (mode 0600).
type FileKeystore struct {
	home string
	path string
	// Log is an optional operational logger (nil = silent). It records the
	// lock/read/atomic-write steps and decrypt failures — never the secret or
	// ciphertext. Set by keystore.ByName at construction; tests may set it
	// directly.
	Log *slog.Logger
}

// FilePath is the file-backed keystore vault path under home (keystore.json).
func FilePath(home string) string { return filepath.Join(home, "keystore.json") }

// NewFile returns a file-backed keystore rooted at home.
func NewFile(home string) *FileKeystore {
	return &FileKeystore{home: home, path: FilePath(home)}
}

func (k *FileKeystore) Backend() string { return "file" }

// log returns the wired logger or a no-op one, so call sites log unconditionally.
func (k *FileKeystore) log() *slog.Logger { return logging.Or(k.Log) }

type storedEntry struct {
	IV   string `json:"iv"`
	Tag  string `json:"tag"`
	Data string `json:"data"`
}

type storeFile struct {
	Version int                    `json:"version"`
	Entries map[string]storedEntry `json:"entries"`
}

// aad binds each ciphertext to its key name, so entries cannot be copied or
// swapped between names without failing authentication.
//
// The "korbit-cli:" prefix is an OPAQUE v1 domain constant, not a name to keep
// in step with the product: it is authenticated data covering every entry ever
// written, so changing a single byte of it makes every existing keystore.json
// fail authentication and every stored key unreadable. Leave it exactly as it
// is.
func aad(name string) []byte { return []byte("korbit-cli:" + name) }

func (k *FileKeystore) load() (storeFile, error) {
	raw, err := os.ReadFile(k.path)
	if errors.Is(err, fs.ErrNotExist) {
		return storeFile{Version: 1, Entries: map[string]storedEntry{}}, nil
	}
	if err != nil {
		return storeFile{}, err
	}
	// This file holds (obfuscated) private keys. If it somehow became
	// group/other-accessible, tighten it back to 0600 on read (best effort).
	if info, statErr := os.Stat(k.path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(k.path, 0o600)
	}
	var file storeFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return storeFile{}, output.Configf("%s is corrupted (not valid JSON)", k.path)
	}
	if file.Version != 1 || file.Entries == nil {
		return storeFile{}, output.Configf("%s has an unsupported format (expected version 1)", k.path)
	}
	return file, nil
}

// save writes the vault atomically (temp file + rename): every Set rewrites
// the whole file, so a crash mid-write must not be able to truncate the one
// file holding every file-backed key. The fixed ".tmp" sidecar name is safe to
// share only because Set/Delete hold the vault lock (see lock) — that
// serializes all writers, so two of them can never race on the same temp path.
func (k *FileKeystore) save(file storeFile) error {
	if err := os.MkdirAll(k.home, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	k.log().Debug("vault write: temp written, renaming", "backend", "file", "tmp", tmp, "path", k.path, "entries", len(file.Entries))
	if err := os.Rename(tmp, k.path); err != nil {
		return err
	}
	k.log().Debug("vault write: committed", "backend", "file", "path", k.path)
	return nil
}

// lock takes the exclusive cross-process VAULT lock (keystore.json.lock),
// serializing Set/Delete's whole load → modify → atomic-rename cycle. Because
// every Set rewrites the ENTIRE vault, two concurrent unlocked Sets would each
// load the same snapshot and the second rename would silently drop the first's
// key — losing a freshly-added key's PRIVATE material while its registry record
// survives (unrecoverable; forces a portal rotation). Get does NOT lock: the
// atomic rename guarantees a reader sees a complete, consistent file.
//
// LOCK ORDERING: this vault lock is acquired AFTER the registry lock when a
// keys.Manager mutation (Add/Remove/Rename) drives a Set/Delete. It is a
// DIFFERENT file from keys.json.lock, which is the only reason that nesting is
// deadlock-free (flock/LockFileEx contend even between distinct fds in one
// process, so a single shared lock would self-deadlock). The vault layer must
// NEVER acquire the registry lock — order is registry → vault, never the
// reverse. See doc.go.
func (k *FileKeystore) lock() (func(), error) {
	lockPath := k.path + ".lock"
	k.log().Debug("vault lock: acquiring", "backend", "file", "lock", lockPath)
	start := time.Now()
	unlock, err := fslock.Lock(lockPath)
	if err != nil {
		// A lock acquisition failure is returned to the caller as an error; log it
		// at Warn (not Error) as a diagnostic, since the caller surfaces it.
		k.log().Warn("vault lock: acquire failed", "backend", "file", "lock", lockPath, "err", err)
		return nil, err
	}
	waited := time.Since(start)
	// A non-trivial wait means another process held the lock — high-value
	// contention signal. A fast acquire is routine Debug.
	if waited > 50*time.Millisecond {
		k.log().Warn("vault lock: acquired after contention", "backend", "file", "lock", lockPath, "waitedMs", waited.Milliseconds())
	} else {
		k.log().Debug("vault lock: acquired", "backend", "file", "lock", lockPath, "waitedMs", waited.Milliseconds())
	}
	return func() {
		unlock()
		k.log().Debug("vault lock: released", "backend", "file", "lock", lockPath)
	}, nil
}

func (k *FileKeystore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(obfuscationKey[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (k *FileKeystore) Set(name, secret string) error {
	k.log().Debug("vault set", "backend", "file", "key", name)
	unlock, err := k.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := k.load()
	if err != nil {
		return err
	}
	gcm, err := k.gcm()
	if err != nil {
		return err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	sealed := gcm.Seal(nil, iv, []byte(secret), aad(name))
	// Split the trailing GCM tag from the ciphertext so the on-disk shape keeps
	// iv/tag/data as separate fields.
	tagLen := gcm.Overhead()
	data, tag := sealed[:len(sealed)-tagLen], sealed[len(sealed)-tagLen:]
	file.Entries[name] = storedEntry{
		IV:   base64.StdEncoding.EncodeToString(iv),
		Tag:  base64.StdEncoding.EncodeToString(tag),
		Data: base64.StdEncoding.EncodeToString(data),
	}
	return k.save(file)
}

func (k *FileKeystore) Get(name string) (string, error) {
	file, err := k.load()
	if err != nil {
		// A load error here is corruption (bad JSON / unsupported format), already
		// returned to the caller; record the path at Warn so a --debug run names
		// the broken file without echoing its contents.
		k.log().Warn("vault read failed", "backend", "file", "path", k.path, "err", err)
		return "", err
	}
	entry, ok := file.Entries[name]
	if !ok {
		k.log().Debug("vault read: no entry", "backend", "file", "key", name)
		return "", nil
	}
	iv, _ := base64.StdEncoding.DecodeString(entry.IV)
	tag, _ := base64.StdEncoding.DecodeString(entry.Tag)
	data, _ := base64.StdEncoding.DecodeString(entry.Data)
	gcm, err := k.gcm()
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(data, tag...), aad(name))
	if err != nil {
		// Auth-tag/decrypt failure: a corrupt vault entry. Log the key name and
		// vault path (NOT the ciphertext, IV, tag, or any key material) at Warn so
		// the failure is diagnosable; the underlying err is generic GCM and carries
		// no secret.
		k.log().Warn("vault decrypt failed (corrupt entry)", "backend", "file", "key", name, "path", k.path)
		return "", output.Configf(
			"keystore entry for %q is corrupted and cannot be decrypted — remove it with `%s key remove %s` and set the key up again",
			name, progname.Name(), name)
	}
	k.log().Debug("vault read: entry decrypted", "backend", "file", "key", name)
	return string(plain), nil
}

func (k *FileKeystore) Delete(name string) error {
	k.log().Debug("vault delete", "backend", "file", "key", name)
	unlock, err := k.lock()
	if err != nil {
		return err
	}
	defer unlock()
	file, err := k.load()
	if err != nil {
		return err
	}
	if _, ok := file.Entries[name]; ok {
		delete(file.Entries, name)
		return k.save(file)
	}
	k.log().Debug("vault delete: no entry", "backend", "file", "key", name)
	return nil
}
