// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/korbit-official/korbit-cli/internal/keys"
)

// keyImportBackend is the keystore backend the seeded key is forced into:
// always "file" — headless/parallel-safe, no OS-keychain prompt, and
// throwaway-cleanable. The sandbox key never goes to the OS keychain.
const keyImportBackend = "file"

// importKey takes user 1's Ed25519 key (apiKey + the PKCS#8 private PEM in
// `secret`) structurally from an already-read `status --json` doc, imports it
// via keys.AddBound (born bound, so its SANDBOX_ id keeps it out of the default
// slot), and (re-)pins its base URL to the actual bound port. It is idempotent:
// an existing sandbox key keeps its material (re-pin only) unless Reimport.
//
// Returns the api-key id and whether the material was freshly imported.
func (m *Manager) importKey(doc statusDoc, port int) (apiKeyID string, imported bool, err error) {
	km := m.deps.KeyManager
	if km == nil {
		return "", false, fmt.Errorf("no key manager configured for sandbox key import")
	}

	apiKey, pem, err := seededKeyFrom(doc)
	if err != nil {
		return "", false, err
	}

	name := m.keyName()
	rest, ws := loopbackURLs(port)

	// Idempotent re-start: reuse an existing key only if it is a sandbox key
	// (never clobber a real key). Re-pin its port every start.
	if existing, serr := km.Show(name); serr == nil {
		if !existing.IsSandbox {
			return "", false, fmt.Errorf(
				"key %q already exists and is not a sandbox key — pass --key-name to pick another name (refusing to clobber a real key)", name)
		}
		if m.cfg.Reimport {
			// Replace material: remove then re-add bound. Force-removal is fine —
			// it's a sandbox key with throwaway material.
			if _, rerr := km.Remove(name, true); rerr != nil {
				return "", false, rerr
			}
			if _, aerr := km.AddBound(name, pem, apiKey, keyImportBackend); aerr != nil {
				return "", false, aerr
			}
			imported = true
		}
		// Always re-pin to the actual bound port (idempotent).
		if perr := km.SetBaseURL(name, rest, ws); perr != nil {
			return "", false, perr
		}
		return apiKey, imported, nil
	}

	// Fresh import: born bound (never transiently the default), then pin the port.
	if _, aerr := km.AddBound(name, pem, apiKey, keyImportBackend); aerr != nil {
		return "", false, aerr
	}
	if perr := km.SetBaseURL(name, rest, ws); perr != nil {
		return "", false, perr
	}
	return apiKey, true, nil
}

// readStatusDoc runs `status --json` against the managed db and parses the
// fields the CLI models (statusDoc). The read is structural — no text scraping.
// It is a bundle invocation on both of `start`'s paths (the paper-mode gate
// before bring-up, and the result build for a freshly-spawned OR reused server),
// so it self-heals a corrupt cached bundle exactly like the bring-up does.
func (m *Manager) readStatusDoc(ctx context.Context, rt Runtime, bundle string) (statusDoc, error) {
	var doc statusDoc
	err := m.withBundleRecovery(ctx, func() error {
		var rerr error
		doc, rerr = m.runStatusDoc(ctx, rt, bundle)
		return rerr
	})
	return doc, err
}

// runStatusDoc is the single `status --json` invocation behind readStatusDoc. A
// failure whose output shows the cached bundle is not JavaScript returns the
// typed *bundleCorruptError, so the caller's recovery refreshes and retries
// rather than surfacing an opaque "reading sandbox status" failure.
func (m *Manager) runStatusDoc(ctx context.Context, rt Runtime, bundle string) (statusDoc, error) {
	bin, argv, env, err := rt.command(ctx, m.denoRunPerms(), bundle, "status", "--db", m.dbPath(), "--json")
	if err != nil {
		return statusDoc{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, argv...)
	withRuntimeEnv(cmd, env)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		// Classify over both streams, like every other site (runInitDB, the run.log
		// tail, the banner): Deno puts the module-parse error on stderr, but nothing
		// here depends on that.
		if bc := bundleCorruptFrom(stdout.String() + stderr.String()); bc != nil {
			return statusDoc{}, bc
		}
		return statusDoc{}, fmt.Errorf("reading sandbox status: %w (%s)", rerr, bytes.TrimSpace(stderr.Bytes()))
	}
	var doc statusDoc
	if jerr := json.Unmarshal(stdout.Bytes(), &doc); jerr != nil {
		return statusDoc{}, fmt.Errorf("parsing sandbox `status --json`: %w", jerr)
	}
	return doc, nil
}

// seededKeyFrom extracts user 1's Ed25519 apiKey + private PEM from a parsed
// `status --json` doc.
func seededKeyFrom(doc statusDoc) (apiKey, pem string, err error) {
	if len(doc.Users) == 0 {
		return "", "", fmt.Errorf("sandbox status reported no users — is the database seeded? (init-db)")
	}
	for _, k := range doc.Users[0].Keys {
		if k.Type == keys.TypeEd25519 && k.Secret != "" {
			return k.APIKey, k.Secret, nil
		}
	}
	return "", "", fmt.Errorf("sandbox user 1 has no ed25519 key with a private PEM in `status --json`")
}
