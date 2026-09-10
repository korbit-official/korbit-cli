// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package keystore

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// This file is the macOS "keychain" backend. It calls Security.framework
// (SecItem*) directly through purego — a pure-Go dynamic FFI, so the binary
// still cross-compiles from any host with CGO_ENABLED=0.
//
// # Why not the /usr/bin/security CLI
//
// The portable OS-keyring library drives macOS through the `security`
// command-line tool. A keychain item's access-control list then binds to
// `security`'s identity, not this CLI's — so the code-signature-based policy a
// signed binary could enforce is unreachable. Calling Security.framework in
// process fixes that: the item created by SecItemAdd binds to THIS binary's
// designated code requirement.
//
// # The security property (legacy keychain ACL)
//
// These calls target the legacy file-based keychain (kSecUseDataProtectionKeychain
// is left unset/false). When a code-signed binary adds a generic-password item
// without an explicit kSecAttrAccess, the system seeds the item's ACL with that
// binary's designated requirement, which is rooted in the signing Team ID. The
// effect for a Developer-ID-signed `korbit`:
//
//   - this binary, and future versions signed with the SAME code identity
//     (Developer ID Team ID AND a stable signing identifier — pin it with
//     `codesign -i`, since it otherwise derives from the file name), read the
//     secret with no prompt: the requirement matches on identity, not on the
//     exact code hash, so an upgrade keeps working;
//   - any OTHER program reading the item triggers the system keychain-consent
//     dialog instead of silently succeeding.
//
// This is the achievable policy for a single signed CLI. Silent same-Team-ID
// sharing across arbitrary binaries would need the data-protection keychain with
// a keychain-access-groups entitlement, which a non-bundled command tool cannot
// carry (it has nowhere to embed the authorizing provisioning profile and would
// fail with errSecMissingEntitlement). An UNSIGNED/ad-hoc dev build still works,
// but its requirement is unstable across rebuilds, so the OS may re-prompt — the
// `file` backend remains the default, so this only affects developers who opt
// into the keychain on an unsigned build.

// # Wire-format compatibility with the portable OS-keyring library
//
// The stored value is encoded EXACTLY as the portable OS-keyring library
// encodes a macOS keychain value: the secret is base64-encoded and prefixed with
// "go-keyring-base64:" (and a "go-keyring-encoded:" hex prefix is also honored on
// read). Items therefore live under the same service+account and carry a value
// that library reads verbatim, so the keychain backend can fall back to it
// without losing access to keys this binary stored (and vice versa) — the only
// difference is the access-control policy above (it may pop a consent dialog).
// keychain_compat_test.go pins this format.

// OSStatus result codes (Security framework, SecBase.h).
const (
	errSecSuccess       = 0
	errSecDuplicateItem = -25299
	errSecItemNotFound  = -25300
)

// CoreFoundation constants.
const (
	cfStringEncodingUTF8 = 0x08000100
	kCFAllocatorDefault  = 0
)

// Value-encoding prefixes, matching the portable OS-keyring library's macOS
// format so a stored item is interchangeable with it (see the wire-format note
// above). set always writes the base64 form; get also decodes the hex form the
// library may have written.
const (
	goKeyringBase64Prefix = "go-keyring-base64:"
	goKeyringHexPrefix    = "go-keyring-encoded:"
)

// encodeSecret renders a secret the way the portable OS-keyring library stores a
// macOS keychain value: base64 with the "go-keyring-base64:" prefix.
func encodeSecret(secret string) string {
	return goKeyringBase64Prefix + base64.StdEncoding.EncodeToString([]byte(secret))
}

// decodeSecret reverses encodeSecret and also accepts the library's hex variant
// and a bare (unprefixed) value, mirroring its read path — including the leading
// TrimSpace, harmless for our own always-prefixed writes.
func decodeSecret(stored string) (string, error) {
	s := strings.TrimSpace(stored)
	switch {
	case strings.HasPrefix(s, goKeyringHexPrefix):
		b, err := hex.DecodeString(strings.TrimPrefix(s, goKeyringHexPrefix))
		if err != nil {
			return "", err
		}
		return string(b), nil
	case strings.HasPrefix(s, goKeyringBase64Prefix):
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, goKeyringBase64Prefix))
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return s, nil
	}
}

