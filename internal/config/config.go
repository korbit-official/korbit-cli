// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package config resolves the CLI home directory and loads config.json.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/digitalx-official/digitalx-cli/internal/envalias"
	"github.com/digitalx-official/digitalx-cli/internal/fslock"
	"github.com/digitalx-official/digitalx-cli/internal/logging"
	"github.com/digitalx-official/digitalx-cli/internal/output"
)

// Backends are the supported keystore backend names.
var Backends = []string{"file", "keychain"}

// ColorSchemes are the valid persisted tui color-scheme names. They mirror
// uikit.ColorScheme.String(); config keeps its own copy so it need not import
// the (charm-dependent) tui/uikit layer. A uikit test pins the two lists
// together so they cannot drift.
var ColorSchemes = []string{"green-red", "red-blue"}

// Order-size preset bounds. Each level is a percentage of the funding balance
// (1..100, where 100 = "max"); MaxOrderLevels caps the count at the number of
// digit keys (1..9) the order UIs can bind. The list is stored as a plain JSON
// array, so extending it later needs no schema change and an older binary reads
// a longer list without error.
const (
	MinOrderLevel  = 1
	MaxOrderLevel  = 100
	MaxOrderLevels = 9
)

// Config is the resolved CLI configuration.
type Config struct {
	// Keystore selects where NEWLY CREATED keys' private material goes: "file"
	// (default, encrypted file) or "keychain" (the OS keyring). It applies only
	// at key creation — every existing key carries its own backend in keys.json,
	// and changing this default never moves or re-routes an existing key (that
	// is `keystore migrate`'s job).
	Keystore string
	// BaseURL optionally overrides the REST base URL.
	BaseURL string
	// WSBaseURL optionally overrides the WebSocket base URL (scheme ws/wss),
	// used by the streaming `monitor` command. Empty means it is derived from
	// the resolved REST base URL.
	WSBaseURL string
	// TUIColorScheme is the persisted up/down color convention for the `tui`
	// command ("green-red" or "red-blue"), toggled with the C key and saved back
	// so it survives across launches. Stored under the "tui" object in
	// config.json. Empty means the TUI's default.
	TUIColorScheme string
	// TUIOrderLevels are the persisted %-of-balance size presets the `tui`
	// order UIs bind to number keys (e.g. [10,25,50,100], where 100 renders as
	// "max"). Stored under the "tui" object. Nil means unset — the tui command
	// pins the built-in defaults on first launch, so a later program update
	// never silently changes what a preset key does.
	TUIOrderLevels []int
}

var httpURL = regexp.MustCompile(`^https?://`)
var wsURL = regexp.MustCompile(`^wss?://`)

// EnvHome names the environment variable that relocates the CLI home. The
// legacy KORBIT_CLI_HOME spelling is accepted as a fallback (see envalias).
const EnvHome = "DIGITALX_CLI_HOME"

// DirName is the CLI home directory under the user's home; LegacyDirName is the
// directory an installation made under the earlier product name carries. Home
// keeps using an existing LegacyDirName rather than starting an empty new one
// beside it, so keys, config, and the journal stay where they already are.
const (
	DirName       = ".digitalx-cli"
	LegacyDirName = ".korbit-cli"
)

// Home returns the CLI home directory: $DIGITALX_CLI_HOME (else the legacy
// $KORBIT_CLI_HOME) when set; otherwise ~/.digitalx-cli, unless that does not
// exist and ~/.korbit-cli does, in which case the existing directory is used.
// The chosen directory need not exist — the write paths create it.
func Home(getenv func(string) string) string {
	if h := envalias.Lookup(getenv, EnvHome); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	current := filepath.Join(home, DirName)
	if isDir(current) {
		return current
	}
	if legacy := filepath.Join(home, LegacyDirName); isDir(legacy) {
		return legacy
	}
	return current
}

// isDir reports whether path exists and is a directory (a plain file by that
// name is not a CLI home).
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Path is the config.json path under home.
func Path(home string) string {
	return filepath.Join(home, "config.json")
}

