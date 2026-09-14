# Upgrading from `korbit` to `dgx-cli`

**English** · [한국어](MIGRATION.ko.md)

**v1.2.0** renames the CLI: command `korbit` → `dgx-cli`, home `~/.korbit-cli` →
`~/.digitalx-cli`, variables `KORBIT_CLI_*` → `DIGITALX_CLI_*`. **Nothing breaks,
nothing moves, everything keeps working:** every earlier name stays honored for
good, and no command in this CLI ever moves or renames anything on your disk.

## Does this affect me?

| You… | Do this |
|---|---|
| installed `dgx-cli` fresh | Nothing. This document is not for you. |
| have a `korbit` install | Run `korbit self update` **twice** ([why twice](#run-self-update-twice)). Your data stays where it is and keeps working. |
| set `KORBIT_CLI_*` variables | Nothing. They stay honored; `DIGITALX_CLI_*` wins when both are set. |
| installed the `.mcpb` Desktop Extension | Remove the earlier extension by hand — the new one [installs beside it](#the-mcpb-bundle-appears-as-a-second-extension). |
| want `~/.digitalx-cli` as your home | Optional, and by hand — [two ways](#moving-to-the-new-directory-name-optional). |

## What changed

| Thing | Current | Earlier | The earlier name is… |
|---|---|---|---|
| Command | `dgx-cli` | `korbit` | Kept forever, as an alias of the same binary, on any install that has it. Fresh installs: `dgx-cli` only. |
| CLI home | `~/.digitalx-cli` | `~/.korbit-cli` | Used as-is, forever. Never moved. |
| Home variable | `DIGITALX_CLI_HOME` | `KORBIT_CLI_HOME` | Honored. Current name wins when both are set. |
| Artifact cache | `<user cache dir>/digitalx-cli` | `<user cache dir>/korbit-cli` | Used as-is, forever. Never moved. |
| Cache variable | `DIGITALX_CLI_SANDBOX_CACHE` | `KORBIT_CLI_SANDBOX_CACHE` | Honored. Same rule. |
| Every other variable | `DIGITALX_CLI_*` | `KORBIT_CLI_*` | Honored. Same rule. |
| Release archive | `digitalx-cli_<os>_<arch>` | `korbit_<os>_<arch>` | Published once — in the release a `korbit` install updates through. Later releases: current name only. |

## Upgrading a `korbit` install

### Run `self update` twice

```sh
korbit self update    # 1. installs the current release, still under the `korbit` name
korbit self update    # 2. adds the `dgx-cli` command, pointing `korbit` at it
```

**Why twice:** the code that creates the `dgx-cli` command ships inside the
release the first run downloads. So the first run leaves you current but still
`korbit`-only; the second run is that new code, and it adds `dgx-cli`, reporting
the fix under `layoutRepaired` with `updated: false` — nothing updated, only
repaired.

From then on **both names stay current**: every install and update places
`dgx-cli` as the real binary and keeps `korbit` beside it as an alias pointing at
the same bytes. Either name runs the same CLI.

### The Agent Skill is renamed on install

`dgx-cli agent skill install` writes the skill under its current directory name
and removes a copy left under the earlier one, so an agent sees one skill, not
two. That rename happens inside the agent's own skills folder; your CLI home is
not touched.

### The `.mcpb` bundle appears as a second extension

A Desktop Extension is keyed on the name in its manifest, so a bundle built under
the current name installs **beside** the one you have rather than replacing it —
two entries, two servers, two copies of every tool. Extensions have no
self-update path: remove the earlier one by hand from your host's extension list.

## No command moves your data

**Nothing on disk is ever moved or renamed for you** — not a home, not a cache,
not a file inside either — and no flag asks for it. A home under the earlier name
is simply the home, for as long as you leave it there.

| Command | Does | Never |
|---|---|---|
| `self install` (what the install one-liner runs) | places the binary, wires `PATH`, writes the manifest; a re-run repairs a broken install, nothing else | a directory move, a file rename in your home, a deletion of anything of yours |
| `self update` | downloads a release, swaps the binary in place, keeps the command layout right (`dgx-cli` real, `korbit` pointing at it) | a directory move, a file rename |
| `self doctor` | reports — read-only by contract | any change at all |
| `self uninstall` | deletes only what you confirm ([details](#option-a--start-fresh-uninstall-then-reinstall)) | a rename, a relocation |

## Where the CLI looks for its home and its cache

### The CLI home

In order — first match wins:

1. `$DIGITALX_CLI_HOME`, when set
2. `$KORBIT_CLI_HOME`, when set
3. `~/.digitalx-cli`, when that directory exists
4. `~/.korbit-cli`, when that directory exists
5. `~/.digitalx-cli` — created on first write

Steps 3 and 4 mean that with **both** directories present, the CLI reads
`~/.digitalx-cli` and the other one is invisible to it. `self doctor`
[reports that](#what-self-doctor-reports).

### The artifact cache

Same shape — first match wins:

1. `$DIGITALX_CLI_SANDBOX_CACHE`, when set
2. `$KORBIT_CLI_SANDBOX_CACHE`, when set
3. `<user cache dir>/digitalx-cli`, when it exists
4. `<user cache dir>/korbit-cli`, when it exists
5. `<user cache dir>/digitalx-cli`

The cache holds only regenerable downloads (the managed Deno runtime and its
module cache), so which one is in use costs nothing either way.

### Files inside the CLI home

The files carry **no product name** — the directory already says whose data they
are:

| Home-relative path | What it is | Sidecars |
|---|---|---|
| `journal.db` | the action journal | `-wal`, `-shm` |
| `bot.db` | the `monitor` bot runtime's script-local database | `-wal`, `-shm` |
| `sandbox/sandbox.db` | the local API sandbox's database | `-wal`, `-shm`, `-pid`, `.market-snapshot.json` |
| `debug-<timestamp>.json` | a diagnostic bundle written by `debug bundle` | — |
| `config.json`, `keys.json`, `keystore.json`, `install.json`, `self.lock`, `sandbox/run.log` | config, keys, bookkeeping | — |

**One exception, one rule: in a home whose own directory name is `.korbit-cli`,
each database keeps its earlier name.**

| In a home named `.korbit-cli` | In every other home |
|---|---|
| `korbit-cli.db` (+ `-wal`, `-shm`) | `journal.db` (+ `-wal`, `-shm`) |
| `korbit-bot.db` (+ `-wal`, `-shm`) | `bot.db` (+ `-wal`, `-shm`) |
| `sandbox/korbit-sandbox.db` (+ `-wal`, `-shm`, `-pid`, `.market-snapshot.json`) | `sandbox/sandbox.db` (+ `-wal`, `-shm`, `-pid`, `.market-snapshot.json`) |

- **Why:** an earlier `korbit` binary still sharing `~/.korbit-cli` reads and
  writes the same files as a current one — both derive the same names from the
  same directory. Nothing else in the CLI consults the earlier names.
- **What counts as the name:** the directory's **own** basename, after resolving
  to an absolute path. A home reached by a relative path, or spelled
  `~/.KORBIT-CLI` on a case-insensitive filesystem, is the same home with the
  same file names — not a second layout in the same directory.
- **A pinned home follows it too.** Pinned with `DIGITALX_CLI_HOME` or
  `KORBIT_CLI_HOME` at a directory named `.korbit-cli`, it uses the earlier
  names; pinned anywhere else, the current ones.
- **Directory name and file names are one unit.** A directory renamed without
  its files is a full home the CLI reads as empty: it opens fresh, empty
  databases beside your real ones. The [manual recipe](#option-b--move-it-by-hand)
  therefore does both in one go, and `self doctor` reports the half-done state
  as a problem.

## Moving to the new directory name (optional)

You never have to do this. Two ways, if you want to.

### Option A — start fresh: uninstall, then reinstall

Throws your data away and sets the CLI up again from scratch. Choose it when
nothing in the home is worth keeping (or you have exported what you need).

```sh
dgx-cli self uninstall     # interactive; see exactly what it does below
# then re-run the install one-liner, and:
dgx-cli setup
```

`self uninstall` is **interactive only**: it needs a terminal and refuses to run
piped, redirected, from an agent, or with `--json` / `--compact`. It has no flags
of its own; the global `--dry-run` asks the same questions, changes nothing, and
reports what your answers would have done.

It asks **three separate questions**. Nothing is touched until you have
answered, and **no** keeps everything in that row.

| Question | Default | Yes removes | Kept regardless |
|---|---|---|---|
| `dgx-cli` binary and install manifest | **yes** | the binary; every alias it owns, `korbit` included; `install.json`; leftover swap files | a `korbit` file it cannot prove is its own |
| config, API keys, and action journal | **no** | `config.json`; `journal.db`/`korbit-cli.db` and `bot.db`/`korbit-bot.db` with their `-wal`/`-shm`; `debug-*.json`; `keys.json` and `keystore.json`, each key first cleared from its backend, OS keychain included | — |
| regenerable caches | **no** | `sandbox/` in each home; the shared Deno/module cache under **both** cache names (a running sandbox is stopped first) | — |

Then, for each shell startup file carrying the installer's `PATH` block, it shows
a diff and asks (default **no**) whether to remove **just that block** — never the
file. On Windows it removes only the digitalx-cli entry from your User `PATH`. A
file you decline is reported so you can edit it yourself.

Worth knowing:

- **Every home is offered** — `~/.digitalx-cli` and `~/.korbit-cli` whenever they
  exist, plus a pinned one — and every database under **both** spellings, so a
  home renamed without its files is cleaned out completely.
- **The home directory itself goes only once it is empty.** Kept data, kept
  caches, or a failed removal leave it in place — deliberately.
- **Removing keys cannot be undone.** Each key is cleared from its backend (OS
  keychain included) before its file is deleted, so nothing is orphaned — but it
  is gone.
- **On Windows the running `.exe` is locked** and cannot delete itself; it is
  reported for you to delete by hand.

### Option B — move it by hand

Keeps everything. Four steps, in order.

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

**3. Rename the three databases and their sidecars.** The new directory name
means the CLI now looks for the current file names; a file you skip here is a
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

**4. Delete the earlier artifact cache.** It holds only regenerable downloads,
and the new location refills itself on the next sandbox run:

| OS | Earlier cache |
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

**A pinned home overrides all of this.** With `DIGITALX_CLI_HOME` or
`KORBIT_CLI_HOME` set, that path is the home whatever is in `~`, and moving
`~/.korbit-cli` changes nothing for you until you change the variable. A pinned
directory also follows the file-name rule by its **own basename**: pinned at a
directory named `.korbit-cli` it keeps the earlier file names — the right layout
if an earlier `korbit` binary is still sharing it.

Finally, verify:

```sh
dgx-cli self doctor
```

## What `self doctor` reports

A **problem** is something broken (exit non-zero); a **note** is something that
works but a command would tidy up (exit 0). Every problem carries a stable
`field` tag:

| `field` | Problem | Fix |
|---|---|---|
| `managed` | install manifest missing or corrupt | the install one-liner |
| `binary` | installed binary missing, or not matching the manifest — including a `korbit` that matches while the `dgx-cli` beside it does not (the documented name would run bytes this install never placed) | the install one-liner; `self update` when only `dgx-cli` mismatches |
| `alias` | `korbit` missing, pointing at something other than `dgx-cli`, or a stale copy of another version | `self update` |
| `path` | install directory not on `PATH` | the install one-liner |
| `home` | one of the two states below | you, by hand |

Only **two** states about the two directory names are ever reported, and both
mean you have data the CLI is not reading:

- **Two CLI homes exist, and the one not in use holds data.** The report names
  the one in use, so you know which half is invisible; when the home in use holds
  no keys, config, or journal at all while the other does, it says so outright —
  a certainty, not an ambiguity. **Remedy:** move what you need across, or remove
  the directory you do not want, following [option B](#option-b--move-it-by-hand).
- **The home in use holds databases under the earlier file names**, and its
  directory is not named `.korbit-cli` — a directory renamed without its files.
  The report names each file and the rename it needs. **Remedy:** step 3 of
  [option B](#option-b--move-it-by-hand); or, for a pinned home, point the
  variable at a directory named `.korbit-cli` instead, which keeps the earlier
  layout with nothing to rename.

Everything else about the earlier names is **silent** — correct, not a finding:
`~/.korbit-cli` holding its earlier-named databases; an artifact cache under the
earlier name; a `.korbit-cli` home that happens to hold a stray `journal.db`,
since the earlier names are the right ones there.

## What never changes

- **The `korbit` command name.** An install that has it keeps it, permanently,
  refreshed on every install and update so it never falls behind.
- **A `korbit` file this install did not place.** Ownership must be proved — the
  manifest records it, it is the manifest's own executable, it is the running
  binary, or its bytes hash to the manifest's SHA-256 — before anything overwrites
  or deletes it. Your own wrapper script at that name is reported and left exactly
  as it is.
- **An install left on the earlier names.** `~/.korbit-cli`, its earlier database
  names, and the artifact cache under the earlier name are read for good — a fully
  supported state with no deadline. Using the current names is a choice you make
  and carry out, never something that happens to you.
- **Key compatibility.** Nothing about how keys are stored, encrypted, or read
  changes: a keystore written by either generation is read by both, and an OS
  keychain item is never re-created or re-encrypted.
- **Environment variables.** Both spellings are honored indefinitely;
  `DIGITALX_CLI_*` wins when both are set — and only when its value is non-empty.
  An empty `DIGITALX_CLI_*` reads as unset and falls through to `KORBIT_CLI_*`, so
  to turn a setting off, unset both.
- **A pinned home, directory and contents.** A home fixed with `DIGITALX_CLI_HOME`
  or `KORBIT_CLI_HOME` keeps whatever [file-name layout](#files-inside-the-cli-home)
  its own basename implies, permanently.
- **The earlier release archive and bundle URLs.** Already-installed versions
  fetch them by name; every earlier tag's `korbit_*` archives stay downloadable at
  the URLs they always had.

## Reading the result as JSON

Every outcome above is a field, not prose, so `--json` is enough to act on:

- `self update` → `layoutRepaired` (the command-name fixes this run made;
  `checkedOnly` says whether they were applied or only planned) and `next`
  (follow-up steps). There is no migration field, because there is no migration.
- `self install` → `repaired`, `path`, and `next`.
- `self doctor` → `problems[].field` for anything broken, `notes` for the
  healthy-but-tidiable states, plus `home` and `legacyHome` so the directories
  need no parsing out of prose.

## For maintainers

An ordinary release publishes **one** archive set, listed in a signed
`checksums.txt` alongside a `release-manifest.json` that names the archive each
platform downloads and the binary inside it. An install reads those names out of
the release rather than assuming the ones compiled into it, which is what lets a
rename cost one archive set instead of a second one published under the earlier
name for as long as installs made under it exist:
[`RELEASING.md`](https://github.com/digitalx-official/digitalx-cli/blob/master/RELEASING.md).