// cfRange mirrors CFRange (two CFIndex == long fields), passed by value to
// CFDataGetBytes to copy out the stored secret.
type cfRange struct {
	location int64
	length   int64
}

// CoreFoundation type-constant pointers, resolved from the dylib at init.
var (
	cfTypeDictionaryKeyCallBacks   uintptr
	cfTypeDictionaryValueCallBacks uintptr
	cfBooleanTrue                  uintptr
)

// Security-framework attribute keys (CFStringRef constants), resolved at init.
var (
	kSecClass                uintptr
	kSecClassGenericPassword uintptr
	kSecAttrService          uintptr
	kSecAttrAccount          uintptr
	kSecValueData            uintptr
	kSecReturnData           uintptr
)

// CoreFoundation functions.
var (
	cfDictionaryCreate        func(allocator uintptr, keys, values *uintptr, numValues int64, keyCallBacks, valueCallBacks uintptr) uintptr
	cfStringCreateWithCString func(allocator uintptr, cStr string, encoding uint32) uintptr
	cfDataCreate              func(allocator uintptr, bytes []byte, length int64) uintptr
	cfDataGetLength           func(data uintptr) int64
	cfDataGetBytes            func(data uintptr, rng cfRange, buffer []byte)
	cfRelease                 func(cf uintptr)
)

// Security-framework item functions.
var (
	secItemCopyMatching func(query uintptr, result *uintptr) int32
	secItemAdd          func(attributes uintptr, result uintptr) int32
	secItemUpdate       func(query uintptr, attributesToUpdate uintptr) int32
	secItemDelete       func(query uintptr) int32
)

func init() {
	cf := mustOpen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
	cfTypeDictionaryKeyCallBacks = mustSym(cf, "kCFTypeDictionaryKeyCallBacks")
	cfTypeDictionaryValueCallBacks = mustSym(cf, "kCFTypeDictionaryValueCallBacks")
	cfBooleanTrue = deref(mustSym(cf, "kCFBooleanTrue"))

	purego.RegisterLibFunc(&cfDictionaryCreate, cf, "CFDictionaryCreate")
	purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&cfDataCreate, cf, "CFDataCreate")
	purego.RegisterLibFunc(&cfDataGetLength, cf, "CFDataGetLength")
	purego.RegisterLibFunc(&cfDataGetBytes, cf, "CFDataGetBytes")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")

	sec := mustOpen("/System/Library/Frameworks/Security.framework/Security")
	kSecClass = deref(mustSym(sec, "kSecClass"))
	kSecClassGenericPassword = deref(mustSym(sec, "kSecClassGenericPassword"))
	kSecAttrService = deref(mustSym(sec, "kSecAttrService"))
	kSecAttrAccount = deref(mustSym(sec, "kSecAttrAccount"))
	kSecValueData = deref(mustSym(sec, "kSecValueData"))
	kSecReturnData = deref(mustSym(sec, "kSecReturnData"))

	purego.RegisterLibFunc(&secItemCopyMatching, sec, "SecItemCopyMatching")
	purego.RegisterLibFunc(&secItemAdd, sec, "SecItemAdd")
	purego.RegisterLibFunc(&secItemUpdate, sec, "SecItemUpdate")
	purego.RegisterLibFunc(&secItemDelete, sec, "SecItemDelete")

	keychain = nativeKeychain{}
}

// nativeKeychain is the macOS keychainProvider over Security.framework.
type nativeKeychain struct{}

func (nativeKeychain) set(service, account, secret string) error {
	svc := cfString(service)
	defer cfRelease(svc)
	acct := cfString(account)
	defer cfRelease(acct)
	data := cfData([]byte(encodeSecret(secret)))
	defer cfRelease(data)

	add := dict(
		[]uintptr{kSecClass, kSecAttrService, kSecAttrAccount, kSecValueData},
		[]uintptr{kSecClassGenericPassword, svc, acct, data},
	)
	defer cfRelease(add)

	switch st := secItemAdd(add, 0); st {
	case errSecSuccess:
		return nil
	case errSecDuplicateItem:
		// Item exists (re-add): update its data in place. The match
		// query addresses the item by service+account; the attributes carry
		// only the new value.
		match := dict(
			[]uintptr{kSecClass, kSecAttrService, kSecAttrAccount},
			[]uintptr{kSecClassGenericPassword, svc, acct},
		)
		defer cfRelease(match)
		attrs := dict([]uintptr{kSecValueData}, []uintptr{data})
		defer cfRelease(attrs)
		if su := secItemUpdate(match, attrs); su != errSecSuccess {
			return fmt.Errorf("SecItemUpdate failed (OSStatus %d)", su)
		}
		return nil
	default:
		return fmt.Errorf("SecItemAdd failed (OSStatus %d)", st)
	}
}

