// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package keystoretest provides an in-memory keystore.Keystore for tests that
// need to exercise migration and error paths without touching a platform keyring
// (which isn't available or deterministic in unit tests). It is test-only
// infrastructure: nothing in the shipped binary imports it.
package keystoretest

// MockKeystore is an in-memory keystore with injectable failures. The zero value
// is not usable — call New.
type MockKeystore struct {
	backend string
	data    map[string]string

	// GetErr/SetErr/DeleteErr, when non-nil, are consulted before the operation;
	// returning a non-nil error makes that operation fail. They receive the key
	// name so a test can fail a specific key (or use a stateful closure to fail,
	// say, only the verification read that follows a Set).
	GetErr    func(name string) error
	SetErr    func(name string) error
	DeleteErr func(name string) error

	// Tamper makes Set store a different value than it was given (simulating
	// silent corruption), so the post-copy verification read mismatches.
	Tamper map[string]string
}

// New returns an empty mock reporting the given backend name.
func New(backend string) *MockKeystore {
	return &MockKeystore{backend: backend, data: map[string]string{}}
}

// Seed pre-populates an entry, bypassing the error/tamper hooks.
func (m *MockKeystore) Seed(name, secret string) { m.data[name] = secret }

// Has reports whether an entry currently exists (test inspection helper).
func (m *MockKeystore) Has(name string) bool { _, ok := m.data[name]; return ok }

func (m *MockKeystore) Backend() string { return m.backend }

func (m *MockKeystore) Get(name string) (string, error) {
	if m.GetErr != nil {
		if err := m.GetErr(name); err != nil {
			return "", err
		}
	}
	return m.data[name], nil
}

func (m *MockKeystore) Set(name, secret string) error {
	if m.SetErr != nil {
		if err := m.SetErr(name); err != nil {
			return err
		}
	}
	if m.Tamper != nil {
		if t, ok := m.Tamper[name]; ok {
			m.data[name] = t
			return nil
		}
	}
	m.data[name] = secret
	return nil
}

func (m *MockKeystore) Delete(name string) error {
	if m.DeleteErr != nil {
		if err := m.DeleteErr(name); err != nil {
			return err
		}
	}
	delete(m.data, name)
	return nil
}
