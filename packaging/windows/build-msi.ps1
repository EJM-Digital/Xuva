#requires -version 5
<#
.SYNOPSIS
  Builds the Xuva single-installer MSI (WiX v5).

.DESCRIPTION
  First slice: builds xuva-server.exe (which embeds the web UI) and packages it
  into an MSI that installs the engine + the XuvaMediaServer Windows Service.
  ffmpeg/ffprobe, the tray component, firewall rules and the updater are layered
  in by later slices.

  Requires the WiX v5 .NET tool on PATH (`dotnet tool install --global wix`)
  AND Node/npm (used to rebuild the Svelte SPA into static-next before the
  server's //go:embed picks it up, and to build the Electron tray bundle).
  Running publish:go-static every build guarantees the MSI ships a current
  SPA — closes #90, where the previous "use committed static-next" path
  shipped stale UI when a frontend PR merged without re-running publish.

.PARAMETER Version
  Release version, e.g. "v1.2.3" or "1.2.3.0". This is injected into the server
  binary (buildinfo.Version, so the running service reports its real version and
  the updater can compare against GitHub releases) AND normalized to a four-part
  numeric MSI ProductVersion (via -d Version=) so MajorUpgrade can tell a newer
  MSI from the installed one. The output filename also carries it.

.PARAMETER OutputDir
  Where the .msi is written. Relative paths are resolved from the repo root.
#>
[CmdletBinding()]
param(
    [string]$Version = "0.0.0.0",
    [string]$OutputDir = "dist/windows-msi"
)

$ErrorActionPreference = "Stop"

# Returns the trimmed stdout of a git command, or "" if git isn't available or
# the command fails. Mirrors build-package.ps1's Get-GitValue.
function Get-GitValue {
    param([Parameter(Mandatory = $true)][string[]]$GitArgs)
    try {
        $value = (& git @GitArgs 2>$null)
        if ($LASTEXITCODE -eq 0) { return ([string]$value).Trim() }
    } catch {}
    return ""
}

# Normalizes a release version into a four-part numeric MSI ProductVersion.
# MSI versions are major.minor.build[.revision] with each field <= 65535, and
# any pre-release suffix (e.g. "-beta.1") is invalid — so strip a leading "v",
# drop everything from the first "-"/"+", and pad/truncate to four parts.
#   v1.2.3        -> 1.2.3.0
#   1.2.3.4       -> 1.2.3.4
#   v1.2.3-beta.1 -> 1.2.3.0
#   dev / garbage -> 0.0.0.0
function Get-MsiProductVersion {
    param([Parameter(Mandatory = $true)][string]$Version)
    $v = $Version.Trim()
    if ($v.StartsWith("v") -or $v.StartsWith("V")) { $v = $v.Substring(1) }
    $v = ($v -split "[-+]", 2)[0]
    $parts = $v -split "\."
    $nums = @()
    foreach ($p in $parts) {
        if ($p -match "^\d+$" -and [long]$p -le 65535) { $nums += [int]$p } else { break }
    }
    while ($nums.Count -lt 4) { $nums += 0 }
    return ($nums[0..3] -join ".")
}

# Normalizes a release version into the semver string electron-builder accepts
# in package.json. Mirrors build-package.ps1's Get-DesktopVersion (so the MSI
# and the legacy NSIS .exe label the Electron app identically). 0.0.0.0 →
# "0.0.0-dev" so PR-CI dry-runs produce a recognisable pre-release tag.
#   v1.2.3 → 1.2.3
#   1.2.3.4 → not strict semver, falls back to "0.0.0-dev"
#   dev / garbage → 0.0.0-dev
function Get-DesktopVersion {
    param([Parameter(Mandatory = $true)][string]$Version)
    $normalized = $Version.Trim()
    if ($normalized.StartsWith("v")) { $normalized = $normalized.Substring(1) }
    if ($normalized -match "^\d+\.\d+\.\d+([-.+][0-9A-Za-z.-]+)?$") {
        return $normalized
    }
    return "0.0.0-dev"
}

# Throws if a command isn't on PATH. Used to fail fast (with a useful message)
# when the runner is missing node/npm before we get to a confusing electron-
# builder error 200 lines later.
function Get-RequiredCommand {
    param([Parameter(Mandatory = $true)][string]$Name)
    $cmd = Get-Command $Name -ErrorAction SilentlyContinue
    if (-not $cmd) {
        throw "Required build command '$Name' was not found on PATH."
    }
    return $cmd.Source
}

# Downloads the BtbN LGPL ffmpeg build, verifies its SHA-256 against the upstream
# checksums manifest, and stages ffmpeg.exe + ffprobe.exe into $DestinationDir.
# (Self-contained mirror of build-package.ps1's Save-VerifiedFFmpeg; the two are
# deduped in a later cleanup.)
function Save-VerifiedFFmpeg {
    param([Parameter(Mandatory = $true)][string]$DestinationDir)

    $archiveName = "ffmpeg-master-latest-win64-lgpl.zip"
    $archiveUrl = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/$archiveName"
    $checksumsUrl = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/checksums.sha256"
    $tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("xuva-msi-ffmpeg-" + [System.Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $tempRoot -Force | Out-Null
    try {
        $archivePath = Join-Path $tempRoot $archiveName
        $checksumsPath = Join-Path $tempRoot "checksums.sha256"
        Write-Host "Downloading FFmpeg checksum manifest..."
        Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath -UseBasicParsing
        $checksumsText = Get-Content -LiteralPath $checksumsPath -Raw
        $checksumPattern = "(?im)^([a-f0-9]{64})\s+\*?$([regex]::Escape($archiveName))\s*$"
        $checksumMatch = [regex]::Match($checksumsText, $checksumPattern)
        if (-not $checksumMatch.Success) {
            throw "FFmpeg checksum entry for '$archiveName' was not found."
        }
        $expected = $checksumMatch.Groups[1].Value.ToLowerInvariant()

        Write-Host "Downloading FFmpeg..."
        Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath -UseBasicParsing
        $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "FFmpeg checksum mismatch. Expected $expected, got $actual."
        }

        $extractDir = Join-Path $tempRoot "extract"
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force
        $binDir = Get-ChildItem -LiteralPath $extractDir -Recurse -Directory |
            Where-Object {
                (Test-Path -LiteralPath (Join-Path $_.FullName "ffmpeg.exe")) -and
                (Test-Path -LiteralPath (Join-Path $_.FullName "ffprobe.exe"))
            } |
            Select-Object -First 1
        if (-not $binDir) {
            throw "Downloaded FFmpeg archive did not contain ffmpeg.exe and ffprobe.exe."
        }

        New-Item -ItemType Directory -Path $DestinationDir -Force | Out-Null
        Copy-Item -LiteralPath (Join-Path $binDir.FullName "ffmpeg.exe") -Destination (Join-Path $DestinationDir "ffmpeg.exe") -Force
        Copy-Item -LiteralPath (Join-Path $binDir.FullName "ffprobe.exe") -Destination (Join-Path $DestinationDir "ffprobe.exe") -Force
    } finally {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$serverDir = Join-Path $repoRoot "server"
$wxsSource = Join-Path $PSScriptRoot "wix\Xuva.wxs"
$staging = Join-Path $repoRoot "dist\msi-staging"
$outputBase = if ([System.IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $repoRoot $OutputDir }

Write-Host "Repo root : $repoRoot"
Write-Host "Staging   : $staging"
Write-Host "Output    : $outputBase"

# Fresh staging + output dirs.
Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $staging | Out-Null
New-Item -ItemType Directory -Force -Path $outputBase | Out-Null

# Build metadata baked into the binary so the running service reports its real
# version (buildinfo.Version) — without this the MSI server reports "dev", the
# update check can't compare it against a GitHub tag, and self-update is dead.
$gitCommit = Get-GitValue -GitArgs @("rev-parse", "HEAD")
if ([string]::IsNullOrWhiteSpace($gitCommit)) { $gitCommit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$msiVersion = Get-MsiProductVersion -Version $Version
Write-Host "Version   : $Version (MSI ProductVersion $msiVersion, commit $gitCommit)"

# 1a) Rebuild the Svelte SPA into server/internal/webapp/static-next/ so the
#     server's //go:embed bundles the current frontend. Without this the MSI
#     would ship whatever static-next happens to be committed — and a frontend
#     PR can land on main without re-running publish:go-static, which is
#     exactly the bug #90 closes. We accept ~10-15s of vite build per MSI
#     build as the cost of correctness; the npm ci + node_modules cache hit
#     keeps PR-CI iteration affordable.
$npm = Get-RequiredCommand -Name "npm"
$svelteRoot = Join-Path $repoRoot "apps\web\svelte"
Write-Host "`n== publish:go-static (Svelte SPA -> static-next) =="
Push-Location $svelteRoot
try {
    & $npm ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci (apps/web/svelte) failed (exit $LASTEXITCODE)" }
    & $npm run "publish:go-static"
    if ($LASTEXITCODE -ne 0) { throw "publish:go-static failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}
$staticNext = Join-Path $repoRoot "server\internal\webapp\static-next"
if (-not (Test-Path -LiteralPath $staticNext)) {
    throw "publish:go-static did not populate server\internal\webapp\static-next"
}

# 1b) Build the server binary (embeds the SPA we just rebuilt). Run from the
#     module root.
Write-Host "`n== go build xuva-server.exe =="
$serverExe = Join-Path $staging "xuva-server.exe"
Push-Location $serverDir
try {
    $ldflagsParts = @(
        "-s",
        "-w",
        "-X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Version=$Version",
        "-X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Commit=$gitCommit",
        "-X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Date=$buildDate"
    )
    # Embed the default metadata API keys when CI provides them (same as the
    # desktop/NSIS build) so an MSI-installed server has working defaults.
    if (-not [string]::IsNullOrWhiteSpace($env:XUVA_DEFAULT_TMDB_API_KEY)) {
        $ldflagsParts += "-X github.com/EJM-Digital/Xuva/server/internal/config.DefaultTMDBAPIKey=$($env:XUVA_DEFAULT_TMDB_API_KEY)"
    }
    if (-not [string]::IsNullOrWhiteSpace($env:XUVA_DEFAULT_FANARTTV_API_KEY)) {
        $ldflagsParts += "-X github.com/EJM-Digital/Xuva/server/internal/config.DefaultFanartTVAPIKey=$($env:XUVA_DEFAULT_FANARTTV_API_KEY)"
    }
    if (-not [string]::IsNullOrWhiteSpace($env:XUVA_DEFAULT_OMDB_API_KEY)) {
        $ldflagsParts += "-X github.com/EJM-Digital/Xuva/server/internal/config.DefaultOMDbAPIKey=$($env:XUVA_DEFAULT_OMDB_API_KEY)"
    }
    $ldflags = $ldflagsParts -join " "
    & go build -trimpath "-ldflags=$ldflags" -o $serverExe ".\cmd\Xuva"
    if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}
if (-not (Test-Path -LiteralPath $serverExe)) { throw "xuva-server.exe was not produced" }

# 1b) Stage ffmpeg/ffprobe into bin\ (the wxs packages them; the server
#     auto-detects <exeDir>\bin\ffmpeg.exe at runtime).
Write-Host "`n== ffmpeg/ffprobe =="
$binStaging = Join-Path $staging "bin"
Save-VerifiedFFmpeg -DestinationDir $binStaging
foreach ($tool in @("ffmpeg.exe", "ffprobe.exe")) {
    if (-not (Test-Path -LiteralPath (Join-Path $binStaging $tool))) { throw "$tool was not staged" }
}

# 1c) Build the Electron tray bundle for the optional TrayFeature.
#     electron-builder picks up xuva-server.exe + bin\ffmpeg via the
#     `extraResources: runtime/ -> resources/runtime` mapping in
#     apps/desktop/package.json, so we stage runtime\ inside apps\desktop
#     first. The result is dist\win-unpacked\ (no NSIS, no zip — just the
#     unpacked Electron app dir). That dir is then copied into the MSI
#     staging area so heat can harvest it into a WiX ComponentGroup.
#
#     NOTE: electron-builder bundles xuva-server.exe + bin\ffmpeg/ffprobe at
#     resources\runtime\* via the package.json extraResources entry. We let
#     it do so (so the unpacked dir is "complete" and would still work as a
#     legacy NSIS layout) and then STRIP those files from the tray staging
#     below — the MSI's EngineFeature already installs the same binaries at
#     INSTALLFOLDER, and apps/desktop/main.js's `adjacentServerPath()` probes
#     that adjacent location when the bundled copy is missing.
# $npm is already resolved up in the SPA build step (step 1a) above.
$desktopVersion = Get-DesktopVersion -Version $Version
$desktopRoot = Join-Path $repoRoot "apps\desktop"
$desktopRuntime = Join-Path $desktopRoot "runtime"
$desktopRuntimeBin = Join-Path $desktopRuntime "bin"
$desktopDist = Join-Path $desktopRoot "dist"

Write-Host "`n== electron tray bundle =="
Write-Host "Desktop ver : $desktopVersion"

Remove-Item -LiteralPath $desktopRuntime -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $desktopDist -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $desktopRuntimeBin -Force | Out-Null
Copy-Item -LiteralPath $serverExe -Destination (Join-Path $desktopRuntime "xuva-server.exe") -Force
Copy-Item -LiteralPath (Join-Path $binStaging "ffmpeg.exe")  -Destination (Join-Path $desktopRuntimeBin "ffmpeg.exe")  -Force
Copy-Item -LiteralPath (Join-Path $binStaging "ffprobe.exe") -Destination (Join-Path $desktopRuntimeBin "ffprobe.exe") -Force

Push-Location $desktopRoot
try {
    & $npm ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci (apps/desktop) failed (exit $LASTEXITCODE)" }
    # `--win dir` = unpacked output only (no NSIS, no zip target). Override
    # the package.json `version` so the running tray reports the release
    # number (app.getVersion()) — matters for the tray-mode update check,
    # which compares against it.
    & $npm run "dist:win:unpacked" "--" "--config.extraMetadata.version=$desktopVersion"
    if ($LASTEXITCODE -ne 0) { throw "electron-builder (dir) failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
    # Clean the per-build runtime\ scratch dir so successive builds don't see
    # stale state and so source control stays clean.
    Remove-Item -LiteralPath $desktopRuntime -Recurse -Force -ErrorAction SilentlyContinue
}

$unpackedDir = Join-Path $desktopDist "win-unpacked"
if (-not (Test-Path -LiteralPath $unpackedDir)) {
    throw "electron-builder did not produce dist\win-unpacked"
}

# Copy the unpacked tree into the MSI staging area as tray\, which the WiX
# <Files Include="$(var.TraySource)\**"> will then auto-harvest into the
# TrayComponents ComponentGroup.
$trayStaging = Join-Path $staging "tray"
Remove-Item -LiteralPath $trayStaging -Recurse -Force -ErrorAction SilentlyContinue
Copy-Item -LiteralPath $unpackedDir -Destination $trayStaging -Recurse -Force
if (-not (Test-Path -LiteralPath (Join-Path $trayStaging "Xuva.exe"))) {
    throw "tray staging missing Xuva.exe (electron-builder output shape changed?)"
}

# Strip the duplicated server runtime from the tray bundle. electron-builder
# bundles xuva-server.exe + bin\ffmpeg/ffprobe at resources\runtime\* via
# package.json's extraResources entry — but the MSI's EngineFeature already
# installs those at INSTALLFOLDER\xuva-server.exe + INSTALLFOLDER\bin\*.
# Keeping the bundled copy duplicated ~140 MB into every MSI. apps/desktop/
# main.js (`packagedServerPath` -> `adjacentServerPath`) now probes the
# engine's adjacent install path when the bundled copy is missing, so the
# tray attaches to it (when a service is running) or spawns it (when not)
# instead. Both layouts still work; the bundle just sheds the dup.
$bundledRuntime = Join-Path $trayStaging "resources\runtime"
if (Test-Path -LiteralPath $bundledRuntime) {
    Remove-Item -LiteralPath $bundledRuntime -Recurse -Force
    Write-Host "Stripped duplicated $bundledRuntime from tray staging."
}

# 2) Build the MSI. Run wix from the staging dir so File/@Source paths
#    (xuva-server.exe, bin\ffmpeg.exe, $(var.TraySource)\...) resolve, with
#    the .wxs alongside. The TrayFeature ComponentGroup uses WiX 4/5's
#    built-in `<Files Include="$(var.TraySource)\**" />` auto-harvest (heat
#    is not a `wix` subcommand in v4/v5), so no separate fragment is needed.
Write-Host "`n== wix build =="
# Firewall rules are provisioned by the server itself (netsh, as LocalSystem) —
# see Xuva.wxs — so no WiX Firewall extension is needed.
Copy-Item -LiteralPath $wxsSource -Destination (Join-Path $staging "Xuva.wxs") -Force
# Strip any leading "v" from the tag so the file is xuva-server-v0.0.99.msi,
# not xuva-server-vv0.0.99.msi (release tags arrive as "v0.0.99"). Matches the
# single-"v" desktop asset naming (xuva-v0.0.99-win-x64.exe).
$fileVersion = $Version.TrimStart('v', 'V')
$msiOut = Join-Path $outputBase "xuva-server-v$fileVersion.msi"
Push-Location $staging
try {
    # -d Version=... feeds $(var.Version) used by Xuva.wxs for the MSI's
    # ProductVersion (MajorUpgrade then recognizes a newer MSI).
    # -d TraySource=tray feeds $(var.TraySource) used by the <Files Include>
    # glob inside the TrayComponents ComponentGroup, so each Electron file is
    # found relative to the staged tray\ folder.
    & wix build "Xuva.wxs" -arch x64 `
        -d "Version=$msiVersion" `
        -d "TraySource=tray" `
        -o $msiOut
    if ($LASTEXITCODE -ne 0) { throw "wix build failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}
if (-not (Test-Path -LiteralPath $msiOut)) { throw "MSI was not produced at $msiOut" }

$size = [math]::Round((Get-Item -LiteralPath $msiOut).Length / 1MB, 2)
Write-Host "`nMSI built: $msiOut ($size MB)"