// Load reads config.json from home. A missing file yields the defaults
// (file backend, no base URL). Malformed config is a ConfigError. log (nil =
// silent) records the path, whether the file was present, and a parse failure —
// config.json holds no secrets, so it is safe to name the path.
func Load(home string, log *slog.Logger) (Config, error) {
	l := logging.Or(log)
	path := Path(home)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		l.Debug("config load: not present, using defaults", "path", path)
		return Config{Keystore: "file"}, nil
	}
	if err != nil {
		l.Warn("config load: read failed", "path", path, "err", err)
		return Config{}, err
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		l.Warn("config load: parse failed", "path", path)
		return Config{}, output.Configf("%s is not a valid JSON object", path)
	}
	l.Debug("config load: parsed", "path", path)

	cfg := Config{Keystore: "file"}
	if v, ok := parsed["keystore"]; ok {
		var ks string
		if err := json.Unmarshal(v, &ks); err != nil || (ks != "file" && ks != "keychain") {
			return Config{}, output.Configf(
				`%s: "keystore" must be one of "file", "keychain"`, path)
		}
		cfg.Keystore = ks
	}
	if v, ok := parsed["baseUrl"]; ok {
		var u string
		if err := json.Unmarshal(v, &u); err != nil || !httpURL.MatchString(u) {
			return Config{}, output.Configf(`%s: "baseUrl" must be an http(s) URL`, path)
		}
		cfg.BaseURL = strings.TrimRight(u, "/")
	}
	if v, ok := parsed["wsBaseUrl"]; ok {
		var u string
		if err := json.Unmarshal(v, &u); err != nil || !wsURL.MatchString(u) {
			return Config{}, output.Configf(`%s: "wsBaseUrl" must be a ws(s) URL`, path)
		}
		cfg.WSBaseURL = strings.TrimRight(u, "/")
	}
	if v, ok := parsed["tui"]; ok {
		// The tui block is validated field-by-field and fails early like every
		// other field — a bad value is a ConfigError, not silently defaulted.
		// For orderLevels that is also the safe choice: falling back to the
		// built-in defaults on a bad edit could silently change what a size
		// preset key does, the exact surprise pinning exists to prevent.
		var tui map[string]json.RawMessage
		if err := json.Unmarshal(v, &tui); err != nil {
			return Config{}, output.Configf(`%s: "tui" must be a JSON object`, path)
		}
		if cv, ok := tui["colorScheme"]; ok {
			var s string
			if err := json.Unmarshal(cv, &s); err != nil || !ValidColorScheme(s) {
				return Config{}, output.Configf(`%s: "tui.colorScheme" must be one of: %s`, path, strings.Join(ColorSchemes, ", "))
			}
			cfg.TUIColorScheme = s
		}
		if lv, ok := tui["orderLevels"]; ok {
			var levels []int
			if err := json.Unmarshal(lv, &levels); err != nil || !ValidOrderLevels(levels) {
				return Config{}, output.Configf(
					`%s: "tui.orderLevels" must be 1-%d integers, each %d-%d (e.g. [10,25,50,100])`,
					path, MaxOrderLevels, MinOrderLevel, MaxOrderLevel)
			}
			cfg.TUIOrderLevels = levels
		}
	}
	return cfg, nil
}

// ValidColorScheme reports whether name is a recognized tui color-scheme value.
func ValidColorScheme(name string) bool {
	for _, s := range ColorSchemes {
		if s == name {
			return true
		}
	}
	return false
}

// ValidOrderLevels reports whether levels is an acceptable size-preset list:
// 1..MaxOrderLevels entries, each a percentage in MinOrderLevel..MaxOrderLevel.
func ValidOrderLevels(levels []int) bool {
	if len(levels) < 1 || len(levels) > MaxOrderLevels {
		return false
	}
	for _, p := range levels {
		if p < MinOrderLevel || p > MaxOrderLevel {
			return false
		}
	}
	return true
}

// ValidBackend reports whether name is a recognized keystore backend. It is the
// single home for the "is this a real backend" check — callers (the CLI command,
// the keystore constructors) should use it rather than re-listing Backends.
func ValidBackend(name string) bool {
	for _, b := range Backends {
		if b == name {
			return true
		}
	}
	return false
}

