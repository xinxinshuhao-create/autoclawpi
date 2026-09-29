<#
.SYNOPSIS
    Compiles and installs the autoclawpi binary on Windows.
.DESCRIPTION
    Checks Go environment, downloads dependencies, compiles autoclawpi.exe,
    and verifies the build status.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

Write-Host "==> Checking prerequisites..." -ForegroundColor Cyan

# 1. Verify Go installation
$goCmd = Get-Command go -ErrorAction SilentlyContinue
if (-not $goCmd) {
    Write-Error "Go compiler was not found in PATH. Please install Go 1.26+ (https://go.dev/dl/)."
    exit 1
}

$goVersion = (& go version)
Write-Host "    Found: $goVersion" -ForegroundColor Green

# 2. Change to project directory
Set-Location $ScriptDir

# 3. Download dependencies
Write-Host "==> Downloading Go modules..." -ForegroundColor Cyan
& go mod download
if ($LASTEXITCODE -ne 0) {
    Write-Error "Failed to download Go modules."
    exit $LASTEXITCODE
}

# 4. Terminate any running instance before building
$running = Get-Process -Name "autoclawpi" -ErrorAction SilentlyContinue
if ($running) {
    Write-Host "    Stopping active autoclawpi process (PID $($running.Id))..." -ForegroundColor Yellow
    Stop-Process -Id $running.Id -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 600
}

# 5. Compile binary
Write-Host "==> Compiling autoclawpi.exe..." -ForegroundColor Cyan
$outPath = Join-Path $ScriptDir "autoclawpi.exe"

& go build -o $outPath ./cmd/autoclawpi
if ($LASTEXITCODE -ne 0) {
    Write-Error "Compilation failed."
    exit $LASTEXITCODE
}

# 6. Verify output
if (Test-Path $outPath) {
    $item = Get-Item $outPath
    $sizeMB = [math]::Round($item.Length / 1MB, 2)
    Write-Host "==> Build successful: $($item.FullName) ($sizeMB MB)" -ForegroundColor Green
    Write-Host "    You can start the server with: .\start-server.ps1" -ForegroundColor Yellow
} else {
    Write-Error "Binary was not created at $outPath"
    exit 1
}
