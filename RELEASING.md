<!--
Copyright (c) 2026 Digital X Co., Ltd.
SPDX-License-Identifier: Apache-2.0
-->

# Releasing digitalx-cli

Releases are built with [GoReleaser](https://goreleaser.com). The configuration
is [`.goreleaser.yaml`](.goreleaser.yaml); it cross-compiles every target,
injects the version from the git tag, code-signs the macOS binaries, and packs a
per-platform [`.mcpb` Desktop Extension](#desktop-extensions-mcpb). macOS
**notarization** is a deliberate second step (`make notarize`) run only for a
real, published release.

## Two archive sets per release

Every release publishes the **same program twice**, under two names:

| Asset | Binary inside | Who downloads it |
| --- | --- | --- |
| `dgx-cli_<os>_<arch>.{tar.gz,zip}` | `dgx-cli` | the installers, and `dgx-cli self update` |
| `korbit_<os>_<arch>.{tar.gz,zip}` | `korbit` | an installed `korbit` binary updating itself |

The two are built from the same source with the same flags, ldflags, and target
matrix — they differ only in the compiled binary's filename (`.goreleaser.yaml`
carries a second `build` and a second `archives` entry for the legacy set).

The legacy set exists because an already-installed `korbit` asks for a fixed
asset name and then extracts the archive entry whose basename is exactly
`korbit`. Both halves of that — the asset name and the name inside the archive —
are a contract with binaries that are already on users' machines, so **neither
may be renamed**: dropping the set, or renaming the binary inside it, strands
every existing install with no way to update.

That is also all the legacy set is for. Once a `korbit` install has updated
through it, the new binary installs `dgx-cli` as the primary and keeps `korbit`
as an alias beside it, and from then on it updates through the `dgx-cli` asset
like any other install.

One `checksums.txt` covers both sets plus the `.mcpb` bundles, so the single
signature over it authenticates every download. The `.mcpb` Desktop Extensions
are built from the `dgx-cli` binaries only — an extension is installed fresh
rather than self-updated, so it needs no legacy name.

## Target matrix

| OS      | Arch          |
| ------- | ------------- |
| darwin  | arm64         |
| linux   | amd64, arm64  |
| windows | amd64, arm64  |

Intel macOS (`darwin/amd64`) is intentionally not built.

## Versioning

The version reported by `dgx-cli --version` (and embedded in the User-Agent
header and the `commands` catalog) is injected at build time from the git tag:

```
-ldflags "-X github.com/digitalx-official/digitalx-cli/internal/version.Version=<tag>"
```

GoReleaser does this automatically (`{{ .Version }}` is the tag with any leading
`v` stripped). A plain `go build` / `make build` leaves the default `dev`.

So a release is just a tag:

```sh
git tag v1.2.3
git push origin v1.2.3
```

## Prerequisites

- **[GoReleaser](https://goreleaser.com/install/)** and
  **[rcodesign](https://gregoryszorc.com/docs/apple-codesign/main/)**
  (`apple-codesign`) on `PATH`. rcodesign signs and notarizes Mach-O binaries on
  **any** OS — so the release can run on macOS or Linux.
- An Apple **Developer ID Application** certificate (for signing).
- An **RSA signing key** (PEM) and **`openssl`** on `PATH` (for the
  broadly-verifiable checksums signature — see below).
- **`zip`** on `PATH` (to pack the `.mcpb` Desktop Extensions; standard on macOS
  and Linux).
- The **`gh`** CLI on `PATH`, authenticated (for publishing).
- For notarization only: an **App Store Connect API key** (see below).

## Signing (by default)

Every macOS binary is signed during the build by `scripts/macos-sign.sh`
(a GoReleaser post-build hook) with `rcodesign sign --for-notarization`, which
enables the hardened runtime and a secure timestamp. It pins a fixed binary
identifier (`kr.co.korbit.korbit-cli`) so the code-signing designated
requirement stays stable across versions — the macOS keychain backend binds
each stored secret's ACL to that requirement, so it must not drift. The signing
identity comes from the environment — use **either** a keychain identity (macOS)
**or** a PKCS#12 file (any OS):

```sh
# macOS keychain — the cert's SHA-256 fingerprint (hex, no spaces):
#   rcodesign keychain-print-certificates | grep -A1 "Developer ID Application"
export MACOS_SIGN_KEYCHAIN_FINGERPRINT=ABCD...

# …or a PKCS#12 / .p12 file (exported from Keychain Access, or on Linux):
export MACOS_SIGN_P12=/path/to/developer-id.p12
export MACOS_SIGN_P12_PASSWORD_FILE=/path/to/p12-password.txt   # preferred
# export MACOS_SIGN_P12_PASSWORD='...'                          # inline (less safe)
```

If no identity is configured, a **snapshot** build (`make dist`) skips signing
with a warning so the matrix still builds anywhere; a **real release** fails
rather than ship an unsigned macOS binary.

To test the build on a machine without a certificate (or to skip signing even
when one is present), disable signing explicitly with `KORBIT_SKIP_MACOS_SIGN`:

```sh
make dist-unsigned       # snapshot build, signing forced off
make release-unsigned    # full release path (real version, archives), not published
```

`KORBIT_SKIP_MACOS_SIGN` skips signing in any mode and is for testing only — the
resulting macOS binaries are **unsigned and must never be shipped**. Like
`make release`, `release-unsigned` needs a tagged commit; for an untagged tree
set `GORELEASER_CURRENT_TAG`.

## Checksum signing

The `checksums.txt` file authenticates every artifact transitively: a verifier
checks the signature on `checksums.txt`, then checks each download against the
hashes in it. A single detached signature is produced over it (a GoReleaser
signs hook), keyed from the environment:

- **`checksums.txt.sig`** — raw RSA/SHA-256 (PKCS#1 v1.5) signature
  (`scripts/openssl-sign.sh`); verify with openssl. Only the **private** key is
  referenced here; its public half is held by the verifier and can be rotated
  there without touching this repo. The verifiers are the evergreen installers
  (which embed the cert) and `dgx-cli self update`, which fetches the cert from
  `https://docs.digitalx.miraeasset.com/release-signing-cert.pem` — a managed host separate
  from the GitHub release, so the pin defends against a compromised release.
  Publishing an empty cert there disables the self-updater's signature check.

### RSA signature

The RSA signing key is a PEM private-key path in the environment:

```sh
export KORBIT_RSA_SIGN_KEY=/path/to/release-signing-key.pem
```

Generate a keypair once (2048-bit; the self-signed cert wraps the public key so
the verifier can pin it as a single blob), keep the private key offline, and
distribute the cert to the verifier:

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout release-signing-key.pem -out release-signing-cert.pem \
  -subj "/CN=digitalx-cli release signing"
```

Verify a download with openssl:

```sh
openssl x509 -in release-signing-cert.pem -pubkey -noout > pub.pem
openssl dgst -sha256 -verify pub.pem -signature checksums.txt.sig checksums.txt
```

As with macOS signing, a snapshot with no `KORBIT_RSA_SIGN_KEY` skips with a
warning, a real release fails rather than ship unsigned checksums, and
`KORBIT_SKIP_RSA_SIGN=1` forces a skip for testing (the `*-unsigned` targets set
it). GoReleaser suppresses a signing command's stderr on success, so the
skip/sign messages only show under `goreleaser ... --verbose`.

## Desktop Extensions (.mcpb)

Each build also produces a per-platform `.mcpb` — an [MCP
Bundle](https://github.com/anthropics/mcpb) ("Desktop Extension"): a single file
a user drags into Claude Desktop to install `dgx-cli mcp serve` with no terminal
step. It is the zero-terminal path that lets the Claude Desktop chat reach the
local, key-holding binary; users comfortable with a shell can instead wire up
the server with `claude mcp add …` (see the README).

An `.mcpb` is just a ZIP with a `manifest.json` at the root plus the binary under
`server/`. `scripts/build-mcpb.sh` is a per-target post-build hook (it runs right
after `macos-sign.sh`) that writes `dist/dgx-cli_<os>_<arch>.mcpb`. We ship **one
bundle per platform/arch** — each carries a single binary — rather than one fat
multi-platform bundle. No Node tooling is required to build them.

Two facts make this ordering safe and simple:

- **The packed binary is the signed one.** The hook runs after signing, so a
  darwin bundle contains the code-signed binary. Notarization (below) is a later
  step that does **not** modify the binary bytes, so the packed binary stays the
  notarized one — Gatekeeper verifies it online on first run.
- **The bundle itself is unsigned**, by design. The `.mcpb` format supports its
  own PKCS#7 signature, but we rely on the inner binary's macOS signing +
  notarization for trust. (Adding `mcpb sign` later would be a drop-in extra
  step.)

The `.mcpb` files are folded into `checksums.txt` (via `checksum.extra_files`),
so the one signature on the checksums authenticates them too, and
`make publish` uploads them alongside the archives.

## Notarization (separate, real releases only)

Notarization is not part of the build — it talks to Apple and is only meaningful
for binaries end users will download. Run it **after** the release build, over
the signed binaries in `dist/`:

```sh
make notarize           # ./scripts/macos-notarize.sh dist
```

It zips each signed macOS binary, submits it to Apple's notary service, and
waits for the verdict — **both** darwin builds, `dgx-cli` and `korbit`, since
both ship to users (see [Two archive sets](#two-archive-sets-per-release)). It
does **not** staple: a stand-alone CLI binary can't
carry a stapled ticket, so Gatekeeper verifies the notarization online the first
time a downloaded copy runs.

### App Store Connect API key

Create one in App Store Connect → **Users and Access → Integrations → App Store
Connect API** → generate a key. You get an **Issuer ID**, a **Key ID**, and a
one-time **`AuthKey_<KeyID>.p8`** download. Encode the trio once for rcodesign:

```sh
rcodesign encode-app-store-connect-api-key \
  <issuer-id> <key-id> AuthKey_<key-id>.p8 > asc-key.json
export ASC_API_KEY_FILE=$(pwd)/asc-key.json
```

Keep `asc-key.json` and the `.p8` out of the repo.

## Publishing (separate, via `gh`)

### The release repository must already carry the current name

`DefaultRepo` (`internal/selfupdate/selfupdate.go`) and `REPO` / `$Repo` in both
installers resolve to **`digitalx-official/digitalx-cli`**. A release built from
this source is therefore publishable only once the GitHub organisation and
repository carry that name: publish it earlier and `self update`, `install.sh`,
and `install.ps1` all resolve a repository that does not exist. Publish the
release **after** the org/repo rename, and keep the previous org name registered
so the redirect below cannot be taken over.

Binaries already on users' machines are unaffected either way: they request
`releases/latest` under the org/repo name compiled into them, and GitHub
redirects that to the renamed repository. They then download the
`korbit_<os>_<arch>` asset set, which every release still publishes (see
[Two archive sets](#two-archive-sets-per-release)).

Building and publishing are separate steps. `make release` **never contacts
GitHub** — it only builds, signs, and (with `make notarize`) reaches Apple. The
artifacts are uploaded later with `make publish`, which uses the `gh` CLI, so
the same built+signed+notarized `dist/` can go to a staging repo for review and
then to the public repo without rebuilding.

```sh
make publish                              # upload dist/ to the current repo's release
KORBIT_RELEASE_REPO=<owner>/<name> make publish   # …or to a specific repo
```

`make publish` requires HEAD to be on the release tag, and `gh` authenticated
(`gh auth login`, or `GH_TOKEN`/`GITHUB_TOKEN` in CI). It uploads the archives,
the `.mcpb` Desktop Extensions, the filled `install.sh`/`install.ps1`,
`checksums.txt`, and its signature (`checksums.txt.sig`);
re-running re-uploads with `--clobber`.
These filled installers are the release-pinned copies attached to the GitHub
release; they embed this release's archive checksums, so they must ship in the
same release as those archives — `make release` fills them from this tag's
`checksums.txt` (see the `release` target's comment). Note that the `curl … | sh`
/ `irm … | iex` one-liners the README advertises fetch the evergreen installer
hosted at `docs.digitalx.miraeasset.com`, not these release-attached copies.
Push the release commit/tag to the target repo first (so `gh` attaches the
release to the right commit).

## Workflow

```sh
# 1. Local smoke test — full matrix, no signing, no notarization.
make dist-unsigned              # or `make dist` to also exercise signing

# 2. Real build — tag, then build + sign + notarize. No GitHub access.
git tag v1.2.3
make release                    # build + sign into dist/
make notarize                   # notarize the signed macOS binaries
#    …verify dist/ locally…

# 3. Publish — staging repo first for review, then the public repo (same artifacts).
KORBIT_RELEASE_REPO=<staging-owner>/<repo>      make publish
KORBIT_RELEASE_REPO=digitalx-official/digitalx-cli  make publish
```

Windows binaries are shipped unsigned (Authenticode signing is not configured).
