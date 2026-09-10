// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	goruntime "runtime"
	"runtime/pprof"

	"github.com/digitalx-official/digitalx-cli/internal/output"
	"github.com/spf13/cobra"
)

// startProfiling honors the hidden --cpuprofile / --memprofile flags present on
// every command. It starts a CPU profile when --cpuprofile is set and returns a
// stop function that stops it and writes a heap profile when --memprofile is
// set. The stop function is always non-nil and safe to call; it is a no-op when
// neither flag is given. Callers run it via defer, so the CPU profile spans the
// whole command (including long-running ones like tui and monitor).
func startProfiling(cmd *cobra.Command) (stop func(), err error) {
	cpuPath, _ := cmd.Flags().GetString("cpuprofile")
	memPath, _ := cmd.Flags().GetString("memprofile")
	if cpuPath == "" && memPath == "" {
		return func() {}, nil
	}

	var cpuFile *os.File
	if cpuPath != "" {
		f, ferr := os.Create(cpuPath)
		if ferr != nil {
			return nil, output.Usagef("could not create --cpuprofile file: %v", ferr)
		}
		if perr := pprof.StartCPUProfile(f); perr != nil {
			_ = f.Close()
			return nil, output.Usagef("could not start CPU profile: %v", perr)
		}
		cpuFile = f
	}

	return func() {
		if cpuFile != nil {
			pprof.StopCPUProfile()
			_ = cpuFile.Close()
		}
		if memPath == "" {
			return
		}
		f, ferr := os.Create(memPath)
		if ferr != nil {
			return
		}
		defer f.Close()
		goruntime.GC() // settle the heap so the profile reflects live objects
		_ = pprof.WriteHeapProfile(f)
	}, nil
}
