# zot installer for Windows (PowerShell).
#
# Usage (in PowerShell):
#   iwr -useb https://raw.githubusercontent.com/patriceckhart/zot/main/install.ps1 | iex
#
# Or with arguments:
#   $env:ZOT_VERSION = "v0.0.1"
#   $env:ZOT_PREFIX  = "$HOME\bin"
#   iwr -useb https://raw.githubusercontent.com/patriceckhart/zot/main/install.ps1 | iex
#
# Detects architecture, downloads the matching .zip from the GitHub
# release, verifies the sha256 against checksums.txt, extracts zot.exe,
# and moves it into $ZOT_PREFIX (defaults to $HOME\bin, added to PATH
# via the User environment if missing).
#
# While the repo is private, set $env:GITHUB_TOKEN to a PAT with
# `contents:read` scope; the script uses it for every download. Once
# the repo goes public, the token becomes optional.


[CmdletBinding()]
param(
  [string]$Version = $env:ZOT_VERSION,
  [string]$Prefix  = $env:ZOT_PREFIX
)

$ErrorActionPreference = "Stop"

$owner  = "patriceckhart"
$repo   = "zot"
$binary = "zot"

# Build Authorization header list once; used on every HTTP call so the
# script works against private repos when $env:GITHUB_TOKEN is set.
$headers = @{}
if ($env:GITHUB_TOKEN) { $headers["Authorization"] = "Bearer $($env:GITHUB_TOKEN)" }

if (-not $Version) { $Version = "latest" }
if (-not $Prefix)  { $Prefix  = Join-Path $HOME "bin" }

function Msg($m)  { Write-Host "==> $m" -ForegroundColor Cyan }
function Warn($m) { Write-Warning $m }
function Die($m)  { Write-Error $m; exit 1 }

# ---- detect architecture ----

switch -wildcard ($env:PROCESSOR_ARCHITECTURE) {
  "AMD64" { $arch = "amd64" }
  "ARM64" { $arch = "arm64" }
  default { Die "unsupported arch: $($env:PROCESSOR_ARCHITECTURE)" }
}

# ARM64 Windows isn't shipped (see .goreleaser.yaml ignore rule) — fall
# back to amd64 which runs fine under ARM64 emulation.
if ($arch -eq "arm64") {
  Warn "windows/arm64 is not published; falling back to amd64"
  $arch = "amd64"
}

# ---- resolve version ----

if ($Version -eq "latest") {
  if ($headers.ContainsKey("Authorization")) {
    $api = Invoke-RestMethod -Headers $headers "https://api.github.com/repos/$owner/$repo/releases/latest"
    $Version = $api.tag_name
  } else {
    $resp = Invoke-WebRequest -UseBasicParsing -MaximumRedirection 5 `
      "https://github.com/$owner/$repo/releases/latest"
    if ($resp.BaseResponse.ResponseUri -match '/tag/([^/]+)') {
      $Version = $Matches[1]
    } else {
      Die "could not resolve latest version (set `$env:GITHUB_TOKEN if the repo is private)"
    }
  }
}

if (-not $Version.StartsWith("v")) { $Version = "v$Version" }
$verNum = $Version.TrimStart("v")

# ---- download + verify + extract ----

$archive     = "${binary}_${verNum}_windows_${arch}.zip"
$baseUrl     = "https://github.com/$owner/$repo/releases/download/$Version"
$archiveUrl  = "$baseUrl/$archive"
$checksumUrl = "$baseUrl/checksums.txt"

$tmp = New-Item -ItemType Directory -Path (Join-Path $env:TEMP ("zot-install-" + [System.Guid]::NewGuid().ToString("N").Substring(0,8)))

try {
  Msg "downloading $archive"
  Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri $archiveUrl -OutFile (Join-Path $tmp $archive)

  Msg "verifying checksum"
  $checksums = Invoke-WebRequest -UseBasicParsing -Headers $headers -Uri $checksumUrl | Select-Object -ExpandProperty Content
  $expected  = ($checksums -split "`n" | Where-Object { $_ -match [regex]::Escape($archive) + "$" } | Select-Object -First 1)
  if (-not $expected) { Die "no checksum for $archive in checksums.txt" }
  $expectedHash = ($expected -split "\s+")[0]

  $actualHash = (Get-FileHash -Path (Join-Path $tmp $archive) -Algorithm SHA256).Hash.ToLower()
  if ($expectedHash.ToLower() -ne $actualHash) {
    Die "checksum mismatch: expected $expectedHash, got $actualHash"
  }

  Msg "extracting"
  Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

  $exe = Join-Path $tmp "$binary.exe"
  if (-not (Test-Path $exe)) { Die "archive did not contain $binary.exe" }

  Msg "installing to $Prefix\$binary.exe"
  New-Item -ItemType Directory -Path $Prefix -Force | Out-Null
  Copy-Item $exe (Join-Path $Prefix "$binary.exe") -Force

  # ---- PATH hint ----

  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  $parts = $userPath -split ";" | Where-Object { $_ }
  if (-not ($parts -contains $Prefix)) {
    Warn "$Prefix is not on your user PATH"
    Warn "adding it for future sessions..."
    [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(";") + ";" + $Prefix), "User")
    Warn "open a new terminal to pick up the change, or run:"
    Warn "  `$env:Path = `"$Prefix;`$env:Path`""
  }

  Msg "installed. run:  zot --help"
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
