// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

// Command licensegen regenerates THIRD_PARTY_LICENSES.txt at the repository root: the
// attribution notices for every third-party Go module linked into the korbit
// binary. The compiled binary statically links these modules, so their
// permissive licenses (MIT / BSD / ISC / Apache-2.0) require their copyright and
// permission notices to travel with every copy we distribute. The generated file
// is shipped inside each release archive (see the archive `files` list in
// .goreleaser.yaml) and `korbit license` points at it in the source repository.
//
// Run from the module root:
//
//	go run ./tools/licensegen
//
// or `make licenses`. Commit the regenerated file. The output is deterministic
// (modules sorted, files sorted) so re-running with no dependency change is a
// no-op diff.
//
// Discovery mirrors what a license scanner does: for every package actually
// linked into the binary (`go list -deps` on the main package, standard-library
// packages excluded), find the nearest LICENSE/COPYING/NOTICE-style file walking
// up from the package directory to its module root, and additionally take any
// NOTICE at the module root (Apache-2.0 §4(d)). Files are de-duplicated by path,
// so a module that carries extra per-directory notices (e.g. a vendored
// third-party license inside one subpackage) contributes all of them exactly
// once.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// mainModule is the module whose linked dependencies are credited. The binary
// is `package main` at the module root, so its transitive deps are exactly what
// ships in the binary.
const mainModule = "github.com/korbit-official/korbit-cli"

// defaultOut is written relative to the module root (the working directory when
// run via `go run ./tools/licensegen` or `make licenses`). It sits at the
// repo root so it is easy to include in release archives and to link to in the
// source repository.
const defaultOut = "THIRD_PARTY_LICENSES.txt"

// licenseStem matches the name (with any extension removed) of a file that
// carries a license, copyright, or notice: LICENSE, LICENCE, COPYING,
// COPYRIGHT, NOTICE, PATENTS, UNLICENSE, and suffixed forms such as LICENSE_V8
// or LICENSE-MIT that modules use for vendored third-party notices.
var licenseStem = regexp.MustCompile(`(?i)^(licen[sc]e|copying|copyright|notice|patents|unlicense)([._-].*)?$`)

// noticeStem matches only NOTICE files (Apache-2.0 §4(d)), collected from every
// module root regardless of where that module's primary license sits.
var noticeStem = regexp.MustCompile(`(?i)^notice([._-].*)?$`)

// allowedExt is the set of extensions a license file may carry. License texts
// are plain text or Markdown; restricting to this allowlist excludes non-text
// companions that share a license stem — detached signatures (.minisig, .sig,
// .asc), keys (.pem), and the like — which are not license text. A file with an
// unrecognized extension is skipped; if that leaves a module with no license at
// all, run() fails loudly rather than shipping an incomplete notice.
var allowedExt = map[string]bool{"": true, ".txt": true, ".md": true}

type module struct {
	path, version, dir string
}

// entry is one license file belonging to one module: the module's path/version,
// the file's base name, and its canonicalized text.
type entry struct{ path, version, filename, body string }

