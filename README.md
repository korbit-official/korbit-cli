# korbit-cli

**English** · [한국어](README.ko.md)

**[Korbit Developer Portal](https://developers.korbit.co.kr/)** · [API Docs](https://docs.korbit.co.kr/)

A command-line client for the [Korbit Open API v2](https://docs.korbit.co.kr/) — market data, trading, and API-key management for the Korbit cryptocurrency exchange, in a single self-contained binary.

`korbit-cli` is **agent-first**: built so AI agents can interface with Korbit reliably on your behalf — through an [Agent Skill or MCP](#use-it-from-an-ai-agent) — and just as usable from your own shell.

## Why korbit-cli?

- **Single binary.** One static Go binary — no runtime, no dependencies.
- **Safe by default.** Order sizing, decimal strings, enum values, and id rules are validated before anything is sent, and order placement is idempotent, so a retry can never place a second order.
- **Keys handled for you.** Onboard through a registration link — no secret to paste anywhere. The private key is generated locally and never leaves your machine, with optional macOS Keychain storage.
- **Human and script friendly.** Every command prints a clean human view; add `--json` (or `--compact`) for one machine-readable document, with predictable exit codes.

## Install

One line downloads the latest release for your platform, verifies its SHA-256, and puts `korbit` on your `PATH`:

```sh
# Linux / macOS
curl -fsSL https://docs.korbit.co.kr/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://docs.korbit.co.kr/install.ps1 | iex
```

The binary then manages itself — `korbit self update`, `korbit self doctor`, `korbit self uninstall`.

Prefer to manage it yourself? Download a release binary, or install from source with `go install github.com/korbit-official/korbit-cli@latest`.

## Quickstart

```sh
# 1. Generate an ED25519 keypair and start setup. The private key stays in the local keystore.
korbit setup --name trading-bot

# 2. `setup` prints a registration link — open it, review the prefilled public key,
#    permissions, and IP allowlist, and confirm with MFA. setup then detects the new key,
#    binds it, and runs a health check automatically.
```

Then trade:

```sh
korbit ticker btc_krw
korbit balance --currencies krw,btc
korbit order place --symbol btc_krw --side buy --type limit --price 100000000 --qty 0.001
korbit order get --symbol btc_krw --client-order-id <id from the place output>
korbit order cancel --symbol btc_krw --order-id 123456
```

## What you can do

| Area | Commands |
|---|---|
| Market data (public) | `ticker` `orderbook` `trades` `candles` `pairs` `ticksize` `currencies` `time` |
| Orders & fills | `order place` `order get` `order cancel` `order open` `order history` `fills` |
| Account | `balance` `fees` `whoami` |
| Deposits & withdrawals | `deposit …` `withdraw …` `krw deposit/withdraw …` |
| Real-time | `tui` `monitor` |
| Keys & setup | `setup` `doctor` `ip` `key …` `keystore …` |
| Local sandbox | `sandbox …` |
| Meta | `commands` `logs` `license` `mcp serve` |

Run `korbit --help` for the full list and `korbit <command> --help` for the details of any command.

### Terminal dashboard

`korbit tui` opens a full-screen, interactive dashboard — the quickest way to watch the Korbit market from your terminal. It streams live prices, the order book, recent trades, and a candlestick chart, and switches symbols as you browse. Signed in with your key, it also shows your balances and open orders updating live. Add `--public` for a market-only view that needs no API key.

```sh
korbit tui                     # your balances and orders alongside the market
korbit tui --public            # market only — no API key needed
```

### Real-time stream

`korbit monitor` streams the WebSocket API as one JSON object per line — public market data and, signed with your key, your own orders, trades, and balances. It reconnects and backfills gaps automatically, can filter or reshape the output with a built-in `--jq` program, and can synthesize real-time OHLCV candles (`--candles`) that the WebSocket API itself does not offer.

```sh
korbit monitor --symbols btc_krw --ticker --json
korbit monitor --symbols btc_krw --candles 1,60 --candle-history 100 --json
korbit monitor --symbols btc_krw --my-orders --my-assets --json
```

## Use it from an AI agent

`korbit-cli` is designed to be driven by an AI agent as well as by you, via two integration paths that share the same validated, journaled, safe command surface.

- **Agent Skill** — teaches an agent to operate the CLI safely across research, monitoring, trading, funding, setup, and debugging. Install it with `korbit agent skill install --all`, then `korbit agent skill doctor` to verify.
- **MCP server** — `korbit mcp serve` exposes every endpoint as a [Model Context Protocol](https://modelcontextprotocol.io) tool for hosts like Claude and Codex. Add `--read-only` for market data only.

```sh
# Register the MCP server (one key per server)
claude mcp add --transport stdio korbit -- korbit mcp serve --key trading
codex  mcp add korbit -- korbit mcp serve --key trading
```

For Claude Desktop without a terminal, install the server as a drag-and-drop [`.mcpb` bundle](https://github.com/anthropics/mcpb) from the release page — it includes the binary.

## Test without real money

- **Dry-run** — add `--dry-run` to any command to print the exact request that would be sent, without sending it. On `order place` it also runs a customer-protection preflight that warns about risky orders (slippage, fat-finger prices, unfillable sizes, and more).
- **Local sandbox** — `korbit sandbox start` runs a single-file local mock of the API with a real signature verifier, so you can exercise signing and the full order flow without touching production or real funds:

```sh
korbit sandbox start          # download + run the sandbox, import its seeded key
korbit whoami --key sandbox    # signed calls now hit the local mock
korbit sandbox stop
```

Add `--paper --fresh` to `sandbox start` for **paper trading**: market data is mirrored live from production Korbit while fills stay simulated — real prices, no real money. By default only the bundle's built-in fixture pairs are seeded; add `--all-pairs` (`korbit sandbox start --paper --all-pairs --fresh`) to seed **every launched production pair** from a live snapshot, so the sandbox carries production's tradable pair set. The first such start takes a few seconds longer; the snapshot is then cached beside the database, so repeated starts (including `--fresh`) reseed in well under a second. `--fresh` recreates the disposable sandbox database, which is also how you switch back; to **restart** an existing paper sandbox keeping its balances and orders, run `korbit sandbox start --paper` without `--fresh` (`korbit sandbox start --help` has the detail).

The sandbox bundle is separately licensed (software of Digital X Co., Ltd.) — `korbit sandbox license` prints the terms; use it for local development and testing only.

## Safety

- **Idempotent placement.** Every `order place` carries a `clientOrderId` (auto-minted and echoed). Reuse it via `--client-order-id` to retry the *same* order — Korbit processes it once, and the CLI reconciles rather than blindly resending.
- **Client-side sizing.** `limit` → `--price` + `--qty`; market/best **buy** → `--amt` (KRW to spend); market/best **sell** → `--qty`. Amounts are decimal strings, passed through untouched — no floating point.
- **No accidental double-spends.** Reads and cancels auto-retry on transient errors; money-moving writes (`order place`, withdrawals, KRW transfers) are never auto-retried.

## Keys & storage

You can add multiple keys; select one per command with `--key` (or set a default with `korbit key use`). Both ED25519 and HMAC-SHA256 keys are supported. Private keys are stored encrypted on disk by default, or in your OS keychain, and all state lives under `~/.korbit-cli/`. Manage keys with `korbit key …` and move them between backends with `korbit keystore migrate`.

## License

Copyright © 2026 Digital X Co., Ltd.

Licensed under the Apache License, Version 2.0 (`SPDX-License-Identifier: Apache-2.0`). See [`LICENSE`](LICENSE) for the full text.

**Disclaimer** — read [`DISCLAIMER.md`](DISCLAIMER.md) before using this tool.

The local sandbox bundle (`korbit sandbox …`) is **not** covered by this license — it is proprietary software of Digital X Co., Ltd. under its own separate terms. The CLI only downloads and runs it from the Official Source; read its terms with `korbit sandbox license`.
