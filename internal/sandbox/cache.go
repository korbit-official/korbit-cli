// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitalx-official/digitalx-cli/internal/envalias"
)

// EnvCacheDir names the environment variable that relocates the shared artifact
// cache. The legacy KORBIT_CLI_SANDBOX_CACHE spelling is accepted as a fallback
// (see envalias).
const EnvCacheDir = "DIGITALX_CLI_SANDBOX_CACHE"

// EnvSandboxURL names the environment variable that overrides the bundle source,
// so an error message can name it without spelling it out again. Its legacy
// KORBIT_CLI_SANDBOX_URL spelling is likewise accepted (see envalias).
const EnvSandboxURL = "DIGITALX_CLI_SANDBOX_URL"

// CacheDirName is the cache directory under os.UserCacheDir();
// LegacyCacheDirName is the one an installation made under the earlier product
// name carries. An existing legacy directory keeps being used rather than
// re-downloading the heavy artifacts into an empty new one beside it.
const (
	CacheDirName       = "digitalx-cli"
	LegacyCacheDirName = "korbit-cli"
)

// ResolveCacheDir resolves the shared artifact cache root: $DIGITALX_CLI_SANDBOX_CACHE
// (else the legacy $KORBIT_CLI_SANDBOX_CACHE) when set; otherwise
// os.UserCacheDir()/digitalx-cli, unless that does not exist and the legacy
// os.UserCacheDir()/korbit-cli does. It is decoupled from the CLI home so
// an ephemeral per-agent home doesn't re-download the heavy artifacts, and it is
// the single resolver both `sandbox` (which fills the cache) and `self uninstall`
// (which can remove it) share, so the two can't drift onto different directories.
// getenv reads the process environment (injectable for tests).
func ResolveCacheDir(getenv func(string) string) (string, error) {
	if c := envalias.Lookup(getenv, EnvCacheDir); c != "" {
		return c, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve a cache directory (%v) — set %s", err, EnvCacheDir)
	}
	current := filepath.Join(base, CacheDirName)
	if isDir(current) {
		return current, nil
	}
	if legacy := filepath.Join(base, LegacyCacheDirName); isDir(legacy) {
		return legacy, nil
	}
	return current, nil
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// DefaultSandboxURL is the Official Source the sandbox bundle is obtained from.
// It is the only place the bundle should ever come from; a local path / file://
// URL is also accepted (offline/dev use) via the URL override. Under the Deno
// runtime the bundle source is passed straight to `deno run`, which fetches and
// caches it; the CLI does not download or cache the bundle itself. Deno keys that
// cache by URL, so this URL is fetched on its first run even when another URL
// serving the same bundle is already cached.
const DefaultSandboxURL = "https://docs.digitalx.miraeasset.com/digitalx-sandbox.mjs"

// Doer performs an HTTP request; *http.Client satisfies it. Used to download the
// managed Deno binary and to probe the sandbox's /v2/time readiness endpoint.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// localPath returns the on-disk path for a local src (a bare filesystem path or
// a file:// URL), or "" when src is an http(s) URL that Deno fetches over the
// network. Used to tell a local override from a remote source for `status`
// (cached == file present) and `update` (a local file needs no refresh).
func localPath(src string) string {
	if strings.HasPrefix(src, "file://") {
		return strings.TrimPrefix(src, "file://")
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		return ""
	}
	// A bare filesystem path.
	return src
}

// writeCacheReadme drops a README explaining the shared artifact cache. It points
// at the Official Source and carries a short fork-safety pointer that the bundle
// is separately licensed (read it with `sandbox license`) — but it must never
// reproduce the disclaimer body (the leak/license contract). Best-effort.
func writeCacheReadme(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte(cacheReadmeText), 0o644)
}

const cacheReadmeText = `# digitalx-cli sandbox cache

Cache maintained by digitalx-cli for its local sandbox (a mock of the Digital X
Open API for local development and testing). Safe to delete — recreated on the next
` + "`sandbox`" + ` command.

The sandbox runs under Deno, which fetches the bundle from the Official Source
(https://docs.digitalx.miraeasset.com) and caches it here:

- ` + "`deno/`" + ` — pinned, SHA-256-verified Deno runtimes (github.com/denoland/deno)
  the CLI downloads on demand, one directory per version and platform
  (` + "`<version>_<target>/`" + `). It runs the bundle under a least-privilege
  permission sandbox (loopback networking + this cache and the sandbox state dir
  only). Old versions are pruned automatically when the CLI updates Deno.
- ` + "`deno-modules/`" + ` — Deno's own cache of the bundle (` + "`digitalx-sandbox.mjs`" + `) and
  any dependencies.

Terms: the bundle (digitalx-sandbox.mjs) is proprietary software of Digital X Co., Ltd. under its
OWN terms — it is NOT covered by digitalx-cli's open-source license. Read those
terms with ` + "`dgx-cli sandbox license`" + `, obtain the bundle only from the
Official Source, and keep use conformant (local development and testing only).

Your sandbox database, keys, and logs are NOT here — they live under your
digitalx-cli home (DIGITALX_CLI_HOME). Override this cache with DIGITALX_CLI_SANDBOX_CACHE.
`
