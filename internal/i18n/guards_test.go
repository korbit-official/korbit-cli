// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Drift guards for the flat locale catalog (locales/<code>/<surface>.json).
// Extraction is a test, not a tool: the guards walk the module's source,
// collect every i18n.T("literal") call site, and hold the locale files and the
// call sites to each other — a new string cannot ship untranslated, an English
// copy edit cannot leave a stale translation behind, a translation cannot
// drift from its key's fmt verbs, and an entry cannot sit in the wrong
// surface's file. The pre-place warning surface (internal/ops) is keyed on
// runtime values, so its keys are held live against the quoted literals under
// internal/ops instead of T call sites. Note the deliberately weaker deal on
// that surface: only staleness is guarded (and against any ops literal, not
// just warning templates) — a NEW warning has no coverage guard, ships
// English-first by the fallback design, and gets its Korean entry when a
// human notices, not when a test fails.
package i18n

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const moduleRoot = "../.."

// i18nImportPath is this package's import path — the anchor the extractor
// resolves each file's local package name (usually "i18n") from.
const i18nImportPath = "github.com/digitalx-official/digitalx-cli/internal/i18n"

// surfaceScopes maps each locale file to the source scope its keys must be
// live in (module-relative path prefixes; the most specific match wins, so
// cli/doctorcmd is doctor's even though cli/ is setup's). This is what lets a
// reviewer read one surface's translations in one file — and what stops an
// entry from hiding in the wrong one. A new surface means a new file here
// plus its scope. The surface is derived from the call site, never written at
// it: T stays T("english text", args…) everywhere.
var surfaceScopes = map[string][]string{
	"tui.json":      {"internal/tui/", "internal/cli/tuicmd/"},
	"setup.json":    {"internal/cli/"},
	"doctor.json":   {"internal/cli/doctorcmd/"},
	"preplace.json": {"internal/ops/"}, // via quoted ops literals, not T call sites
}

// surfaceFor names the locale file a key found at path (module-relative)
// belongs in — the most specific matching scope; "" for a path outside every
// scope (a new surface).
func surfaceFor(path string) string {
	best, bestLen := "", 0
	for file, scopes := range surfaceScopes {
		for _, p := range scopes {
			if strings.HasPrefix(path, p) && len(p) > bestLen {
				best, bestLen = file, len(p)
			}
		}
	}
	return best
}

// tCall is one i18n.T call site found in the tree.
type tCall struct {
	pos     token.Position
	relPath string // module-relative source path ("internal/tui/view.go")
	key     string // the folded literal key; "" when not a literal
	literal bool
}

// parseTree parses every authored (non-_test) Go file under root and hands
// each to visit with its module-relative path.
func parseTree(t *testing.T, root string, visit func(fset *token.FileSet, relPath string, f *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		visit(fset, filepath.ToSlash(rel), f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// foldStringLit resolves a compile-time-constant string expression: a string
// literal or a "+" concatenation of them. ok is false for anything else.
func foldStringLit(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := foldStringLit(v.X)
		r, rok := foldStringLit(v.Y)
		return l + r, lok && rok
	case *ast.ParenExpr:
		return foldStringLit(v.X)
	}
	return "", false
}

// extractTCalls returns every i18n.T call site in the module's authored,
// non-test source. Calls inside this package (plain T, not i18n.T) are not
// call sites of the public seam and are deliberately not extracted — the
// PreplaceWarn wrappers are the one sanctioned dynamic entry point.
func extractTCalls(t *testing.T) []tCall {
	t.Helper()
	var out []tCall
	parseTree(t, moduleRoot, func(fset *token.FileSet, relPath string, f *ast.File) {
		local := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == i18nImportPath {
				local = "i18n"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "." {
			// A dot-imported T("literal") parses as a bare identifier call, which
			// the SelectorExpr matcher below cannot see — the file's strings would
			// silently escape every guard.
			t.Errorf("%s dot-imports %s — import it by name so extraction stays sound", relPath, i18nImportPath)
			return
		}
		if local == "" || local == "_" {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "T" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != local {
				return true
			}
			if len(call.Args) == 0 {
				return true // vet catches this as a real error
			}
			key, lit := foldStringLit(call.Args[0])
			out = append(out, tCall{pos: fset.Position(call.Pos()), relPath: relPath, key: key, literal: lit})
			return true
		})
	})
	if len(out) == 0 {
		t.Fatal("extractor found no i18n.T call sites — walk or matcher broken")
	}
	return out
}

// opsStringLiterals returns every quoted string literal under internal/ops —
// the liveness source for the pre-place warning keys (label code values and
// message templates), which reach T through the PreplaceWarn wrappers rather
// than as T literals.
func opsStringLiterals(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	parseTree(t, filepath.Join(moduleRoot, "internal", "ops"), func(_ *token.FileSet, _ string, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
				if s, err := strconv.Unquote(l.Value); err == nil {
					out[s] = true
				}
			}
			return true
		})
	})
	if len(out) == 0 {
		t.Fatal("no string literals under internal/ops — walk broken")
	}
	return out
}

