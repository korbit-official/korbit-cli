# korbit-cli installer (Windows / PowerShell).
#
# This is a TEMPLATE for the release-pinned installer. The runnable copy is
# filled with this release's version + archive checksums and attached to the
# GitHub release, fetchable directly at:
#
#   irm https://github.com/korbit-official/korbit-cli/releases/latest/download/install.ps1 | iex
#
# (The README's advertised one-liner instead fetches the evergreen installer
# hosted at https://docs.korbit.co.kr/install.ps1.)
#
# What it does: detect your architecture, download that release's .zip, verify
# its SHA-256 against the value embedded below, extract it, and hand off to
# `korbit self install`, which places the binary, wires the User PATH, and writes
# the install manifest. Trust is TLS + SHA-256.
#
# The whole body runs inside `& { … } @args` so that a failure (a `throw`) or
# the normal end of the script returns to the caller's prompt instead of
# terminating it. Under `irm … | iex` the script's text executes in the CURRENT
# session, so a top-level `exit` would kill the hosting PowerShell and close the
# terminal window. `@args` is forwarded into the block so any passthrough
# arguments still reach `korbit self install`.
& {
  $ErrorActionPreference = 'Stop'

# >>> release-pin (filled at release time) >>>
# NOTE: these three lines stay flush-left (column 0), even though they sit inside
# the `& { }` block above. Two things require it: PowerShell demands the here-string
# terminator `'@` at column 0, and scripts/fill-install-templates.sh matches these
# lines with column-0-anchored awk patterns. Indenting them silently breaks the
# release fill — do not reflow.
$PIN_VERSION = ''
# One "<sha256>  <archive-name>" per line (sha256sum format), for this release's
# archives. Empty in the in-repo template.
$PIN_SHA256 = @'
'@
# <<< release-pin <<<

  $Repo = 'korbit-official/korbit-cli'

  # Report a fatal condition. `throw` (not `exit`) so that, under `irm | iex`, we
  # unwind back to the caller's prompt rather than closing the terminal.
  function Die($msg) { throw "install: $msg" }

  if (-not $PIN_VERSION) {
    Die "this is the in-repo template, not a released installer — install with:`n  irm https://github.com/$Repo/releases/latest/download/install.ps1 | iex"
  }

  # --- architecture detection ---
  switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Die "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
  }
  $asset = "korbit_windows_$arch.zip"

  # Expected hash for this platform's archive, from the embedded pin block.
  $expected = $null
  foreach ($line in ($PIN_SHA256 -split "`n")) {
    $f = ($line.Trim() -split '\s+')
    if ($f.Count -ge 2 -and $f[1] -eq $asset) { $expected = $f[0].ToLower(); break }
  }
  if (-not $expected) { Die "no embedded checksum for $asset in this installer" }

  # --- download ---
  $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("korbit-" + [System.Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp -Force | Out-Null
  try {
    $url = "https://github.com/$Repo/releases/download/$PIN_VERSION/$asset"
    $zip = Join-Path $tmp $asset
    Write-Host "install: downloading korbit $PIN_VERSION (windows/$arch)..."
    Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing

    # --- verify SHA-256 (always enforced; no skip) ---
    $actual = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLower()
    if ($actual -ne $expected) {
      Die "checksum mismatch for ${asset}:`n  expected $expected`n  actual   $actual"
    }

    # --- extract and hand off to the binary ---
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $exe = Join-Path $tmp 'korbit.exe'
    if (-not (Test-Path $exe)) { Die "archive did not contain korbit.exe" }

    # The binary owns install policy (PATH location, PATH entry, manifest) and is
    # reconciling, so this both installs fresh and repairs a broken install.
    & $exe self install @args
    if ($LASTEXITCODE -ne 0) { Die "korbit self install exited with code $LASTEXITCODE" }
  }
  finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
} @args
