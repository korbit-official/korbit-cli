# AGENTS.md

Developer guide for AI agents working on this codebase. It is a **map, not a
spec**: it tells you where each thing lives and the few cross-cutting rules that
no single file owns. The authoritative detail lives in the code — package
`doc.go` files and call-site comments — and this guide points at it rather than
restating it. End-user documentation lives in `README.md`; keep contributor
material here and user material there.

> **Read ["Maintaining this guide"](#maintaining-this-guide) before you add to
> this file.** Its one rule: if the detail belongs in code, put it there and
> link to it here — do not grow a second copy that will drift.

## Maintaining this guide

This file earns its keep only as a navigational layer. To keep it from rotting
back into a duplicate of the source:

1. **Code is the single source of truth.** Behavior, contracts, and rationale
   that have a natural code home belong in that package's `doc.go` or at the
   call site. Document it *there*, then reference it here in one line.
2. **Before adding a paragraph, check whether the code already says it.** If it
   does, link to it instead. If it doesn't but should, write it in the code and
   link to it. Only write it out *here* when it has no code home (see #4).
3. **Reference by path + symbol, never by line number.** Use `file/path.go`
   plus a function/type/const name, or for a Markdown doc its section heading.
   Line numbers churn on every edit; symbol names are stable and greppable.
4. **What legitimately lives here in full** (no natural code home): the package
   map, cross-cutting policy and rationale that spans layers (e.g. the
   server-clock-vs-system-clock guideline), the contributor workflow ("to add X,
   edit Y"), the dependency-choice reasons, the test methodology, and the
   "keep these in sync" checklists.
5. **When you change referenced code, fix the reference.** A renamed symbol or
   moved test makes a pointer here actively misleading — update or delete it in
   the same change.
6. **This repo is public.** Never add internal infrastructure/repo names,
   parent-repo paths, or proprietary license text. For the separately-licensed
   sandbox bundle, carry a pointer, never the body (see the sandbox section).

## Project

`digitalx-cli` — a self-contained, single-binary CLI for the Digital X Open API v2,
designed as a stable tool surface for AI trading agents. ED25519 and
HMAC-SHA256 signing. Written in Go.

**Dependencies and why each was chosen** (the names are in `go.mod`; the
reasoning is the part worth keeping). Almost every choice is in service of
**cgo-free cross-compilation** and a zero-friction single binary:

- `spf13/cobra` — the command tree.
- `modernc.org/sqlite` — the local action journal; pure-Go, no cgo.
- `coder/websocket` — the WebSocket client; pure-Go, zero transitive deps.
- `ebitengine/purego` — the cgo-free FFI behind the native macOS Keychain
  backend, so the keychain item binds to this binary's code signature rather
  than a helper tool's. `zalando/go-keyring` is the Windows/Linux keyring backend.
- `dop251/goja` + `dop251/goja_nodejs` — the pure-Go JS engine + event loop
  behind the monitor command's **experimental** `--where`/`--on`/`--init` bot runtime.
- `itchyny/gojq` — the pure-Go jq behind the monitor command's **stable** `--jq` filter.
- `shopspring/decimal` — exact decimal arithmetic for local money math, on
  advisory/derived values only; order values themselves stay wire strings.
- `modelcontextprotocol/go-sdk` — the official MCP SDK behind `mcp serve`.
- The Charm v2 stack (`charm.land/bubbletea/v2`, `bubbles/v2`, `lipgloss/v2`,
  plus `charmbracelet/x/term` and `charmbracelet/colorprofile`) — behind the `tui` command.

## Build & commands

```sh
go build -o dgx-cli .   # build the binary
go test ./...          # run the suite (fast; run after every change)
go vet ./...           # static checks
gofmt -l .             # must print nothing
go run . <args>        # run from source during development
make dist              # local snapshot of all release artifacts (no publish)
```

The `Makefile` is the source of truth for these targets. **Releases** are built
with GoReleaser (`.goreleaser.yaml`) — cross-compile matrix, version injected
from the git tag, macOS code-signing, and a per-platform `.mcpb` Desktop
Extension (`scripts/build-mcpb.sh`); notarization is a separate real-release
step. A plain `go build` leaves the version as `dev`. Full workflow and signing
credentials: [`RELEASING.md`](RELEASING.md).

## Architecture

The package map below is the navigational index. Where a package has a `doc.go`,
that file is the authoritative description — read it before changing the package.

```
main.go        entrypoint: cli.Execute(os.Args[1:]) -> exit code
internal/
  cmdmeta/     shared command-surface vocabulary (incl. the Safety class) + the
               per-value validation engine (NormalizeValue/CoerceValue), depended
               on across the layers                                    — see cmdmeta/doc.go
  spec/        the BUILTIN command surface + shared vocabulary types    — see the Registry var in spec/registry.go
  rawapi/      L1 typed endpoint layer over the wire client             — see rawapi/doc.go
  apiclient/   L0 wire layer (encode/sign/envelope/retry taxonomy) + the
               L1 primitive client (Client.Do)                          — see apiclient/doc.go
  netbind/     outbound source/interface/IP-family binding (--bind/--family) — see netbind/doc.go
  useragent/   composes the User-Agent header sent on REST + WS requests — see the package doc in useragent/useragent.go
  accountseq/  the accountSeq selection policy shared by REST ops + private WS subscriptions — see the package doc in accountseq/accountseq.go
  ops/         L2 operations layer: owns the retry/idempotency policy for every
               endpoint operation the command surface dispatches (read-only probes
               elsewhere declare apiclient.Policy{Idempotent:true} inline; the ladder
               mechanics stay in internal/apiclient); the Operation catalog, place
               reconcile, history/candles paging, cross-field validation — see ops/doc.go
  clock/       one shared server-clock estimate (State) + the one Syncer — see clock/doc.go
  callrec/     the journal-backed apiclient.Recorder + the single journaling policy — see callrec/doc.go
  journal/     local SQLite action journal (api_calls + orders)         — see the package doc in journal/journal.go
  keys/        named-key registry (keys.json) + lifecycle commands      — see keys/doc.go
  keystore/    private-key vaults (file AES-256-GCM; native macOS keychain) — see keystore/doc.go
  logging/     leveled diagnostic logger (log/slog), distinct from program output — see logging/doc.go
  i18n/        display-language decisions + localized-string lookup — see the package doc in i18n/i18n.go
  output/      stdout/stderr contract, JSON emit, error taxonomy -> exit codes
  config/      config.json + CLI home resolution (DIGITALX_CLI_HOME)
  fslock/      advisory cross-process file lock serializing read-modify-write over on-disk state — see the package doc in fslock/fslock.go
  sqlitefile/  the one SQLite DSN builder every opener of a database under the CLI home shares, so a home path containing `?` or `%` resolves to the same file everywhere — see the package doc in sqlitefile/sqlitefile.go
  progname/    the program's invoked name (set once at startup; read by help/examples/guidance) — see the package doc in progname/progname.go
  ids/         UUIDv7 minting + clientOrderId charset
  stream/      resilient real-time WebSocket layer                      — see stream/doc.go
  stream/state/ materializes stream events into current state
  candles/     shared decimal-string OHLC bucket math (Series) + the monitor's
               candle-channel synthesizer (Synth); the TUI chart consumes Series
               too                                                      — see the package docs in candles/candles.go and the Synth doc in candles/synth.go
  botapi/      monitor's JS bot runtime (goja loop + worker pool)       — see the package doc in botapi/botapi.go
  jqfilter/    monitor's stable --jq filter/transform engine            — see the package doc in jqfilter/jqfilter.go
  indicators/  technical-indicator math                                 — see indicators/doc.go
  sandbox/     local API-sandbox lifecycle manager (+ sandbox/deno managed runtime) — see the package doc in sandbox/sandbox.go
  tui/         interactive trading terminal (Bubble Tea v2)             — see tui/README.md
  agentskill/  install/inspect the bundled Agent Skill from an embedded fs.FS — see the package doc in agentskill/agentskill.go
  selfupdate/  on-disk install layout + self-update mechanics (install/update/uninstall/doctor, PATH wiring, manifest) — see the package doc in selfupdate/selfupdate.go. It moves and renames NOTHING: no command relocates a CLI home, an artifact cache, or a file inside them. Home resolution, a home's file names, and the by-hand move are the user-facing contract in MIGRATION.md; the two states `self doctor` reports about it live in selfupdate/doctor.go
  cli/         cobra tree built from the unified surface; dispatch; help; the
               error->exit-code wrapper. Subpackages behind the clienv seam:
                 clienv/    the per-invocation Env + Backend/Console contracts
                 probe/     base-URL/WS validation + reachability probes
                 textout/   human-output toolkit + the TextFormatter seam
                 setupui/   interactive TTY front-end for the `setup` command
                 {doctorcmd,sandboxcmd,agentskillcmd,monitorcmd,tuicmd,keymgmtcmd,selfcmd}/  extracted per-command logic
  version/     CLI version string (build-injected; "dev" default)
```

**Subpackage rule:** a command subpackage under `cli/` **never imports `cli`** —
it receives everything through the `clienv` seam (`cli/clienv`), and `cli`
dispatches to it via `Run(cx *clienv.Cmd, …)`. Keep this direction acyclic.

**The bundled skill is embedded in `package main`** (`skillfs.go`,
`//go:embed all:skills/digitalx-cli`) because an embed directive can't use `..`. It is
injected into `cli` via `Deps.SkillFS`; `internal/agentskill` only ever takes an
`fs.FS`, so tests inject an `fstest.MapFS`.

### The command surface drives everything

Help text (`cli/help.go`), the machine-readable `commands` catalog
(`cli/catalog.go`), and the cobra tree (`cli/root.go`) all derive from one
**unified surface** (`cli/surface.go`) that merges the ops catalog (endpoint
Operations) with the spec builtins. Therefore:

- **To add/change an endpoint:** edit its Operation in `internal/ops` (its
  `OpMeta` carries params, the cross-field rule, the safety class).
- **To add/change a builtin:** edit its `internal/spec` registry entry.
- **Never hand-write help text or the catalog** — they are generated.

Drift guards: `internal/ops/catalog_test.go` (endpoints — path↔spec match,
safety/auth/destructive classification, response-hint existence) and
`internal/spec/registry_test.go` (builtins — unique ids, kebab-case flags that
never shadow globals, enums declared, defaults self-validating, examples
resolving back to their own command). `responseFields` are sourced from each
Operation's `OpMeta.Response` (never hand-written) and name-checked against the
public spec by `TestCatalogResponseHintsMatchPublicSpec` in
`internal/ops/catalog_test.go`. The `catalogVersion` evolution policy is
documented at the top of `cli/catalog.go` — that comment is authoritative.

### Marking a feature experimental

An opt-in, not-yet-stable feature is gated behind the global
`--enable-experimental` flag (or `DIGITALX_CLI_ENABLE_EXPERIMENTAL`). Mark it
**structurally**, never with inline `[experimental]` text in a `Desc`:

1. Set `Experimental: true` on the `cmdmeta.Param`.
2. Put notes/examples in `ExperimentalNotes`/`ExperimentalExamples` (on
   `spec.Command` for a builtin, `OpMeta` for an endpoint), not the stable
   `Notes`/`Examples`. **Keep each piece of content in exactly one bucket — never
   duplicate between stable and experimental.**
3. The handler must still refuse to run without the opt-in (returning a usage
   error) — the help gating is cosmetic, the runtime gate is the enforcement.

The mechanism (hide from plain `--help`, reveal tagged under
`--enable-experimental --help`, keep the machine surfaces complete) lives in
`runtime.experimentalEnabled` (`cli/root.go`), `RenderCommandHelp`
(`cli/help.go`), and the catalog's experimental handling (`cli/catalog.go`); it
is pinned by `TestHelpGatesExperimentalSurface` (`cli/help_test.go`). The MCP
`bot_runtime_reference` tool exists so an agent driving MCP can still discover
the experimental bot runtime.

## Cross-cutting contracts agents depend on

These are the load-bearing invariants. The mechanics live in code (pointers
below); what's written out here is the *rule* and the *why*.

### Wire format (`internal/apiclient`)

Signing, ordered param encoding, and envelope unwrap are documented in
`apiclient/doc.go`, with the primitives in `sign.go`, `encode.go`, and
`client.go`. Two facts worth stating explicitly because no code comment asserts
them:

- **The error envelope is the ONLY shape that exists:** `{"success": false,
  "error": {"code": <httpStatus>, "message": "SYMBOLIC_CODE", "description":
  "..."?}}`. The symbolic code is `error.message`; `error.code` is the HTTP
  status. There is **no** flat `{code, message}` shape. (Success is
  `{"success": true, "data": ...}`; bare acks normalize to `{"success": true}`.)
- **Money/quantity values are decimal strings end to end.** Validate the format
  and pass the original string through untouched — never parse to a float. (The
  one exception is the indicator layer; see the monitor section.)

Do not "fix" the signing order: the signature is computed over the exact
url-encoded string sent, `signature` excluded and appended last. The ordered
encoder (`encode.go`) exists precisely because `url.Values.Encode` sorts keys.

### Output & JSON stability (`internal/output`, `cli/humanout.go`)

stdout is human-readable by default (tables/kv, regardless of TTY); `--json`
opts into the machine contract (`--compact` implies `--json`). Errors follow the
same human/JSON split and the same exit codes (0/1/2/3/4). The rendering model
(endpoint formatters vs the `textout.TextFormatter` seam, the `{prog}`
substitution, money grouped on a display copy only) is documented at
`emitMode`/`endpointFormatters` in `cli/humanout.go` and in `cli/textout`.

**Output discipline (the stdout/stderr contract).** An agent must be able to
drive every command reading **stdout only**; stderr is a side channel it may
ignore entirely without losing anything it needs to act. Three rules, enforced
across *every* command (not just the ones that happen to have tests):

1. **Everything the agent needs lives in the result.** Result data, next steps,
   warnings, caveats, recovery guidance — all of it goes in the stdout result
   (and therefore in the `--json`/`--compact` document, where `FormatText` is not
   called, so the detail must be a **struct field**, not prose only). Patterns
   already in use: a `next []string` field for follow-up steps, a `warnings`
   field for non-fatal problems, in-band envelope flags (`acknowledgmentOnly`,
   `truncated`). On the **error path** the JSON error envelope goes to stderr (by
   convention, with a non-zero exit) — so recovery guidance that an error must
   carry rides the envelope's `guidance` field (`output.ApiError.Guidance`), not
   a separate stderr line.
2. **No duplicate on stderr.** If a detail is already in the stdout result (text
   or JSON), never also write it to stderr. stderr carries only: long-running
   **progress** (e.g. `keystore migrate` recovery steps), the always-on
   `signing as key` disclosure, `--debug`/`--log-level` logs, and the error line
   itself. `IO.Note`/`Notef` are for those cases — not happy-path narration.
3. **One document per single-shot command; many for an asynchronous one.** A
   single-shot command emits exactly one stdout document (one JSON document in
   JSON mode). A command that **works asynchronously** — that waits for or
   streams events — instead emits a sequence of documents over time, each a
   standalone JSON document in JSON mode: `setup --wait` (the awaiting/link
   document, then the final configured result) and `monitor` (one JSON event per
   line). `mcp serve`, `tui`, and interactive `setup` own stdout for their own
   protocol/UI and are outside this rule.

**What the CLI owns vs inherits** (the part that isn't obvious from any one
file): the `--json` success document is the API's own `data` passed through
**verbatim** (`json.RawMessage`, field order preserved — `output/output.go`,
pinned by `TestEmitJSONPreservesRawOrder`). So the CLI does **not** define most
of the success shape and **cannot** insulate a consumer from an upstream field
rename. What the CLI *does* own and keeps stable (each pinned by a test): the
**reshapes** it applies — bare-ack → `{"success":true}`, the `clientOrderId`
echo + full-fetched-order on `order place`, the paged-array merge, and the
truncation envelope — plus the error envelope (pinned by
`TestErrorEnvelopeExactShape`) and the `commands` catalog schema. `catalogVersion`
versions *these CLI-owned contracts*, not the upstream API's per-endpoint fields.
Extend all of them additively. Incomplete paged results say so in-band via the
`{"data":[...],"truncated":true,"note":"..."}` envelope (`wrapTruncated` in
`cli/endpoint.go`).

### Base-URL & key selection

- **Base-URL precedence** (highest first): `--base-url` flag → `DIGITALX_CLI_BASE_URL`
  → the signing key's per-key `baseUrl` → `config.json` `baseUrl` → the built-in
  production host. The parallel WebSocket precedence and the stored-tier guards
  are documented at `resolveBaseURL` / `resolveWSBaseURL` in `cli/endpoint.go`
  (WS derivation convention at `probe.DeriveWSBaseURL`). Keep this ordering —
  trading code depends on it.
- **Key selection** is `keys.Select` + `Manager.ResolveSelection` (the single
  front door for `endpoint`/`doctor`/`tui`/`monitor`/`mcp`): a stored key by
  name **XOR** an inline env credential, mutually exclusive, reading no secret.
  See `keys/select.go`. Keep one resolver authoritative.

### Clock: server-clock vs system-clock (read before touching any time code)

The mechanism — the asymmetric server window, `MeasureClockOffset`, the one
shared `clock.State`/`Syncer` (single-flight + cooldown), `--time-sync`, and the
`EXCEED_TIME_WINDOW` resync — is fully documented in `clock/doc.go`,
`apiclient/timesync.go`, and `apiclient/retry.go`, and the loop-safety
invariants are audited in `apiclient/doc.go`. The retry ladder's master gate is
each Operation's `OpMeta.Safety` (money-movers `nonIdempotent` = single-shot),
pinned by `TestCatalogSafetyClassification` in `ops/catalog_test.go`.

What is **not** captured in any single code comment, and is the easiest thing to
get wrong, is **when to use the server clock at all**:

> The server-clock estimate (`clock.SignNow`/`ServerNowMs`/`Offset`, and the
> `apiclient.Client`/`ops`/`botapi` seams over it) is a **scarce, special-purpose
> correction — NOT a general time source.** It is a network-measured estimate
> (±RTT/2 uncertainty, can jump on a mid-session resync, and is just the system
> clock with offset 0 on a run that never measures). Use it ONLY for a value
> that is sent to, gated by, or compared against the server's own clock:
>
> 1. **Signing requests** — the `timestamp`/`recvWindow` a signed call carries.
> 2. **Comparing against a server-returned timestamp** — ordering a REST
>    backfill row against live WS frames, or measuring delivery delay.
> 3. **A server-relative time filter** the server evaluates against its own rows.
>
> Everything else is a **system-clock** use (`time.Now`, or the `Deps.Now` test
> seam): journal row times, `Health.LastDataAt`, log lines, durations, backoff
> budgets, keepalive ticks. **Default to the system clock; make each server-clock
> use earn its place.** And **scope a server read to the purpose that justified
> it** — read the system clock separately for the local part, even if that means
> reading the time twice. (Concretely: the place protocol signs with `SignNow`
> but journals its order row from the system clock; the bot runtime exposes the
> server clock as `api.now()` yet stamps `LastDataAt` from the system clock.)

### Logging (`internal/logging`)

The full contract — the program-output-vs-operational-log split, the five
levels and why Error stays lean, the three line shapes, the layering/altitude
rule, and the never-log-secrets rule — is in `logging/doc.go`. The cli is the
only layer that *builds* a logger (`setupLogging`/`logger`/`surfaceLogger` in
`cli/root.go`, `logStyle` resolved once and shared with `Env.StreamLogger` in
`cli/clienv/clienv.go`); lower layers take an optional `*slog.Logger` and log
their own diagnostics. One non-obvious coupling worth stating: **`--debug` does
two things** — it lowers the log threshold to Debug *and* enables read
journaling (writes journal regardless) — and the two are decoupled (when both
are set, `--log-level` wins for the level).

### Action journal (`internal/journal`, `internal/callrec`)

A local SQLite DB recording write API calls plus a dedicated row per placed
order. The record/don't-record policy (writes always; reads only in
`--debug`), the lazy-open + pre-send hard-guarantee ordering, the no-secrets
rule, the `clientOrderId`-UNIQUE upsert with sticky `accepted` status, and the
WAL/`busy_timeout`/single-connection concurrency model are all documented in
`callrec/doc.go` and the package doc + `StartOrder`/`FinishOrder`/`Open` comments
in `journal/journal.go`. The cli wires every recorder through one site,
`rt.NewRecorder` (`cli/client.go`). `DIGITALX_CLI_NO_JOURNAL=1` opts out;
`--no-fsync` trades crash-durability for speed (and also opens the monitor bot DB
with `synchronous=OFF`).

### Idempotency, keys, secrets, funding

- **Idempotency:** a real `order place` always carries a `clientOrderId`
  (auto-minted UUIDv7 when omitted) and echoes it; `--dry-run` does **not**
  auto-mint one. The mint logic + rationale is commented at the place-mint block
  in `cli/endpoint.go`; the protocol is in `ops/op_place.go` and `ops/doc.go`.
- **Key soundness & secrets:** keys may belong to different Digital X accounts —
  never silently pick a different key (removing the default *unsets* it, never
  reassigns). Every private call prints `signing as key "<name>"` to stderr (a
  side-channel safety disclosure, not a log). Private key material lives only
  in the keystore backends and `apiclient/sign.go` and must never be printed,
  logged, or put in an error. The per-key two-axis (`type` + `keystore`) model,
  tolerant-on-load/strict-at-use loading, `key remove --force`, atomic writes,
  and the `keystore.Migrate` copy→verify→commit→delete ordering are documented in
  `keys/doc.go` and `keystore/doc.go`.
- **Funding is gated, not absent:** the full deposit/withdraw surface exists, but
  the fund-moving *writes* stay behind gates that are the product — the
  registration link omits the write-deposit/withdraw permission bits unless
  `--with-transfers` (`permTrading` vs `PermAll` in `cli/keymgmtcmd`), crypto
  withdrawals only target pre-registered addresses, and KRW transfers only send
  an app-confirmed push. Don't weaken these.
- **Stability is the product:** flags, command names, JSON shapes, and exit codes
  are public API for agents — extend additively, never rename or repurpose.
  Validation errors should say what to do instead (see `crossValidatePlace` in
  `ops/crossvalidate.go` for the tone).

## Commands with their own subsystems

Each of these is a short orientation plus a pointer to the authoritative doc.

### Key setup & `doctor`

`setup`/`key add` produce a fresh public key and a `registrationLink` (a
developers-portal deep link that pre-fills the key-creation form). The deep-link
mechanics (base64url-encoded public key, the `permTrading`/`PermAll` bitmask, IP
allowlist prefill, resume-vs-already-configured) are commented in `cli/keymgmtcmd`,
which owns the key/setup and `keystore` commands.
`doctor` (`cli/doctorcmd`) is a read-only health-check chain whose every step
carries a doc comment on its function (`doctorKeystoreChecks`, `doctorWSCheck`,
`doctorAccountSeqCheck`, `doctorDiagnoseAllowlist`); exit 0/4/1 after emitting
the report.

### `monitor` (`cli/monitorcmd` + `internal/botapi`)

The one **streaming** command (the deliberate exception to the one-JSON-document
rule), and a small bot runtime via `--init`/`--where`/`--on`. The resilient WS
layer it builds on is `internal/stream` — **read `stream/doc.go`** for the
recovery matrix, the "up to date or TOLD" property, and the notice/logging
contract before changing stream behavior. The bot runtime's threading model
(all JS on one event-loop goroutine; the dispatcher FIFO; the bounded worker
pool; `Close` drains the pool), the generated `api.*`/`db.*` API (with `korbit` bound to the same object as a permanent alias), the
`ta.*`/`state.*` synchronous globals, and the line-oriented output shapes are all
documented in the package doc of `botapi/botapi.go` and the comments in
`bindings.go`/`state.go`/`indicators.go`/`jserr.go`. The bot-scripting surface is
a **frozen public contract** pinned by the golden tests in
`botapi/surface_test.go` — extend additively, never rename. The stable `--jq`
path is `internal/jqfilter` (number-fidelity contract documented there). The
synthesized `--candles` channel (origin `derived` — the one non-verbatim
payload) is `internal/candles` — read the `Synth` doc in `candles/synth.go`
(guarantee inheritance, first-trade-frame-kicked seeds, the {interval, timestamp}
last-wins rule) before changing it. Two
facts not in code comments: there is no raw-call escape hatch — every endpoint
is a validated generated method (no `api.get`/`api.call`); and the agent
notes for `ta` flow from the `monitor` spec entry (`spec/registry.go`), the
single source feeding `--help`, the catalog, and the MCP reference. The exit-code
mapper is `RunError` in `cli/monitorcmd`.

### `mcp serve` (`cli/mcpcmd.go` + `cli/mcptool.go`)

A Model Context Protocol stdio server — another generated frontend over the ops
catalog, carrying no call policy of its own. Tool generation, the
onboarding/diagnostics tools (`setup`/`doctor`), the in-session
`refreshAfterKeyChange`, the one-server-one-key model, and the JSON-RPC output
contract are documented at their symbols in `mcpcmd.go`/`mcptool.go`. The
`digitalx_guide` tool (`makeGuideHandler`, fed by `agentskill.GuideContent`)
surfaces the bundled Skill's guidance for an MCP-only host that never loads the
skill the way Claude Code does — see "The bundled Skill" below. **The
load-bearing warning** (also at the call site): the raw `AddTool` path does
**not** validate arguments against the advertised `inputSchema` — the handler is
the sole enforcement point, so never drop a handler check believing the schema
enforces it.

### `tui` (`cli/tuicmd` + `internal/tui` + `internal/stream/state`)

The interactive full-screen trading terminal for humans (agents use `monitor`).
Its layering and load-bearing facts — the Trader goes through the same
validated/journaled path as `order place`/`cancel`, state reconciliation in
`internal/stream/state`, the single-goroutine store, visible reliability notices,
deliberate-stops-exit-0, the sidebar/focus model, dynamic active-symbol
subscription, and snapshot-only account backfill — are in
[`internal/tui/README.md`](internal/tui/README.md). Read that before changing
anything under `internal/tui/` or `internal/stream/state/`. Command-surface facts
are commented at `runTUI` in `cli/tuicmd`.

**Order open-vs-terminal classification** has one source of truth: the fixed
open-status set `openStatuses` in `internal/stream/state/state.go`. Open is
exactly `pending`/`open`/`partiallyFilled`; terminal is its complement (never
enumerate terminal statuses).

### `sandbox` (`internal/sandbox` + `cli/sandboxcmd`)

A lifecycle manager for the Digital X API Sandbox — a single-file local mock with a
real signature verifier, for full-stack signing/order-protocol checks without
touching production. All logic lives in `internal/sandbox` (+ `sandbox/deno`);
the cli layer only dispatches and formats. The detail — Deno-only/no-bundle-cache,
the detached-spawn + pidfile-as-source-of-truth + never-spawn-a-duplicate guard,
the min-version gate (`bringUpWithRecovery`) and the corrupt-cache self-heal
(`withBundleRecovery`, wrapping every bundle invocation `start` makes — for a
bundle URL that briefly served HTML; both refresh the bundle and retry once),
managed-Deno resolution, the
least-privilege `denoRunPerms` flag set, and the storage split — is documented at
its symbols across `sandbox/sandbox.go`, `lifecycle.go`, `runtime.go`,
`cache.go`, and `sandbox/deno/deno.go`. The sandbox-key safety predicates
(`IsSandbox`/`SandboxAPIKeyPrefix`/`AssertSandboxKeyName`/`SandboxNameToken`,
loopback-only, never-write-`config.json`) live in `keys/keys.go`. **License
conformance:** the bundle is **separately licensed** (Digital X Co., Ltd. proprietary, not
covered by this CLI's open-source license) — the CLI keeps use conformant by
fetching only from the Official Source, binding loopback-only, and never
redistributing it. Carry a pointer, never the license body; `sandbox license`
prints the terms straight from the bundle, and `cache_test.go` enforces
pointer-present/body-absent.

## The bundled Skill

`skills/digitalx-cli/` is the Agent Skill that consumes this CLI (`SKILL.md` plus
`references/`). It is mode-agnostic (CLI or `mcp serve`), organized around use
cases, defers the exhaustive flag/enum surface to `dgx-cli commands` / `--help`,
and deliberately **excludes** the experimental JS bot runtime. Shipping is via
`dgx-cli agent skill install`/`doctor` (embedded in the binary, idempotent,
self-cleaning, drift-checked by content hash) — see `cli/agentskillcmd` and
`internal/agentskill`. A copy of this same skill sitting at the legacy directory
name (`skills/korbit`) is removed by `agent skill install` once the current one
is written, and reported by `agent skill doctor`, so an agent never loads two
skills with the same triggers. Removal requires proof of ownership
(`agentskill.Managed`); a hand-written skill that merely names this CLI is
reported and left alone.

It reaches an MCP host two ways. Claude Code installs the skill files on disk
(above). A pure MCP host (Claude Desktop via the `.mcpb` bundle) never does, so
`mcp serve` exposes the **same** content through the read-only `digitalx_guide`
tool: `internal/agentskill/guide.go` reads the embedded tree (`SKILL.md` as
the overview, each `references/<topic>.md` as a topic) and the server's
`Instructions` nudge the model to call it. One embedded source feeds both paths,
so they cannot drift.

**Update the Skill when you change something it states explicitly:** the
output/error contract, exit codes, the order-sizing matrix, the `order place
--dry-run` simulation/warnings, clientOrderId/idempotency semantics, the
key-setup flow, the funding gates, or the journal/`logs` debugging playbook.

## Tests

`internal/**/*_test.go`, table-driven where it fits. CLI behavior is tested
end-to-end through `cli.Execute(args, Deps{...})` with injected
`Getenv`/`Stdout`/`Stderr`/`Doer` and a temp `DIGITALX_CLI_HOME` — **no process
spawning**. Signature tests verify the actual wire bytes the way the server does
(strip the `signature` segment, verify over the rest, in sent order). When you
change the command surface or wire handling, add the test in the matching
package; when you touch order placement, cover the sizing-matrix branch you changed.

## License headers

The project is Apache-2.0 (`LICENSE` + `NOTICE` at the repo root). Every source
file begins with a short header — a copyright line plus `// SPDX-License-Identifier:
Apache-2.0` — above any build constraint or package doc comment. Add it to any
new file; `TestEveryGoFileHasSPDXHeader` fails the build if a Go file is missing it.

## Third-party notices

The binary statically links open-source Go modules whose permissive licenses
require their notices to accompany every copy. `THIRD_PARTY_LICENSES.txt` (repo
root) carries them; it ships inside every release archive (the archive `files`
list in `.goreleaser.yaml`) and `dgx-cli license` links to it in the source repo.
The file is **generated** — regenerate with `make licenses` (see
`tools/licensegen`) after any dependency change and commit it;
`make licenses-check` fails on drift.