// loadLocaleFile reads one locale file enforcing file hygiene as it goes: no
// duplicate keys (encoding/json would silently keep the last), keys in sorted
// order (canonical layout keeps diffs local and merges trivial), and no empty
// values (delete the entry to fall back to English instead).
func loadLocaleFile(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("%s: expected a top-level object", path)
	}
	out := map[string]string{}
	prev := ""
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		key := kTok.(string)
		var val string
		if err := dec.Decode(&val); err != nil {
			t.Fatalf("%s: value for %q: %v", path, key, err)
		}
		if _, dup := out[key]; dup {
			t.Errorf("%s: duplicate key %q", path, key)
		}
		if prev != "" && !(prev < key) {
			t.Errorf("%s: key %q out of order (after %q) — keep keys sorted", path, key, prev)
		}
		if val == "" {
			t.Errorf("%s: empty value for %q — delete the entry to fall back to English", path, key)
		}
		out[key] = val
		prev = key
	}
	return out
}

// localeSurfaces returns a locale's per-surface tables keyed by file name,
// enforcing that every file is a declared surface and no key appears in two
// files (the runtime loader merges them, so a cross-file duplicate would be
// resolved by merge order — silently).
func localeSurfaces(t *testing.T, code string) map[string]map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("locales", code))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // a locale with no entries has no directory (like the loader)
		}
		t.Fatalf("read locale dir %s: %v", code, err)
	}
	out := map[string]map[string]string{}
	owner := map[string]string{}
	for _, e := range entries {
		if _, ok := surfaceScopes[e.Name()]; !ok {
			t.Errorf("locales/%s/%s is not a declared surface file — add its scope to surfaceScopes", code, e.Name())
			continue
		}
		table := loadLocaleFile(t, filepath.Join("locales", code, e.Name()))
		for k := range table {
			if prev, dup := owner[k]; dup {
				t.Errorf("locales/%s: key %q appears in both %s and %s — one surface owns a key", code, k, prev, e.Name())
			}
			owner[k] = e.Name()
		}
		out[e.Name()] = table
	}
	return out
}

// loadLocale returns a locale's merged table (what the runtime loader builds).
func loadLocale(t *testing.T, code string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range localeSurfaces(t, code) {
		for k, v := range table {
			out[k] = v
		}
	}
	return out
}

// TestTCallsUseLiteralKeys pins the extraction contract: outside this package,
// T takes a compile-time string literal — that is what makes the coverage and
// liveness guards below sound. A dynamic key needs a named wrapper here (see
// PreplaceWarnLabel/PreplaceWarnMessage) whose liveness the guards understand.
func TestTCallsUseLiteralKeys(t *testing.T) {
	for _, c := range extractTCalls(t) {
		if !c.literal {
			t.Errorf("%s: i18n.T key is not a string literal — use a literal, or add a documented dynamic wrapper in internal/i18n", c.pos)
		}
	}
}

