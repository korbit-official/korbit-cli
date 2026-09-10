# Names, directories, and what the CLI does with them

`digitalx-cli` ships as the binary `dgx-cli` and keeps its data under
`~/.digitalx-cli`. An installation made under the earlier product name carries
different names — the command `korbit`, the home `~/.korbit-cli`, the artifact
cache `<user cache dir>/korbit-cli`, and the databases inside that home — and
**all of them keep working, indefinitely**.

**Nothing on disk is ever moved or renamed for you.** No command in this CLI
relocates a home, relocates a cache, or renames a file inside either — not
`self install`, not `self update`, not `self doctor`, and there is no flag that
asks for it. A home under the earlier name is simply the home, for as long as you
leave it there.

This document is the contract for how the two generations coexist: what the
rename changes for an install you already have, where the CLI looks for its data,
and — if you would rather use the current directory name — how to move it
yourself.

## Names

| Thing | Current | Earlier | Status of the earlier name |
|---|---|---|---|
| Command | `dgx-cli` | `korbit` | Kept as an **alias** on any install that has it — it runs the same binary, forever. Fresh installs get `dgx-cli` only. |
| CLI home | `~/.digitalx-cli` | `~/.korbit-cli` | Used as-is, indefinitely. Nothing moves it. |
| Home variable | `DIGITALX_CLI_HOME` | `KORBIT_CLI_HOME` | Honored. The canonical name wins when both are set. |
| Artifact cache | `<user cache dir>/digitalx-cli` | `<user cache dir>/korbit-cli` | Used as-is, indefinitely. Nothing moves it. |
| Cache variable | `DIGITALX_CLI_SANDBOX_CACHE` | `KORBIT_CLI_SANDBOX_CACHE` | Honored, same rule. |
| Every other variable | `DIGITALX_CLI_*` | `KORBIT_CLI_*` | Honored, same rule. |
| Release archive | `digitalx-cli_<os>_<arch>` | `korbit_<os>_<arch>` | Both are published; `self update` downloads the current one. |

## 1. What the rename changes for an install you already have

### Upgrading from a `korbit` install takes two `self update` runs

```sh
korbit self update    # 1. installs the current release, still under the `korbit` name
korbit self update    # 2. adds the `dgx-cli` command, pointing `korbit` at it
```

**The first update leaves you current but still `korbit`-only, because the code
that creates the `dgx-cli` command ships inside the release it is downloading.**
The second run is the new code, and it adds `dgx-cli`, reporting what it fixed
under `layoutRepaired` with `updated: false` — nothing was updated, only
repaired.

From then on **both command names stay current**: every install and update
places `dgx-cli` as the real binary and keeps `korbit` beside it as an alias
pointing at the same bytes. Either name runs the same CLI.

### The Agent Skill is renamed on install

`dgx-cli agent skill install` writes the skill under its current directory name
and removes a copy left under the earlier one, so an agent sees one skill rather
than two. That is a skill-directory rename inside the agent's own skills folder;
it does not touch your CLI home.

### The `.mcpb` bundle appears as a second extension

A Desktop Extension is keyed on the name in its manifest, so a bundle built under
the current name installs **beside** one you already have rather than replacing
it — two entries, two servers, two copies of every tool. Extensions have no
self-update path, so remove the older one by hand from your host's extension
list.

## 2. Where the CLI looks for its home and its cache

### The CLI home

In order, first match wins:

1. `$DIGITALX_CLI_HOME`, when set
2. `$KORBIT_CLI_HOME`, when set
3. `~/.digitalx-cli`, when that directory exists
4. `~/.korbit-cli`, when that directory exists
5. `~/.digitalx-cli` — created on first write

