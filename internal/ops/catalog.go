// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package ops

// catalog is the registry of operations, populated by register() calls in the
// domain files' init(). It is the SINGLE source of the endpoint command surface:
// there is no string-keyed routing and no default-to-passthrough — every
// endpoint is an explicit registered operation, so an endpoint with no operation
// simply is not in the catalog.
var catalog []Operation

// register adds an operation to the catalog. Called from init() in the domain
// files.
func register(op Operation) {
	catalog = append(catalog, op)
}

// Catalog returns a copy of the registered operations in registration order.
func Catalog() []Operation {
	out := make([]Operation, len(catalog))
	copy(out, catalog)
	return out
}

// Find resolves an operation from command-path segments, preferring the deepest
// matching operation (mirrors spec.Find) so a nested leaf wins over a shorter
// prefix. nil when no operation matches.
func Find(id ...string) Operation {
	for n := len(id); n >= 1; n-- {
		for _, op := range catalog {
			m := op.Meta()
			if len(m.ID) == n && idHasPrefix(m.ID, id[:n]) {
				return op
			}
		}
	}
	return nil
}

// idHasPrefix reports whether id begins with every segment of prefix.
func idHasPrefix(id, prefix []string) bool {
	if len(id) < len(prefix) {
		return false
	}
	for i := range prefix {
		if id[i] != prefix[i] {
			return false
		}
	}
	return true
}