// TestEveryTLiteralHasKoEntry: every English source string in the tree has a
// Korean entry, so a new string cannot ship silently untranslated. The error
// names the surface file the entry belongs in (derived from the call site).
// To keep a string deliberately English, set its ko value to the key itself —
// a conscious, reviewable state.
func TestEveryTLiteralHasKoEntry(t *testing.T) {
	ko := loadLocale(t, "ko")
	seen := map[string]bool{}
	for _, c := range extractTCalls(t) {
		if !c.literal || seen[c.key] {
			continue
		}
		seen[c.key] = true
		if _, ok := ko[c.key]; ok {
			continue
		}
		file := surfaceFor(c.relPath)
		if file == "" {
			t.Errorf("%s: no ko entry for %q — this call site is outside every declared surface; add a surface file + scope in surfaceScopes, then the entry", c.pos, c.key)
			continue
		}
		t.Errorf("%s: no ko entry for %q — add it to locales/ko/%s (value = the key itself to deliberately keep English)", c.pos, c.key, file)
	}
}

// TestLocaleEntriesLiveInTheirSurface: every entry maps to a live key WITHIN
// its file's own scope — a T literal under the surface's source paths, or for
// preplace.json a quoted literal under internal/ops. This both catches dead
// entries (an English copy edit leaves a stale translation → update or delete
// it) and misfiled ones (a tui string in setup.json would fail even though it
// is live globally). en files additionally hold only real display overrides.
func TestLocaleEntriesLiveInTheirSurface(t *testing.T) {
	liveIn := map[string]map[string]bool{} // surface file → keys live in its scope
	for file := range surfaceScopes {
		liveIn[file] = map[string]bool{}
	}
	for _, c := range extractTCalls(t) {
		if !c.literal {
			continue
		}
		if file := surfaceFor(c.relPath); file != "" {
			liveIn[file][c.key] = true
		}
	}
	for k := range opsStringLiterals(t) {
		liveIn["preplace.json"][k] = true
	}
	for _, code := range Languages() {
		for file, table := range localeSurfaces(t, code) {
			keys := make([]string, 0, len(table))
			for k := range table {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !liveIn[file][k] {
					t.Errorf("locales/%s/%s: key %q is not live in this surface's scope %v — stale after an English copy edit, or filed under the wrong surface? Update, move, or delete it", code, file, k, surfaceScopes[file])
				}
				if code == DefaultLang && table[k] == k {
					t.Errorf("locales/en/%s: entry %q equals its key — the English files hold only display overrides; delete the no-op entry", file, k)
				}
			}
		}
	}
}

// verbTok is one fmt verb parsed from a template: verb is "%s"/"%q"/"%d"/"%f"
// (the argument index stripped), idx is the explicit argument index of a
// %[n]-form verb (0 for a positional verb).
type verbTok struct {
	verb string
	idx  int
}

// verbs extracts the fmt verbs of s in order; "%%" is skipped. Flags are
// rejected outright: T renders via plain fmt, so the gotext-era "#" flag (and
// any other) is dead weight a translation must not reintroduce.
func verbs(t *testing.T, s string) []verbTok {
	t.Helper()
	var out []verbTok
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		i++
		if i >= len(s) {
			t.Fatalf("dangling %% in %q", s)
		}
		if s[i] == '%' {
			continue // %% literal
		}
		if strings.IndexByte("#+-0 ", s[i]) >= 0 {
			t.Fatalf("%q uses a fmt flag (%%%c…) — localized templates carry bare verbs only (T renders via fmt; the gotext-era # flag is gone)", s, s[i])
		}
		idx := 0
		if s[i] == '[' {
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(s[i+1 : j])
			if err != nil || j >= len(s) || s[j] != ']' {
				t.Fatalf("malformed argument index in %q", s)
			}
			idx = n
			i = j + 1
		}
		if i >= len(s) {
			t.Fatalf("dangling verb in %q", s)
		}
		v := s[i]
		if strings.IndexByte("sqdf", v) < 0 {
			t.Fatalf("%q uses unsupported verb %%%c — localized templates use %%s/%%q/%%d/%%f (extend the guard deliberately if a new verb is needed)", s, v)
		}
		out = append(out, verbTok{verb: "%" + string(v), idx: idx})
	}
	return out
}

