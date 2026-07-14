// Copyright (c) 2026 Korbit Inc.
//
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package keystore

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// osKeyring is the keychainProvider for every platform except macOS. It uses
// zalando/go-keyring, which talks to the OS credential store natively in pure
// Go: the Windows Credential Manager (advapi32 via danieljoos/wincred) and the
// Linux/BSD Secret Service (D-Bus via godbus). Neither shells out to a helper
// binary, so there is no "wrong identity" problem to solve here — only macOS,
// where go-keyring drives the /usr/bin/security CLI, needs the native override
// in keychain_darwin.go.
type osKeyring struct{}

func init() { keychain = osKeyring{} }

func (osKeyring) set(account, secret string) error {
	return keyring.Set(keychainService, account, secret)
}

func (osKeyring) get(account string) (string, bool, error) {
	secret, err := keyring.Get(keychainService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}

func (osKeyring) del(account string) error {
	err := keyring.Delete(keychainService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (osKeyring) probe() error {
	_, err := keyring.Get(keychainService, probeAccount)
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