func (nativeKeychain) get(service, account string) (string, bool, error) {
	svc := cfString(service)
	defer cfRelease(svc)
	acct := cfString(account)
	defer cfRelease(acct)

	query := dict(
		[]uintptr{kSecClass, kSecAttrService, kSecAttrAccount, kSecReturnData},
		[]uintptr{kSecClassGenericPassword, svc, acct, cfBooleanTrue},
	)
	defer cfRelease(query)

	var result uintptr
	switch st := secItemCopyMatching(query, &result); st {
	case errSecSuccess:
		// result is a CFDataRef holding the encoded value.
		defer cfRelease(result)
		n := cfDataGetLength(result)
		if n <= 0 {
			return "", true, nil
		}
		buf := make([]byte, n)
		cfDataGetBytes(result, cfRange{location: 0, length: n}, buf)
		secret, err := decodeSecret(string(buf))
		if err != nil {
			return "", false, fmt.Errorf("stored keychain value is malformed: %w", err)
		}
		return secret, true, nil
	case errSecItemNotFound:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("SecItemCopyMatching failed (OSStatus %d)", st)
	}
}

func (nativeKeychain) del(service, account string) error {
	svc := cfString(service)
	defer cfRelease(svc)
	acct := cfString(account)
	defer cfRelease(acct)

	query := dict(
		[]uintptr{kSecClass, kSecAttrService, kSecAttrAccount},
		[]uintptr{kSecClassGenericPassword, svc, acct},
	)
	defer cfRelease(query)

	switch st := secItemDelete(query); st {
	case errSecSuccess, errSecItemNotFound:
		return nil
	default:
		return fmt.Errorf("SecItemDelete failed (OSStatus %d)", st)
	}
}

func (nativeKeychain) probe(service string) error {
	// Read-only lookup of a never-written sentinel: a healthy keychain answers
	// errSecItemNotFound with no prompt; only a genuine backend failure errors.
	_, _, err := nativeKeychain{}.get(service, probeAccount)
	return err
}

// cfString builds a CFStringRef the caller must CFRelease.
func cfString(s string) uintptr {
	return cfStringCreateWithCString(kCFAllocatorDefault, s, cfStringEncodingUTF8)
}

// cfData builds a CFDataRef the caller must CFRelease.
func cfData(b []byte) uintptr {
	return cfDataCreate(kCFAllocatorDefault, b, int64(len(b)))
}

// dict builds an immutable CFDictionaryRef from parallel key/value slices, using
// the CoreFoundation type callbacks so it retains the CFType keys and values.
// The caller must CFRelease the result. keys and values must be the same length
// and non-empty.
func dict(keys, values []uintptr) uintptr {
	return cfDictionaryCreate(
		kCFAllocatorDefault,
		&keys[0], &values[0], int64(len(keys)),
		cfTypeDictionaryKeyCallBacks,
		cfTypeDictionaryValueCallBacks,
	)
}

func mustOpen(path string) uintptr {
	lib, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		panic("keystore: cannot load " + path + ": " + err.Error())
	}
	return lib
}

func mustSym(lib uintptr, name string) uintptr {
	sym, err := purego.Dlsym(lib, name)
	if err != nil {
		panic("keystore: cannot resolve " + name + ": " + err.Error())
	}
	return sym
}

// deref reads the pointer a Dlsym handle points at. A framework data symbol
// (e.g. a CFStringRef constant like kSecClass) resolves to the ADDRESS of the
// constant, so one dereference yields the CFTypeRef value itself. The
// double-cast is the documented way to do this without tripping go vet's
// unsafe.Pointer misuse check (golang.org/issue/41205).
func deref(ptr uintptr) uintptr {
	return **(**uintptr)(unsafe.Pointer(&ptr))
}