Note steps 3 and 4: with **both** directories present, the CLI reads
`~/.digitalx-cli` and the other one is invisible to it. `self doctor` reports
that (see [section 5](#5-what-self-doctor-reports)).

### The artifact cache

The same shape: `$DIGITALX_CLI_SANDBOX_CACHE`, then
`$KORBIT_CLI_SANDBOX_CACHE`, then `<user cache dir>/digitalx-cli` if it exists,
then `<user cache dir>/korbit-cli` if it exists, else
`<user cache dir>/digitalx-cli`. The cache holds only regenerable downloads (the
managed Deno runtime and its module cache), so which one is in use costs nothing
either way.

### Files inside the CLI home

The files under the home carry **no product name at all** — the directory
already says whose data they are:

| Home-relative path | What it is | Files that belong beside it |
|---|---|---|
| `journal.db` | the action journal | `-wal`, `-shm` |
| `bot.db` | the `monitor` bot runtime's script-local database | `-wal`, `-shm` |
| `sandbox/sandbox.db` | the local API sandbox's database | `-wal`, `-shm`, `-pid`, `.market-snapshot.json` |
| `debug-<timestamp>.json` | a diagnostic bundle `debug bundle` wrote for you | — |
| `config.json`, `keys.json`, `keystore.json`, `install.json`, `self.lock`, `sandbox/run.log` | config, keys, and bookkeeping | — |

There is exactly **one** exception, and it is one rule: **in a home whose own
directory name is `.korbit-cli`, each of those databases keeps its earlier name.**

| In a home named `.korbit-cli` | In every other home |
|---|---|
| `korbit-cli.db` (+ `-wal`, `-shm`) | `journal.db` (+ `-wal`, `-shm`) |
| `korbit-bot.db` (+ `-wal`, `-shm`) | `bot.db` (+ `-wal`, `-shm`) |
| `sandbox/korbit-sandbox.db` (+ `-wal`, `-shm`, `-pid`, `.market-snapshot.json`) | `sandbox/sandbox.db` (+ `-wal`, `-shm`, `-pid`, `.market-snapshot.json`) |

Nothing else in the CLI consults the earlier names. That single rule is what lets
an older `korbit` binary still sharing `~/.korbit-cli` read and write the same
files as a current one: both derive the same names from the same directory.

The rule reads the directory's **own name**, resolved to an absolute path first —
so a home reached by a relative path, or (on a filesystem that ignores case)
spelled `~/.KORBIT-CLI`, is the same home with the same file names, not a second
layout in the same directory. A home you pin with `DIGITALX_CLI_HOME` or
`KORBIT_CLI_HOME` follows it too: pinned at a directory named `.korbit-cli` it
uses the earlier names, pinned anywhere else it uses the current ones.

**The directory name and the file names are one unit.** Renaming the directory
without renaming the files inside it produces a full home the CLI reads as empty:
it would open fresh, empty databases beside your real ones. That is why the
manual recipe below does both, in one go, and why `self doctor` reports the
half-done state as a problem.

## 3. Nothing is moved for you

- **`self install`** (what the install one-liner runs) places the binary, wires
  `PATH`, and writes the manifest. It moves no directory, renames no file in your
  home, and deletes nothing of yours. Re-running it repairs a broken install and
  nothing else.
- **`self update`** downloads a release, replaces the binary in place, and keeps
  the command layout correct (`dgx-cli` real, `korbit` pointing at it). It moves
  no directory and renames no file.
- **`self doctor`** is read-only by contract. It changes nothing, ever.
- **`self uninstall`** removes; it never renames or relocates. See
  [section 4](#4-moving-to-the-new-directory-name-optional).

An install left on `~/.korbit-cli`, with its earlier database names and its
earlier-named artifact cache, is a **fully supported state with no deadline on
it**. Both directory names are read for good.

## 4. Moving to the new directory name (optional)

You never have to do this. Two ways, if you want to.

### Option A — start fresh: uninstall, then reinstall

This throws your data away and sets the CLI up again from scratch. Choose it when
you have nothing in the home worth keeping (or have exported what you need).

```sh
dgx-cli self uninstall     # interactive; see exactly what it does below
# then re-run the install one-liner, and:
dgx-cli setup
```

`self uninstall` is **interactive only**: it needs a terminal, and refuses to run
piped, redirected, from an agent, or with `--json` / `--compact`. It takes no
flags of its own; the global `--dry-run` runs the same questions and changes
nothing, reporting only what your answers would have done.

It asks **three separate questions**, and nothing is touched until you answer.
Answer them independently — this is exactly what each one covers:

| Question | Default | Removes | Keeps |
|---|---|---|---|
| the `dgx-cli` binary and install manifest | **yes** | the installed binary, every alias command name it owns (including `korbit`), `install.json`, and leftover swap files | a file at the `korbit` name it cannot prove is its own |
| config, API keys, and action journal | **no** | `config.json`, `journal.db`/`korbit-cli.db`, `bot.db`/`korbit-bot.db` (with their `-wal`/`-shm`), `debug-*.json` bundles, and — after clearing each key from its backend, OS keychain included — `keys.json` and `keystore.json` | everything, if you answer no |
| regenerable caches | **no** | the `sandbox/` state directory in each home and the shared Deno/module cache under **both** cache names, stopping a running sandbox first | everything, if you answer no |

Then, for each shell startup file carrying the installer's `PATH` block, it shows
a diff and asks (default **no**) whether to remove **just that block** — it never
deletes the file. On Windows it removes only the digitalx-cli entry from your User
`PATH`. A file you decline is reported so you can edit it yourself.

Three things worth knowing:

- **Every home is offered**, not just the one in use: `~/.digitalx-cli` and
  `~/.korbit-cli` whenever they exist, plus a pinned one. Within each home, every
  database is listed under **both** spellings, so a home renamed without its files
  is cleaned out completely.
- **The home directory itself is removed only once it is empty.** If you keep the
  data, or keep the caches, or a removal failed, the directory stays — those are
  deliberate keeps, not failures.
- **Removing keys cannot be undone.** Keys are cleared from their backend
  (including the OS keychain) before any key file is deleted, so nothing is
  orphaned — but they are gone.

On Windows the running `.exe` is locked and cannot delete itself; it is reported
for you to delete manually.

### Option B — move it by hand

This keeps everything. Do the four steps in order.

**1. Stop every process using that home**, under both command names — a running
sandbox, `monitor`, `tui`, or `mcp serve` holds the databases open, and renaming
a file out from under a live process corrupts what it was doing:

```sh
dgx-cli sandbox stop      # and `korbit sandbox stop` if that command exists
# then quit any running `monitor`, `tui`, or `mcp serve`
```

**2. Check that `~/.digitalx-cli` does not already exist.** `mv` onto an existing
directory **nests** the source inside it instead of merging — you would end up
with `~/.digitalx-cli/.korbit-cli`. If both exist, decide which one you want and
deal with the other first.

```sh
ls -d ~/.digitalx-cli     # must say "No such file or directory"
mv ~/.korbit-cli ~/.digitalx-cli
```

PowerShell:

```powershell
Test-Path ~\.digitalx-cli    # must be False
Move-Item ~\.korbit-cli ~\.digitalx-cli
```

**3. Rename the three databases and their sidecars**, because the new directory
name means the CLI now looks for the current filenames. A file you skip here is a
file the CLI will not open.

```sh
cd ~/.digitalx-cli
for pair in "korbit-cli.db journal.db" "korbit-bot.db bot.db" "sandbox/korbit-sandbox.db sandbox/sandbox.db"; do
  set -- $pair
  for suffix in "" -wal -shm -pid .market-snapshot.json; do
    [ -e "$1$suffix" ] && mv "$1$suffix" "$2$suffix"
  done
done
```

PowerShell:

```powershell
cd ~\.digitalx-cli
$pairs = @{
  'korbit-cli.db'                = 'journal.db'
  'korbit-bot.db'                = 'bot.db'
  'sandbox\korbit-sandbox.db'    = 'sandbox\sandbox.db'
}
foreach ($from in $pairs.Keys) {
  foreach ($suffix in @('', '-wal', '-shm', '-pid', '.market-snapshot.json')) {
    if (Test-Path "$from$suffix") { Move-Item "$from$suffix" "$($pairs[$from])$suffix" }
  }
}
```

(`-pid` and `.market-snapshot.json` belong only to the sandbox database; the loop
skips what is not there. `-wal` and `-shm` are SQLite's write-ahead log and its
shared-memory index — a database moved without its `-wal` loses whatever had not
been folded into it yet.)

**4. Delete the old artifact cache.** It holds only regenerable downloads, and
the new location refills itself on the next sandbox run:

| OS | Old cache |
|---|---|
| Linux | `~/.cache/korbit-cli` |
| macOS | `~/Library/Caches/korbit-cli` |
| Windows | `%LOCALAPPDATA%\korbit-cli` |

```sh
rm -rf ~/.cache/korbit-cli          # or ~/Library/Caches/korbit-cli on macOS
```

```powershell
Remove-Item -Recurse -Force $env:LOCALAPPDATA\korbit-cli
```

**A pinned home overrides all of this.** If `DIGITALX_CLI_HOME` or
`KORBIT_CLI_HOME` is set, that path is the home whatever is in `~`, and moving
`~/.korbit-cli` changes nothing for you until you change the variable. Note also
that a pinned directory follows the same file-name rule by its **own basename**:
pinned at a directory named `.korbit-cli` it keeps the earlier filenames, which is
the right layout if a pre-rename `korbit` is still sharing it.

Finally, verify:

```sh
dgx-cli self doctor
```

## 5. What `self doctor` reports

`self doctor` reports a **problem** for something broken (it exits non-zero) and
a **note** for something that works but that a command would tidy up (it stays
exit 0). Every problem carries a stable `field` tag:

| `field` | Reports | Fixed by |
|---|---|---|
| `managed` | no install manifest, or a corrupt one | the install one-liner |
| `binary` | the installed binary is missing, or does not match the manifest — including the case where the running `korbit` **does** match it and the `dgx-cli` beside it does not, so the documented command name runs bytes this install never placed | the install one-liner, or `self update` for the mismatched-`dgx-cli` case |
| `alias` | the `korbit` command is missing, points at something other than `dgx-cli`, or is a stale copy of another version | `self update` |
| `path` | the install directory is not on your `PATH` | the install one-liner |
| `home` | one of the two states below | you, by hand |

Only **two** states about the two generations of directory names are ever
reported, and both mean you have data the CLI is not reading:

- **Two CLI homes exist and the one not in use holds data.** The report names
  which one is in use, so you know which half is invisible. When the home in use
  holds no keys, config, or journal at all while the other one does, it says so
  explicitly — that is not ambiguity but a certainty. **Remedy:** move what you
  need across, or remove the directory you do not want, following
  [option B](#option-b--move-it-by-hand).
- **The home in use holds databases under the earlier file names** while its own
  directory is not named `.korbit-cli` — a directory renamed without its files.
  The report names the files and the rename each one needs. **Remedy:** step 3 of
  [option B](#option-b--move-it-by-hand); or, for a pinned home, point the
  variable at a directory named `.korbit-cli` instead, which keeps the earlier
  layout with nothing to rename.

Everything else about the earlier names is **silent**. A home at `~/.korbit-cli`
holding its earlier-named databases is correct, not a finding; so is an artifact
cache under the earlier name; so is a `.korbit-cli` home that happens to hold a
stray `journal.db`, since the earlier names are the right ones there.

## What never changes

- **The `korbit` command name.** An install that has it keeps it, permanently.
  It is refreshed on every install and update so it never falls behind.
- **A file at the `korbit` name that this install did not place.** Ownership
  must be proved (the manifest records it, it is the manifest's own executable,
  it is the running binary, or its bytes hash to the manifest's SHA-256) before
  anything overwrites or deletes it. Your own wrapper script at that name is
  reported and left exactly as it is.
- **An install left on the earlier directory names.** `~/.korbit-cli`, its
  earlier database names, and the artifact cache under the earlier name are read
  for good. Using the current names is a choice you make and carry out, not
  something that eventually happens to you.
- **Key compatibility.** Nothing about how keys are stored, encrypted, or read
  changes: a keystore written by either generation is read by both, and an OS
  keychain item is never re-created or re-encrypted.
- **Environment variables.** Both spellings are honored indefinitely; the
  canonical `DIGITALX_CLI_*` name wins when both are set — and only when its
  value is non-empty. Exporting the canonical name as an empty string reads as
  unset and falls through to the legacy one, so to turn a setting off, unset the
  legacy name too.
- **A pinned home, directory and contents.** A home fixed with
  `DIGITALX_CLI_HOME` or `KORBIT_CLI_HOME` keeps whatever
  [file-name layout](#files-inside-the-cli-home) its own basename implies,
  permanently.
- **The legacy release archive and bundle URLs**, which already-installed
  versions fetch by name. Every historical tag's `korbit_*` archives stay
  downloadable at the URLs they always had.

## Reading the result as JSON

Every outcome above is a field, not prose, so `--json` is enough to act on:

- `self update` → `layoutRepaired` (the command-name fixes this run made, with
  `checkedOnly` telling you whether they were applied or merely planned) and
  `next` (follow-up steps). There is no migration field, because there is no
  migration.
- `self install` → `repaired`, `path`, and `next`.
- `self doctor` → `problems[].field` for anything broken, `notes` for the
  healthy-but-tidiable states, plus `home` and `legacyHome` so the directories
  need no parsing out of prose.

## For maintainers

Every release is published to **both** release repositories, each with its own
archive set and both with the same signed `checksums.txt`, which is what lets an
installed CLI of either generation update itself and verify what it fetched:
[`RELEASING.md`](https://github.com/digitalx-official/digitalx-cli/blob/master/RELEASING.md).
