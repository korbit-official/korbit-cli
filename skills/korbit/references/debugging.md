# Debugging: "what went wrong?" / "what did I do?"

The tool keeps a **local action journal** — an automatic SQLite record of every write API call and a
dedicated row per placed order, written on the machine (nothing is sent anywhere, no secrets are
stored). When the user asks what happened, **read the journal instead of guessing.** This is the
single best answer to "did my order go through?", "why did that fail?", or "what have you done so
far?".

Journaling is on by default: writes (order place/cancel, transfers) are always recorded, reads only
under `--debug`. It's a hard guarantee — an order's row is written *before* the request is sent. Opt
out with `DIGITALX_CLI_NO_JOURNAL=1`.

## Reading the journal

```sh
korbit logs --json                  # recent write API calls (newest first)
korbit logs --orders --json         # every order this tool placed, and whether it landed
korbit logs --operations --json     # logical operations (a place-then-reconcile, a paged walk) grouped
korbit logs --limit 50 --json       # widen the window (default 20, max 1000)
```

What each view answers:

- **`--orders`** — the source of truth for order outcomes. Each row has `clientOrderId`, `symbol`,
  `side`, `type`, `status` (`attempting`/`accepted`/`failed`/`unknown`), `orderId`, `errorCode`, and
  `retryCount`. An `accepted` status is **sticky** — a later failed retry never downgrades it — so if
  you see `accepted`, the order *was* placed (use the recorded `clientOrderId` to look it up or to
  retry idempotently). This is how you recover after a crash: find the last `attempting`/`unknown`
  row, then `order get --client-order-id <id>` to learn its real state.
- **default (api_calls)** — one row per HTTP call: `endpoint`, `httpStatus`, `success`, `errorCode`,
  `errorMessage`, `retryCount`, `durationMs`. To answer "what went wrong?", scan for `success:false`
  and read the `errorCode`/`httpStatus`.
- **`--operations`** — groups the calls a single logical action made (a placement that sent then
  looked up; a history walk that paged), with the overall `outcome` and `attempts`.

The journal records every surface uniformly — CLI commands, MCP tool calls, and monitor activity all
show up the same way. (Over MCP, `logs` isn't a tool; read it in a shell.)

## Wider diagnostics for support

```sh
korbit debug bundle --json          # writes a redacted diagnostic file (no secrets) for Korbit support
korbit debug bundle --out /tmp/korbit-debug.json --limit 500
```

The bundle collects recent operations/calls/orders plus environment facts (OS, version, home,
configured key metadata — never key material). Use it when escalating an issue to Korbit, not for
routine inspection (`logs` is lighter).

## Live diagnostics

For a clock/IP/connectivity problem rather than a past action, `korbit doctor --json` is the
read-only health check (key → binding → `whoami` → default accountSeq permission → public IP → clock skew → WebSocket reachability),
and each finding carries a `fix`. Add `--debug` (or `DIGITALX_CLI_DEBUG=1`) to any command for verbose
stderr diagnostics (request target, retry decisions, timing).

## Clock skew / `EXCEED_TIME_WINDOW`

Signed requests carry a timestamp the server checks against a tight window, so a wrong **host clock**
makes signed calls fail with `EXCEED_TIME_WINDOW`. `korbit doctor` reports the measured offset, as does
`korbit time` (server time plus the local-clock `offsetMs`/`rttMs`) — a public check that needs no key. The
CLI auto-corrects reactively for the calls it safely can (the `auto` default), and passing
`--time-sync on` to any signed command signs with a server-corrected timestamp up front (safe even for
single-shot writes — it corrects the one send without resending). But the real fix is the system clock:

- **Confirm the OS clock is NTP-synced.** On Linux: `timedatectl` (want `System clock synchronized:
  yes` / `NTP service: active`; enable with `sudo timedatectl set-ntp true`). On macOS: System
  Settings → General → Date & Time → *Set time and date automatically*. On Windows: `w32tm /resync`. A
  drifting clock that isn't NTP-synced keeps failing until that's fixed.
- **To diagnose *why* the clock drifted**, `korbit doctor --diagnose-clock` (Windows: the W32Time
  service + NTP source; Linux: `timedatectl`) reports the OS time-sync state and names the fix commands.
- A clock running **ahead** of the server can only be fixed by NTP or `--time-sync on` (the server's
  future bound is fixed).
