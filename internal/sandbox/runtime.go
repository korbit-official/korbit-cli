// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/korbit-official/korbit-cli/internal/sandbox/deno"
)

// RuntimeKind selects which Deno runs the bundle. There is no Node runtime: the
// sandbox is a deliberately limited convenience feature and runs only under Deno
// (for its permission sandbox). For anything more involved, download the bundle
// from the Official Source and run it yourself.
type RuntimeKind string

const (
	// RuntimeSystemDeno runs the bundle with a `deno` already on PATH.
	RuntimeSystemDeno RuntimeKind = "deno"
	// RuntimeManagedDeno runs it with the CLI-managed, pinned Deno (downloaded on
	// first use). It is the default — a known, checksum-verified version.
	RuntimeManagedDeno RuntimeKind = "managed-deno"
)

// Runtime is a resolved way to invoke the bundle: `<deno> run <least-privilege
// perms> <bundleURLorPath> <args>` (see Manager.denoRunPerms). The bundle source
// (URL or local path) is handed straight to Deno, which fetches+caches it.
type Runtime struct {
	Kind RuntimeKind
	// denoPath is the resolved system `deno` (RuntimeSystemDeno only).
	denoPath string
	// deno is the managed-Deno manager. Carried for BOTH kinds: the managed kind
	// resolves+downloads the binary through it, and either kind uses its RunEnv
	// (DENO_DIR) and module-cache helpers.
	deno *deno.Manager
}

// lookPath is the executable resolver (exec.LookPath in production; tests stub
// it to control whether a `deno` appears present).
type lookPath func(string) (string, error)

// resolveRuntime picks the runtime: an explicit preference wins; else the managed
// pinned Deno (the default — a known, checksum-verified version, downloaded on
// first use); else a system `deno` only where no managed-Deno build exists for
// the platform (Alpine/musl Linux). pref is "", "deno", or "managed-deno".
func resolveRuntime(pref string, look lookPath, denoMgr *deno.Manager) (Runtime, error) {
	switch RuntimeKind(pref) {
	case RuntimeSystemDeno:
		p, err := look("deno")
		if err != nil {
			return Runtime{}, fmt.Errorf("--runtime deno requested but no `deno` is on PATH: %w", err)
		}
		return Runtime{Kind: RuntimeSystemDeno, denoPath: p, deno: denoMgr}, nil
	case RuntimeManagedDeno:
		return Runtime{Kind: RuntimeManagedDeno, deno: denoMgr}, nil
	case "":
		// Default to the managed, pinned Deno. Fall back to a system `deno` only
		// where no managed-Deno build exists (musl Linux); if neither is available,
		// still return managed so its Ensure surfaces the precise "no managed Deno
		// build for <platform> — install Deno and use --runtime deno" message.
		if _, err := deno.Target(); err == nil {
			return Runtime{Kind: RuntimeManagedDeno, deno: denoMgr}, nil
		}
		if p, err := look("deno"); err == nil {
			return Runtime{Kind: RuntimeSystemDeno, denoPath: p, deno: denoMgr}, nil
		}
		return Runtime{Kind: RuntimeManagedDeno, deno: denoMgr}, nil
	default:
		return Runtime{}, fmt.Errorf("unknown --runtime %q (want deno or managed-deno)", pref)
	}
}

// command builds the argv to run the bundle with the resolved runtime, ensuring
// the managed Deno is downloaded first when that runtime is selected. perms are
// the least-privilege Deno permission flags (see Manager.denoRunPerms). bundleRef
// is the source URL or local path passed verbatim to Deno, which fetches+caches a
// remote module itself. env is the extra environment to set on the child
// (DENO_DIR) — apply it with withRuntimeEnv. The argv shape is identical for both
// runtimes; only the binary differs (a system `deno` vs the managed one).
func (r Runtime) command(ctx context.Context, perms []string, bundleRef string, args ...string) (bin string, argv []string, env []string, err error) {
	if r.deno == nil {
		return "", nil, nil, fmt.Errorf("Deno runtime is not configured")
	}
	switch r.Kind {
	case RuntimeSystemDeno:
		_, a := r.deno.RunArgs(bundleRef, perms, args...)
		return r.denoPath, a, r.deno.RunEnv(), nil
	case RuntimeManagedDeno:
		managedBin, a := r.deno.RunArgs(bundleRef, perms, args...)
		if _, err := r.deno.Ensure(ctx); err != nil {
			return "", nil, nil, err
		}
		return managedBin, a, r.deno.RunEnv(), nil
	default:
		return "", nil, nil, fmt.Errorf("unresolved runtime")
	}
}

// denoBin resolves the Deno executable for this runtime: a system `deno` as-is,
// or the managed binary (downloading it on first use). Used by non-`run` Deno
// invocations (e.g. `deno cache --reload` for sandbox update).
func (r Runtime) denoBin(ctx context.Context) (string, error) {
	switch r.Kind {
	case RuntimeSystemDeno:
		return r.denoPath, nil
	case RuntimeManagedDeno:
		if r.deno == nil {
			return "", fmt.Errorf("managed Deno runtime is not configured")
		}
		return r.deno.Ensure(ctx)
	default:
		return "", fmt.Errorf("unresolved runtime")
	}
}

// realLookPath is the production executable resolver.
func realLookPath(name string) (string, error) { return exec.LookPath(name) }
