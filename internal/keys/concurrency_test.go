// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/korbit-official/korbit-cli/internal/keystore"
)

// fileManager builds a Manager over the REAL file vault rooted at home — not the
// in-memory mock — because the lost-update bug these tests guard against lives
// in the on-disk load → modify → atomic-rename cycle. Separate Manager instances
// over the same home open SEPARATE file descriptors, which is exactly the
// cross-process shape advisory flock/LockFileEx serializes (and the only shape
// that can reproduce a lost update — a single process sharing one Manager would
// already be serialized by Go's own scheduling within a method).
func fileManager(home string) *Manager {
	return NewManagerWithBackends(
		home, "file",
		func(b string) (keystore.Keystore, error) { return keystore.ByName(b, home, nil) },
		func(b string) error { return keystore.Available(b, nil) },
		nil,
	)
}

// withTimeout fails the test loudly instead of letting CI hang, so a regression
// that reintroduces a deadlock (e.g. a single shared lock self-deadlocking on
// the nested registry→vault acquisition) surfaces as a clear failure.
func withTimeout(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("operation did not complete within %v — likely a lock deadlock", d)
	}
}

// TestConcurrentAddNoLostKeys is the headline regression test. N goroutines each
// Add a DISTINCT key into the SAME home through SEPARATE Manager instances
// (separate fds = the cross-process shape). Afterwards ALL N records must exist
// in keys.json AND all N secrets must decrypt from the file vault. Without the
// advisory locks, concurrent whole-file rewrites of keys.json and keystore.json
// drop entries (last-writer-wins); with them every key survives. Must pass under
// `-race`.
//
// Add holds the registry lock WHILE its store.Set takes the vault lock — the
// nested acquisition. The timeout guard turns a deadlock into a loud failure
// rather than a hung CI run.
func TestConcurrentAddNoLostKeys(t *testing.T) {
	const n = 8
	home := t.TempDir()

	withTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Fresh Manager per goroutine: distinct fds, distinct file backend
				// instance — the genuine multi-process shape.
				_, errs[i] = fileManager(home).Add(fmt.Sprintf("key%02d", i), "", "")
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Add key%02d: %v", i, err)
			}
		}
	})

	// Every record must be present in keys.json.
	names, err := fileManager(home).Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != n {
		t.Fatalf("got %d keys in keys.json, want %d: %v", len(names), n, names)
	}

	// Every secret must decrypt from the file vault — this is the part the
	// unlocked vault would silently lose.
	vault := keystore.NewFile(home)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("key%02d", i)
		secret, err := vault.Get(name)
		if err != nil {
			t.Fatalf("vault Get %q: %v", name, err)
		}
		if secret == "" {
			t.Fatalf("secret for %q missing from the file vault (lost update)", name)
		}
	}
}

// TestConcurrentDistinctFieldUpdatesSurvive guards the registry against
// lost updates on ONE record: two goroutines repeatedly mutate DIFFERENT fields
// of the same key (Bind sets apiKeyId, SetBaseURL sets baseUrl). Each mutation
// is a full load-modify-write; without the registry lock one writer's snapshot
// clobbers the other's field. With the lock, the final record carries BOTH a
// bound api key id and a base URL.
func TestConcurrentDistinctFieldUpdatesSurvive(t *testing.T) {
	home := t.TempDir()
	if _, err := fileManager(home).Add("k", "", ""); err != nil {
		t.Fatalf("seed Add: %v", err)
	}

	const rounds = 50
	withTimeout(t, 30*time.Second, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := fileManager(home).Bind("k", fmt.Sprintf("API_KEY_%03d", i)); err != nil {
					t.Errorf("Bind: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := fileManager(home).SetBaseURL("k", fmt.Sprintf("https://h%03d.example.com", i), ""); err != nil {
					t.Errorf("SetBaseURL: %v", err)
					return
				}
			}
		}()
		wg.Wait()
	})

	show, err := fileManager(home).Show("k")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if show.APIKeyID == nil || *show.APIKeyID == "" {
		t.Error("apiKeyId was lost — a SetBaseURL write clobbered the Bind field")
	}
	if show.BaseURL == "" {
		t.Error("baseUrl was lost — a Bind write clobbered the SetBaseURL field")
	}
}
