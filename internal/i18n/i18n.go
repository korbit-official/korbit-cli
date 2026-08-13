// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package i18n owns every locale decision in the program: which display
// languages exist, which one is active, and how a localized string is looked
// up. No other layer interprets a language value — the command layer resolves
// the language with Detect and hands the result to Activate, and rendering code
// calls T.
//
// English is both the default and the reference language: a T key IS the
// English text, so a key with no registered translation renders as-is. That
// is a deliberate drift property — when English copy changes, its old
// translation goes dead and the UI falls back to English instead of showing
// stale text (and the dead entry fails TestLocaleEntriesAreLive).
//
// Translations live in flat per-locale JSON files under locales/<code>/,
// embedded at build time and merged into one table per locale: each file maps
// keys (the English call-site literal, verbatim) to that locale's rendering
// templates. The files split by SURFACE so a human reviews one surface's
// translations in one place — tui.json (TUI chrome), setup.json (interactive
// setup + shared key guidance), doctor.json (health-check diagnostics),
// preplace.json (pre-place order warnings) — and the guards enforce the
// placement (a key must be live in its file's source scope, and no key may
// appear in two files). locales/en/ exists only for display overrides — the
// rare key whose on-screen English differs from the key itself because one
// English word translates differently by context (e.g. key "market order"
// displays "market", but Korean needs 시장가 주문 distinct from a venue's
// 마켓). Everything else the catalog needs is enforced by tests in this
// package: every i18n.T literal in the tree has a ko entry, every entry maps
// to a live key, and a translation's verbs match its key's (guards_test.go).
//
// The pre-place warnings (internal/ops) are keyed on runtime values
// (PlaceWarning.Format / .Code), not literals, so the call sites cannot be
// extracted — they render through the PreplaceWarnLabel/PreplaceWarnMessage
// wrappers, and their keys are kept live by matching the quoted literals under
// internal/ops instead. Owning the ops surface's translation here keeps ops
// itself free of any localization dependency — it emits the English source
// plus the raw template/args, and a consumer renders the localized form.
//
// Two language values matter, kept deliberately distinct:
//
//   - The DETECTED language — what the run resolved from the global --lang flag
//     (else the host locale, else DefaultLang). Resolve computes it once at
//     startup, validating --lang and erroring on an unsupported code, and records
//     it; Detected reports it. Language selection is a program-wide concern, so
//     --lang is a global flag and Resolve runs regardless of command.
//   - The ACTIVE language — what T renders. It stays English until a surface
//     that localizes calls Activate(Detected()); any surface that does not
//     activate stays English regardless of the host locale. Agent- and
//     machine-facing output does not localize.
//
// Reading the host locale is confined to Detect/Resolve; Activate merely sets a
// resolved code and never touches the environment.
//
// Trading vocabulary (side/type/tif values, channel names), symbols, numbers,
// and API error codes/descriptions are never localized either — those pass
// through T untouched or bypass it entirely.
//
// Formatting rule: T interpolates with fmt.Sprintf, so a template may use any
// fmt verb — %d and %f render exactly as fmt renders them, never locale
// digit-grouped (money stays a pre-formatted decimal string passed as %s
// regardless). A translation must use exactly its key's verbs; it may reorder
// them for the target language's word order via explicit indices (%[2]s).
// TestLocaleEntriesUseSaneVerbs enforces this for every entry.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"

	"golang.org/x/text/language"
)

// DefaultLang is the language of the source strings, active until Activate
// picks another. All T keys are written in it.
const DefaultLang = "en"

// langTags maps every supported language code to its BCP 47 tag. Adding a
// language means adding it here plus its translations at locales/<code>.json.
var langTags = map[string]language.Tag{
	"en": language.English,
	"ko": language.Korean,
}

// Languages lists the supported language codes, DefaultLang first — the
// single source for the --lang enum and any future language picker.
func Languages() []string { return []string{"en", "ko"} }

//go:embed locales/*/*.json
var localeFS embed.FS

// tables holds each locale's key → rendering-template map, loaded once by
// merging the embedded per-concern files under locales/<code>/ (the guard
// tests forbid a key appearing in two of them, so merge order cannot matter).
// A missing key falls back to the key itself (the English source) at lookup
// time in T.
var tables = func() map[string]map[string]string {
	out := make(map[string]map[string]string, len(langTags))
	for _, code := range Languages() {
		dir := "locales/" + code
		// A locale with nothing to say has no directory at all (English,
		// before its first display override existed) — that is an empty
		// table, not an error; the coverage guards keep a real locale from
		// silently losing its files.
		entries, _ := fs.ReadDir(localeFS, dir)
		m := map[string]string{}
		for _, e := range entries {
			b, err := localeFS.ReadFile(dir + "/" + e.Name())
			if err != nil {
				panic("i18n: unreadable locale file " + e.Name() + ": " + err.Error())
			}
			if err := json.Unmarshal(b, &m); err != nil {
				panic("i18n: bad locale file " + dir + "/" + e.Name() + ": " + err.Error())
			}
		}
		out[code] = m
	}
	return out
}()

// locale is the active language and its lookup table, swapped atomically so a
// future in-session language switch is just another Activate call — readers
// (T) never lock.
type locale struct {
	lang  string
	table map[string]string
}

var active atomic.Pointer[locale]

