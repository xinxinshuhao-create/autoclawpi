<#
.SYNOPSIS
    Starts the autoclawpi local proxy server.
.DESCRIPTION
    Checks if binary exists, ensures port availability, and launches the server.
.PARAMETER Port
    HTTP listen port (default: 8787).
.PARAMETER WebPassword
    Password for the web management panel (default: admin).
.PARAMETER ApiKey
    Optional API key for /v1/ endpoints authentication.
.PARAMETER Hostname
    Listen address (default: 127.0.0.1).
.PARAMETER KillExisting
    Force kill any process occupying the target port.
#>
[CmdletBinding()]
param(
    [int]$Port = 8787,
    [string]$WebPassword = "admin",
    [string]$ApiKey = "",
    [string]$Hostname = "127.0.0.1",
    [switch]$KillExisting
)

$ErrorActionPreference = "Stop"
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ExePath = Join-Path $ScriptDir "autoclawpi.exe"

# 1. Verify binary exists (build if missing)
if (-not (Test-Path $ExePath)) {
    Write-Host "Binary not found at $ExePath. Running install.ps1 first..." -ForegroundColor Yellow
    & (Join-Path $ScriptDir "install.ps1")
    if (-not (Test-Path $ExePath)) {
        Write-Error "Unable to find or build autoclawpi.exe. Aborting."
        exit 1
    }
}

# 2. Check if port is already in use
$conn = Get-NetTCPConnection -LocalPort $Port -ErrorAction SilentlyContinue | Where-Object { $_.State -eq 'Listen' }
if ($conn) {
    $proc = Get-Process -Id $conn.OwningProcess -ErrorAction SilentlyContinue
    $procName = if ($proc) { $proc.ProcessName } else { "Unknown" }

    if ($proc -and $proc.ProcessName -eq "autoclawpi") {
        Write-Host "Recycling previous autoclawpi instance (PID $($proc.Id)) on port $Port..." -ForegroundColor Yellow
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 600
    } elseif ($KillExisting) {
        Write-Host "Terminating process $procName (PID $($conn.OwningProcess)) on port $Port..." -ForegroundColor Yellow
        Stop-Process -Id $conn.OwningProcess -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 600
    } else {
        Write-Error "Port $Port is already occupied by $procName (PID $($conn.OwningProcess)). Use -KillExisting or specify another -Port."
        exit 1
    }
}

# 3. Assemble arguments
$serverArgs = @("serve", "--host", $Hostname, "--port", "$Port", "--web-password", $WebPassword)
if (-not [string]::IsNullOrWhiteSpace($ApiKey)) {
    $serverArgs += @("--api-key", $ApiKey)
}

Write-Host "==> Starting autoclawpi server on http://${Hostname}:${Port}" -ForegroundColor Green
Write-Host "    Web Panel: http://${Hostname}:${Port}/" -ForegroundColor Cyan
Write-Host "    Password:  $WebPassword" -ForegroundColor Cyan
Write-Host "    API Base:  http://${Hostname}:${Port}/v1" -ForegroundColor Cyan
Write-Host "    Press Ctrl+C to stop the server.`n" -ForegroundColor Gray

# 4. Execute server
Set-Location $ScriptDir
& $ExePath @serverArgs
