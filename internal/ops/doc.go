// Copyright (c) 2026 Digital X Co., Ltd.
//
// SPDX-License-Identifier: Apache-2.0

// Package ops is the L2 operations layer: the Korbit Open API v2 with THIS
// CLI's guarantees layered on top of the L1 primitive client (internal/apiclient).
//
// # Where it sits
//
//	frontends (cli, monitor/botapi, mcp)             — no call policy of their own
//	          │  resolve an Operation from the catalog (Find/Catalog) and call op.Run
//	          ▼
//	internal/ops  (THIS package)                     — owns ALL retry/idempotency POLICY
//	          │  issues typed rawapi calls under an apiclient.Policy it decides
//	          ▼
//	internal/rawapi + internal/apiclient  (L1/L0)       — typed endpoints, contract-faithful executor
//
// The command surface is the catalog: each endpoint is one registered Operation
// (Catalog/Find), carrying its presentation+validation metadata (OpMeta) and its
// behavior (Run). There is no string-keyed routing and no default-to-passthrough
// — an endpoint with no operation simply is not in the catalog.
//
// The architecture rule is: POLICY LIVES HERE. The L1 client runs exactly the
// retry Policy it is handed (its zero Policy is a single shot); frontends just
// ask an Operation to Run and render the Result. ops decides — per operation,
// from its OpMeta.Safety — whether the underlying send may be retried and how.
//
// # Dependency rule
//
// ops imports internal/rawapi + internal/apiclient (the typed endpoints, the client
// + the exported retry taxonomy) and internal/cmdmeta (the shared metadata
// vocabulary). It does NOT import internal/spec.
// ops MUST NOT import internal/stream or internal/botapi: the allowed direction
// is stream → ops (stream consumes WalkHistory from here), never the reverse,
// and the frontends depend on ops, not vice versa. ops does NO output
// formatting, runs NO JavaScript, knows nothing about the WebSocket layer, and
// owns no journal policy (the journal severity decision belongs to the
// Recorder behind the L1 client; the only journal seam ops carries is the
// per-order intent/finish hook for the place protocol, which a frontend wires).
//
// # Backend contract assumptions (the money-safety foundation)
//
// The place protocol's safety rests on three properties of the Korbit backend.
// Each line states the property and the code path that depends on it; if a
// property stops holding, that path is unsafe.
//
//   - clientOrderId uniqueness is server-enforced: a reused id is rejected with
//     DUPLICATE_CLIENT_ORDER_ID, never creating a second order. Every resend
//     reuses the same id, so a lost-response resend that actually landed returns
//     DUPLICATE instead of double-placing. This is the no-double-order guarantee.
//   - HTTP 429 is pre-execution: the rate limiter rejects at the gate, before the
//     order reaches matching. A budget-exhausted 429 is therefore a clean failure
//     with no verification lookup (apiclient.ClassRateLimited ∈ IsPreExecution).
//     EXCEED_TIME_WINDOW is pre-execution for the same reason (a clock-gate
//     rejection).
//   - The order read path is eventually consistent: a just-accepted order can be
//     briefly invisible to a clientOrderId lookup. A negative lookup is therefore
//     never proof an order did not land: the lookup retries across ~1s
//     (lookupAttempts/lookupRetryMs), and the endgame only asserts an outcome it
//     has positive proof of (a 2xx accept or a DUPLICATE answer). An ambiguous
//     send whose order is not visible is UNKNOWN, not failed.
//
// # The place-order protocol — why it is safe
//
// The place operation is the flagship guarantee. Order placement is a money
// mover with no natural idempotency (OpMeta.Safety is nonIdempotent), so it is
// NEVER auto-resent by the L1 retry ladder. ops makes resending safe explicitly,
// using the server's own deduplication:
//
//   - Every placement carries a clientOrderId (auto-minted UUIDv7 when the
//     caller omits one). The server answers a REUSED id with
//     DUPLICATE_CLIENT_ORDER_ID — so a resend of the SAME id can never place a
//     second order.
//   - The order's mint-time intent is journaled BEFORE anything is sent (a hard
//     pre-send guarantee: a journal failure fails the placement with nothing on
//     the wire).
//   - The send is single-shot. ops then reconciles by the failure's class
//     (internal/apiclient's pre-execution vs ambiguous taxonomy):
//   - EXCEED_TIME_WINDOW — provably rejected at the server's clock gate
//     BEFORE any side effect: resync the clock ONCE and resend the same id.
//   - HTTP 429 — provably rate-limited before execution: honor Retry-After
//     (else back off) within the retry budget, then resend the same id.
//   - network error / HTTP 5xx — AMBIGUOUS (the order MAY have landed):
//     resend the SAME id; a DUPLICATE answer proves the earlier attempt
//     landed. Bounded by the retry budget.
//   - any other rejection (insufficient funds, bad price, …) — surfaced
//     immediately; no resend.
//   - Every success path (first-try, resync-corrected, duplicate-resolved)
//     returns the FULL fetched order — one shape for the caller.
//   - The order read path is EVENTUALLY CONSISTENT, so the endgame only ever
//     ASSERTS an outcome it has positive proof of; a negative lookup is never
//     read as "not placed". After a 2xx accept or a DUPLICATE answer the order
//     IS placed: the lookup is retried across ~1s, and if it still can't be read
//     back we return the accept acknowledgement (2xx, carrying the orderId) or
//     re-surface DUPLICATE_CLIENT_ORDER_ID (duplicate) — never "failed". Only
//     the AMBIGUOUS (network/5xx) budget-exhausted case is genuinely unsure: its
//     lookup either finds the order (return it) or yields state-UNKNOWN (throw
//     with verification instructions). 429/EXCEED_TIME_WINDOW are pre-execution,
//     so they surface as clean failures with no lookup. We never GUESS about
//     money, and we never call a placed order failed.
//
// # The honesty rule (never silently truncate)
//
// The cursorless history endpoints (order.history, fills) and the funding
// histories can only return a bounded window, and candles past the server's
// 200-row cap must be paged. When a requested window cannot be fully covered,
// ops returns the rows it DID get with Result.Truncated set and a Result.Note
// explaining the limit — it never silently under-delivers. A frontend renders
// that out-of-band (the bot API attaches truncated/note array properties and
// prints the note to stderr; a CLI consumer renders it differently).
package ops