// detected is the run's resolved display language (see Resolve). It defaults to
// DefaultLang until Resolve runs and is independent of the active language:
// resolving a language does not activate it.
var detected atomic.Pointer[string]

func init() {
	active.Store(&locale{lang: DefaultLang, table: tables[DefaultLang]})
	d := DefaultLang
	detected.Store(&d)
}

// Activate sets the active display language. Empty input activates DefaultLang.
// An unsupported code is an error naming the valid ones; the active language is
// left unchanged. Callers resolve the language with Detect first (which is where
// the host locale is read); Activate itself never consults the environment.
func Activate(lang string) error {
	if lang == "" {
		lang = DefaultLang
	}
	if _, ok := langTags[lang]; !ok {
		return fmt.Errorf("unsupported language %q — supported: %v", lang, Languages())
	}
	active.Store(&locale{lang: lang, table: tables[lang]})
	return nil
}

// localeMatcher matches a host locale against the supported languages. It is
// built from langTags in Languages() order, so DefaultLang (first) is the
// fallback and adding a language needs no change here.
var localeMatcher = func() language.Matcher {
	tags := make([]language.Tag, 0, len(langTags))
	for _, code := range Languages() {
		tags = append(tags, langTags[code])
	}
	return language.NewMatcher(tags)
}()

// Detect resolves the display language for a run without changing the active
// one — a caller activates the result (only the tui does). An explicit choice
// (the --lang value) wins and, if it names an unsupported language, is an error
// so a typo is reported rather than silently ignored. With no explicit choice
// the host locale is consulted — the POSIX locale env ($LC_ALL/$LC_MESSAGES/
// $LANG) on Unix, else the user's preferred UI languages on Windows — and an
// absent or unrecognized locale yields DefaultLang.
func Detect(explicit string, getenv func(string) string) (string, error) {
	if explicit != "" {
		if _, ok := langTags[explicit]; !ok {
			return "", fmt.Errorf("unsupported language %q — supported: %v", explicit, Languages())
		}
		return explicit, nil
	}
	return localeToCode(hostLocales(getenv)), nil
}

// hostLocales returns the host's preferred locales, most preferred first
// ("ko_KR", "en-US", …), or nil. The POSIX locale env wins when set (the
// ".<codeset>"/"@<modifier>" suffix stripped, the portable "C"/"POSIX" locales
// treated as absent); otherwise the OS hook supplies them — the user's preferred
// UI languages on Windows, nothing on Unix (where the env is the only source).
func hostLocales(getenv func(string) string) []string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := getenv(k)
		if i := strings.IndexAny(v, ".@"); i >= 0 {
			v = v[:i]
		}
		if v == "" || v == "C" || v == "POSIX" {
			continue
		}
		return []string{v}
	}
	return osUILanguages()
}

// localeToCode picks the supported code that best matches the preference-ordered
// locales, defaulting to DefaultLang when none is recognized.
func localeToCode(locales []string) string {
	tags := make([]language.Tag, 0, len(locales))
	for _, l := range locales {
		if t, err := language.Parse(strings.ReplaceAll(l, "_", "-")); err == nil {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return DefaultLang
	}
	if _, idx, conf := localeMatcher.Match(tags...); conf != language.No {
		return Languages()[idx]
	}
	return DefaultLang
}

// Resolve determines the run's display language once, at startup, from the
// global --lang value (else the host locale, else DefaultLang), records it as
// the detected language, and returns it. A non-empty value that names an
// unsupported language is an error — this is where the global --lang flag is
// parsed and rejected. Resolve does NOT activate the language; a surface that
// localizes activates it with Activate(Detected()).
func Resolve(explicit string, getenv func(string) string) (string, error) {
	code, err := Detect(explicit, getenv)
	if err != nil {
		return "", err
	}
	detected.Store(&code)
	return code, nil
}

// Detected reports the run's detected display language — DefaultLang until
// Resolve runs.
func Detected() string { return *detected.Load() }

// Active reports the active language code.
func Active() string { return active.Load().lang }

// T renders a localized string: the key is the English text (and the
// fallback), args interpolate fmt.Sprintf-style. The lookup is by the key
// verbatim — including %% for a literal percent, which Sprintf collapses at
// render time in the key and translation alike.
func T(key string, args ...any) string {
	format := key
	if tr, ok := active.Load().table[key]; ok {
		format = tr
	}
	return fmt.Sprintf(format, args...)
}

// PreplaceWarnLabel localizes a pre-place warning's code label
// (ops.PlaceWarning.Code) for display. It is T with a runtime key: the code
// value is the lookup key, so English (and any untranslated label) renders the
// stable code itself. This wrapper — not a T("literal") call site — is the
// sanctioned dynamic entry point for warning labels; the extraction guard
// keys their locale entries to the quoted literals under internal/ops instead.
func PreplaceWarnLabel(code string) string { return T(code) }

// PreplaceWarnMessage localizes and renders a pre-place warning message.
// format is the English template (ops.PlaceWarning.Format), the lookup key at
// runtime; args interpolate into the translation (or into the template itself
// when none is registered — a partial table falls back to English by design,
// so a newly added warning ships English until its locales/<code>/preplace.json
// entry lands; unlike a T literal, no guard forces that entry to exist).
// Like PreplaceWarnLabel, this is the sanctioned dynamic T entry point for
// warning messages.
func PreplaceWarnMessage(format string, args ...any) string { return T(format, args...) }
