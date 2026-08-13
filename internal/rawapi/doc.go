// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package rawapi is a statically-typed layer over the wire client with one
// function per REST endpoint. Each endpoint has a typed request struct and a
// typed response struct (or element slice for an array response), and a method
// that builds the ordered wire parameters, issues exactly one call through the
// wire client under a caller-supplied policy, and returns the decoded typed
// value alongside the verbatim response bytes.
//
// The layer is static: it carries no runtime catalog, spec, documentation
// registry, or validation engine. It holds no retry or idempotency policy — the
// caller supplies the policy for the single call each function issues. Money and
// quantity values are decimal strings end to end and pass through untouched.
//
// Ordered parameters follow the wire declaration order: positionals first, then
// the remaining parameters, with absent optional parameters omitted. The wire
// client signs the exact encoded parameter string it sends, so this ordering is
// the signing-byte contract.
package rawapi
