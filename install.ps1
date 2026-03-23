# Mantis installer for Windows
# Usage:  irm https://raw.githubusercontent.com/seedhire/mantis/main/install.ps1 | iex
# Or:     .\install.ps1 -InstallDir "C:\tools" -Version "v0.7.6"

param(
    [string]$InstallDir = "$env:LOCALAPPDATA\mantis\bin",
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
$Repo = "seedhire/mantis"

# ── Detect architecture ──────────────────────────────────────────────────────
$Arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else {
    Write-Error "Mantis requires a 64-bit Windows system."
    exit 1
}

# ── Resolve version ──────────────────────────────────────────────────────────
if (-not $Version) {
    Write-Host "Fetching latest release... " -NoNewline
    $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
    $Version = $release.tag_name
    Write-Host $Version
}

# ── Download ─────────────────────────────────────────────────────────────────
$Filename = "mantis_windows_$Arch.zip"
$Url = "https://github.com/$Repo/releases/download/$Version/$Filename"
$TempDir = Join-Path $env:TEMP "mantis-install-$(Get-Random)"
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

Write-Host "Downloading $Filename... " -NoNewline
try {
    Invoke-WebRequest -Uri $Url -OutFile (Join-Path $TempDir $Filename) -UseBasicParsing
    Write-Host "done"
} catch {
    Write-Error "Download failed. Check that $Version has a Windows build at: $Url"
    exit 1
}

# ── Extract ──────────────────────────────────────────────────────────────────
Write-Host "Extracting... " -NoNewline
Expand-Archive -Path (Join-Path $TempDir $Filename) -DestinationPath $TempDir -Force
Write-Host "done"

# ── Install ──────────────────────────────────────────────────────────────────
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

$ExePath = Join-Path $TempDir "mantis.exe"
if (-not (Test-Path $ExePath)) {
    Write-Error "mantis.exe not found in archive."
    exit 1
}

Copy-Item $ExePath (Join-Path $InstallDir "mantis.exe") -Force

# ── Add to PATH (user-level, persists across sessions) ───────────────────────
$UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($UserPath -notlike "*$InstallDir*") {
    [Environment]::SetEnvironmentVariable("PATH", "$UserPath;$InstallDir", "User")
    $env:PATH = "$env:PATH;$InstallDir"
    Write-Host ""
    Write-Host "Added $InstallDir to your PATH."
    Write-Host "Restart your terminal for PATH changes to take effect."
}

# ── Clean up ─────────────────────────────────────────────────────────────────
Remove-Item -Recurse -Force $TempDir -ErrorAction SilentlyContinue

# ── Verify ───────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "Mantis $Version installed to $InstallDir\mantis.exe"
Write-Host ""
& (Join-Path $InstallDir "mantis.exe") --help 2>$null | Select-Object -First 6
Write-Host ""
Write-Host "Get started:"
Write-Host "  cd your-project"
Write-Host "  mantis init --lang ts"
Write-Host "  mantis"