func main() {
	out := defaultOut
	if len(os.Args) > 2 && os.Args[1] == "-o" {
		out = os.Args[2]
	}
	if err := run(out); err != nil {
		fmt.Fprintln(os.Stderr, "licensegen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	pkgs, err := listPackages()
	if err != nil {
		return err
	}

	// Group linked package directories by module, and record each module's root.
	mods := map[string]*module{}
	pkgDirs := map[string][]string{} // module key -> package dirs
	for _, p := range pkgs {
		if p.modPath == "" || p.modPath == mainModule {
			continue // standard library, or our own module
		}
		key := p.modPath + "@" + p.modVersion
		if _, ok := mods[key]; !ok {
			mods[key] = &module{path: p.modPath, version: p.modVersion, dir: p.modDir}
		}
		pkgDirs[key] = append(pkgDirs[key], p.dir)
	}

	// Collect the license files for each module, de-duplicated by absolute path.
	type modFiles struct {
		mod   *module
		files []string
	}
	var collected []modFiles
	for key, m := range mods {
		seen := map[string]bool{}
		var files []string
		add := func(path string) {
			if !seen[path] {
				seen[path] = true
				files = append(files, path)
			}
		}
		for _, dir := range pkgDirs[key] {
			for _, f := range nearestLicenses(dir, m.dir) {
				add(f)
			}
		}
		for _, f := range matchingFiles(m.dir, noticeStem) {
			add(f) // module-root NOTICE (Apache-2.0 §4(d))
		}
		if len(files) == 0 {
			return fmt.Errorf("no license file found for module %s (dir %s)", key, m.dir)
		}
		// Sort by module-relative path so the order is stable across machines
		// (the GOMODCACHE prefix, which varies per machine, is stripped).
		sort.Slice(files, func(i, j int) bool {
			return relTo(m.dir, files[i]) < relTo(m.dir, files[j])
		})
		collected = append(collected, modFiles{mod: m, files: files})
	}

	// Read each collected license file once, canonicalizing its text so files
	// differing only in incidental whitespace compare (and render) identically.
	var entries []entry
	for _, c := range collected {
		for _, f := range c.files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			entries = append(entries, entry{c.mod.path, c.mod.version, filepath.Base(f), canonicalize(raw)})
		}
	}

	// Merge entries that share identical license text into one section crediting
	// every dependency that carries it. The key is the exact canonical text — NOT
	// the license *type* — so two MIT licenses with different copyright lines stay
	// separate and no attribution is dropped; only genuinely identical texts (a
	// shared upstream, one publisher's whole family) collapse.
	byBody := map[string][]entry{}
	for _, e := range entries {
		byBody[e.body] = append(byBody[e.body], e)
	}
	type group struct {
		members []entry
		body    string
	}
	var groups []group
	for body, mem := range byBody {
		sort.Slice(mem, func(i, j int) bool { return lessEntry(mem[i], mem[j]) })
		groups = append(groups, group{members: mem, body: body})
	}
	// Order groups by their lowest member so the file is deterministic and a
	// dependency change stays a local diff — a version bump or add/remove touches
	// one section rather than reshuffling the file.
	sort.Slice(groups, func(i, j int) bool { return lessEntry(groups[i].members[0], groups[j].members[0]) })

	var buf bytes.Buffer
	writeHeader(&buf)
	for _, g := range groups {
		writeGroup(&buf, g.members, g.body)
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "licensegen: wrote %s (%d modules, %d license texts)\n", out, len(collected), len(groups))
	return nil
}

const sep = "================================================================================"

func writeHeader(buf *bytes.Buffer) {
	fmt.Fprintf(buf, "Third-party software notices for korbit-cli\n")
	fmt.Fprintf(buf, "%s\n\n", sep)
}

// writeGroup writes one section: every dependency that shares the license text
// (one "module version (file)" line each), then the text once.
func writeGroup(buf *bytes.Buffer, members []entry, body string) {
	fmt.Fprintf(buf, "%s\n", sep)
	for _, m := range members {
		fmt.Fprintf(buf, "%s %s (%s)\n", m.path, m.version, m.filename)
	}
	fmt.Fprintf(buf, "%s\n\n", sep)
	buf.WriteString(body)
	buf.WriteString("\n\n")
}

// lessEntry orders entries by module path, then version, then filename.
func lessEntry(a, b entry) bool {
	if a.path != b.path {
		return a.path < b.path
	}
	if a.version != b.version {
		return a.version < b.version
	}
	return a.filename < b.filename
}

// canonicalize normalizes license text for both comparison and display: each
// line's trailing whitespace is stripped and surrounding blank lines are
// trimmed, so notices that differ only in incidental whitespace merge. Content
// is otherwise preserved verbatim.
func canonicalize(raw []byte) string {
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// nearestLicenses returns the license-style files in the closest ancestor of
// pkgDir (inclusive) that contains any, stopping at modDir. An empty result
// means neither the package nor any ancestor up to the module root carries one.
func nearestLicenses(pkgDir, modDir string) []string {
	dir := pkgDir
	for {
		if m := matchingFiles(dir, licenseStem); len(m) > 0 {
			return m
		}
		if dir == modDir || !strings.HasPrefix(dir, modDir) {
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// relTo returns path relative to base, or path unchanged if that fails. Used
// only to derive a machine-independent sort key.
func relTo(base, path string) string {
	if r, err := filepath.Rel(base, path); err == nil {
		return r
	}
	return path
}

// matchingFiles returns the absolute paths of regular files in dir whose
// extension is allowed and whose stem (name without extension) matches stem,
// sorted by name.
func matchingFiles(dir string, stem *regexp.Regexp) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !allowedExt[ext] || !stem.MatchString(strings.TrimSuffix(name, filepath.Ext(name))) {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

type pkg struct {
	dir, modPath, modVersion, modDir string
}

// listPackages runs `go list -deps` on the main package and returns one entry
// per non-standard-library package linked into it.
func listPackages() ([]pkg, error) {
	const tmpl = `{{if not .Standard}}{{if .Module}}{{.Dir}}::{{.Module.Path}}::{{.Module.Version}}::{{.Module.Dir}}{{end}}{{end}}`
	cmd := exec.Command("go", "list", "-deps", "-f", tmpl, mainModule)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var pkgs []pkg
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "::")
		if len(parts) != 4 {
			return nil, fmt.Errorf("unexpected go list line: %q", line)
		}
		pkgs = append(pkgs, pkg{dir: parts[0], modPath: parts[1], modVersion: parts[2], modDir: parts[3]})
	}
	return pkgs, nil
}
