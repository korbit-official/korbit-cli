# Testing without real money: the local sandbox

The tool can spin up a **local mock of the Korbit API** — a full-stack check of signing, the order
protocol, and envelope handling that never touches production. Use it to rehearse a flow (especially
order placement and any untested strategy logic) before running it live.

> The sandbox is a **mock** — passing here does not guarantee production behavior. Verify against
> production with small sizes before trusting a flow with real funds.

## Spin up → test → tear down

```sh
export DIGITALX_CLI_HOME=$(mktemp -d)    # isolate this run (also lets parallel agents not collide)

korbit sandbox start --fresh           # run the mock detached on a CLEAN database, import a seeded key
                                       # (drop --fresh to keep an existing sandbox's balances/orders)
korbit whoami --key sandbox --json     # verify the imported key works
korbit balance --key sandbox --json

# Shape scenario state via the bundle's own subcommands (forwarded verbatim):
korbit sandbox exec set-balance --user 1 --currency krw --available 100000000

# Run test trades — you MUST pass --key sandbox explicitly (see safety note):
korbit order place --key sandbox --symbol btc_krw --side buy --type limit --price 10000000 --qty 0.001 --json
korbit logs --orders --json            # inspect what landed (journaled exactly like production)

korbit sandbox stop                    # graceful shutdown
```

Other lifecycle commands: `korbit sandbox status` (source, runtime, server state, imported key),
`korbit sandbox update` (refresh the mock bundle), `korbit sandbox license` (print the bundle's own
terms). On first run, `sandbox start` downloads a small managed runtime into a shared cache — that's
expected; subsequent starts are fast.

## Paper trading (real market data, simulated fills)

`korbit sandbox start --paper --fresh` starts a clean database mirroring **live production market
data** — real prices, order book, and trades — while order fills stay locally simulated (no real
money; needs network). Pairs production lists as not launched stay on the simulated walk; the start
result names them in `walkPairs`. Use it to test a strategy against real market dynamics instead of
the default simulated walk. By default only the bundle's built-in fixture pairs are seeded; add
`--all-pairs` (`korbit sandbox start --paper --all-pairs --fresh`) to seed every LAUNCHED production
pair from a live snapshot, so the sandbox carries production's tradable pair set. The first such
start fetches a tick-size policy per pair (a few seconds); the snapshot is then cached beside the
database, so repeated starts (including `--fresh`) reseed in well under a second. The mode is set at database creation, so `--fresh` IS the mode switch
(in either direction — `korbit sandbox start --fresh` alone returns to the simulated walk). To
**restart** an existing paper sandbox keeping its balances and orders, run
`korbit sandbox start --paper` without `--fresh`; to switch modes without wiping data, flip one pair
with `korbit sandbox exec set-market --symbol btc_krw --source live`. Full detail lives in the
sandbox's own help (`korbit sandbox exec help`); `korbit sandbox exec status` shows each pair's
market mode and paper-trading health (mirror age, live-feed state). Fills are approximations — they
consume only the sandbox's local view of the book (each displayed quantity fills at most once until
the next book update; the real market is untouched) and queue position is not modeled — so paper
results overestimate fill quality; the production-verification rule above still applies.

## Safety properties to rely on

- **The sandbox key is never the default and is loopback-only.** It's pinned to `127.0.0.1`, stored
  in the file keystore, and bound to a `SANDBOX_…` api-key id, so a bare command can never
  accidentally hit the mock — you must pass `--key sandbox` (or your `--key-name`) every time. That
  also means real commands without `--key sandbox` still go to production, so don't assume "I started
  a sandbox" routes anything.
- **A sandbox key's name must contain "sandbox"** — so every sandbox command is self-evident on the
  command line.
- **`config.json` is never touched** by sandbox commands, and a sandbox start refuses to clobber a
  real key of the same name.

The sandbox is for the **shell** workflow; an MCP-host agent can't drive `sandbox` (it's not a tool) —
do sandbox testing in a terminal.
