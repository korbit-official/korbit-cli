// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package selfupdate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// manifestFileName is the manifest's filename under the CLI home. It is named
// here rather than inline so the home diagnosis, which must recognize a home
// holding nothing but this file, cannot drift from where the manifest is
// written.
const manifestFileName = "install.json"

// Manifest is the durable record of a managed install, written to
// <home>/install.json by self install / self update. Its presence (plus the
// running binary being the installed binary on PATH) is what gates self update to
// managed installs; when it is absent the install was done some other way
// (Homebrew, go install, a hand-downloaded archive) and self update refuses with
// method-specific guidance.
type Manifest struct {
	Method      string `json:"method"` // MethodManagedScript
	Executable  string `json:"executable"`
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	SHA256      string `json:"sha256"`
	Repo        string `json:"repo"`
	InstalledAt string `json:"installedAt"` // unix-ms as a string
	// Aliases are the extra command names in the install dir that run the same
	// binary (see Layout.LegacyBinName). Absent on an install that has none, so
	// self doctor treats a listed-but-missing alias as a broken install and
	// self uninstall knows exactly which extra names to remove.
	Aliases []string `json:"aliases,omitempty"`
}

// loadManifest reads and parses the manifest at path. found is false (with a nil
// error) when the file is simply absent; a present-but-corrupt file returns
// found=true with a parse error, so a caller (self install) can distinguish
// "fresh machine" from "rebuild the broken manifest".
func loadManifest(path string) (m Manifest, found bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, true, err
	}
	return m, true, nil
}

// save writes the manifest to path atomically (temp file + rename), creating the
// home directory 0700 if needed. It holds no secrets but is written 0600 to
// match keys.json / config.json.
func (m Manifest) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeBytesAtomic(path, append(out, '\n'), 0o600)
}