// SetKeystore persists the default-keystore-for-new-keys choice into
// config.json (the `keystore default` command), leaving every other field
// (baseUrl, and any field this version doesn't model) exactly as it was.
func SetKeystore(home, backend string, log *slog.Logger) error {
	if !ValidBackend(backend) {
		return output.Configf(`unknown keystore backend %q — one of: %s`, backend, strings.Join(Backends, ", "))
	}
	if err := mutateConfig(home, log, func(fields map[string]json.RawMessage) error {
		enc, err := json.Marshal(backend)
		if err != nil {
			return err
		}
		fields["keystore"] = enc
		return nil
	}); err != nil {
		return err
	}
	logging.Or(log).Info("config keystore default updated", "backend", backend, "path", Path(home))
	return nil
}

// SetTUIColorScheme persists the tui command's up/down color convention
// ("green-red" or "red-blue") under config.json's "tui" object, leaving every
// other field — top-level, and any other key inside "tui" — exactly as it was.
// The tui command calls it off the UI goroutine when the C key toggles the
// scheme.
func SetTUIColorScheme(home, scheme string, log *slog.Logger) error {
	if !ValidColorScheme(scheme) {
		return output.Configf(`unknown color scheme %q — one of: %s`, scheme, strings.Join(ColorSchemes, ", "))
	}
	return mutateConfig(home, log, func(fields map[string]json.RawMessage) error {
		return setTUIField(fields, "colorScheme", scheme)
	})
}

// SetTUIOrderLevels persists the tui command's %-of-balance size presets under
// config.json's "tui" object, leaving every other field alone. The tui command
// pins the built-in defaults here on first launch.
func SetTUIOrderLevels(home string, levels []int, log *slog.Logger) error {
	if !ValidOrderLevels(levels) {
		return output.Configf(
			"order levels must be 1-%d integers, each %d-%d", MaxOrderLevels, MinOrderLevel, MaxOrderLevel)
	}
	return mutateConfig(home, log, func(fields map[string]json.RawMessage) error {
		return setTUIField(fields, "orderLevels", levels)
	})
}

// setTUIField sets one key inside config.json's "tui" object, preserving the
// object's other keys. A "tui" value that isn't an object is replaced rather
// than propagated — keeping it would make this and every future write fail.
func setTUIField(fields map[string]json.RawMessage, key string, value any) error {
	tui := map[string]json.RawMessage{}
	if v, ok := fields["tui"]; ok {
		_ = json.Unmarshal(v, &tui)
	}
	enc, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tui[key] = enc
	obj, err := json.Marshal(tui)
	if err != nil {
		return err
	}
	fields["tui"] = obj
	return nil
}

// mutateConfig performs a locked, atomic read-modify-write of config.json:
// mutate edits the parsed top-level field map in place (empty when the file is
// absent), and any field it leaves alone survives the round-trip untouched — so
// writing one field never drops an unknown key. config.json holds no secrets,
// but it is written 0600 (and the home created 0700) to match keys.json /
// keystore.json, and the write is atomic (temp file + rename) so a crash
// mid-write cannot leave a truncated config.json behind.
//
// The read-merge-write is serialized against concurrent digitalx-cli processes
// via config.json's own lock (independent of the registry/vault locks): the
// merge that preserves unknown fields is itself a load-modify-write, so two
// unlocked writers could last-writer-wins and drop a field. config.json has no
// nesting relationship with the other locks, so it has no ordering constraint.
func mutateConfig(home string, log *slog.Logger, mutate func(fields map[string]json.RawMessage) error) error {
	l := logging.Or(log)
	path := Path(home)
	lockPath := path + ".lock"
	l.Debug("config lock: acquiring", "lock", lockPath)
	start := time.Now()
	unlock, err := fslock.Lock(lockPath)
	if err != nil {
		l.Warn("config lock: acquire failed", "lock", lockPath, "err", err)
		return err
	}
	if waited := time.Since(start); waited > 50*time.Millisecond {
		l.Warn("config lock: acquired after contention", "lock", lockPath, "waitedMs", waited.Milliseconds())
	} else {
		l.Debug("config lock: acquired", "lock", lockPath)
	}
	defer unlock()

	fields := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// start from an empty object
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, &fields); err != nil {
			return output.Configf("%s is not a valid JSON object", path)
		}
	}
	if err := mutate(fields); err != nil {
		return err
	}
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