// TestLocaleEntriesUseSaneVerbs: a translation must use exactly its key's
// verbs. It may reorder them for the target language's word order, but only
// via explicit indices (%[n]s) — never by mixing explicit and positional
// verbs, and never referencing an out-of-range or wrong-typed argument. A
// positional (index-free) translation must keep the key's verb order. Keys
// themselves never carry indices — only translations reorder.
func TestLocaleEntriesUseSaneVerbs(t *testing.T) {
	for _, code := range Languages() {
		for key, tr := range loadLocale(t, code) {
			kv, tv := verbs(t, key), verbs(t, tr)
			for _, v := range kv {
				if v.idx != 0 {
					t.Errorf("key %q must not use an explicit argument index (%%[n]s) — only translations may reorder", key)
				}
			}
			if len(kv) != len(tv) {
				t.Errorf("verb-count mismatch for %q: key has %d, translation %q has %d", key, len(kv), tr, len(tv))
				continue
			}
			explicit, positional := false, false
			for _, v := range tv {
				if v.idx != 0 {
					explicit = true
				} else {
					positional = true
				}
			}
			switch {
			case explicit && positional:
				t.Errorf("translation for %q mixes explicit (%%[n]s) and positional verbs — use one form or the other", key)
			case explicit:
				for _, v := range tv {
					if v.idx < 1 || v.idx > len(kv) {
						t.Errorf("translation for %q references arg %d, out of range 1..%d", key, v.idx, len(kv))
						continue
					}
					if v.verb != kv[v.idx-1].verb {
						t.Errorf("translation for %q uses %s for arg %d, but the key expects %s", key, v.verb, v.idx, kv[v.idx-1].verb)
					}
				}
			default: // positional: args are consumed in order, so verbs must line up
				for i := range tv {
					if tv[i].verb != kv[i].verb {
						t.Errorf("verb mismatch for %q at position %d: key %s, translation %s", key, i+1, kv[i].verb, tv[i].verb)
					}
				}
			}
		}
	}
}

// TestCatalogRenders is the end-to-end smoke test of the embedded catalog:
// Korean renders a real translation, English renders the key — except for a
// display override (locales/en/), which renders its override in English
// while Korean still renders its own translation. "market order" is the
// canonical homonym: its English surface is "market", which Korean must split
// from the venue sense — so it is pinned here verbatim.
func TestCatalogRenders(t *testing.T) {
	restoreDefault(t)

	if got := T("market order"); got != "market" {
		t.Fatalf(`en T("market order") = %q, want the "market" display override`, got)
	}
	if got := T("side"); got != "side" {
		t.Fatalf(`en T("side") = %q, want the key itself`, got)
	}
	if err := Activate("ko"); err != nil {
		t.Fatal(err)
	}
	if got := T("market order"); got != "시장가 주문" {
		t.Fatalf(`ko T("market order") = %q, want "시장가 주문"`, got)
	}
	if got, want := T("side"), loadLocale(t, "ko")["side"]; got != want {
		t.Fatalf(`ko T("side") = %q, want %q`, got, want)
	}
	// The pre-place dynamic wrappers ride the same table.
	if got := PreplaceWarnLabel("INSUFFICIENT_LIQUIDITY"); got != "유동성 부족" {
		t.Fatalf("ko PreplaceWarnLabel = %q", got)
	}
}
