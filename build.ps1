# ============================================================================
# Nginx Weekly Report - build script (Windows PowerShell)
#
# NOTE: This file is intentionally ASCII-only so that it parses correctly in
#       Windows PowerShell 5.1 (which decodes BOM-less files as ANSI/GBK).
#
# Usage:
#   .\build.ps1              # cross compile Linux amd64 -> nginx_weekly_report
#   .\build.ps1 -Windows     # build Windows amd64      -> nginx_weekly_report_win.exe
#   .\build.ps1 -Clean       # remove old artifact of the SAME target, then build
#   .\build.ps1 -Run         # build then run once with mock log (Windows only)
#
# Combine them: .\build.ps1 -Windows -Clean -Run
# ============================================================================

param(
    [switch]$Windows,
    [switch]$Run,
    [switch]$Clean
)

$ErrorActionPreference = "Stop"
# Static binaries, no cgo dependency (cross-compiling from Windows to Linux)
$env:CGO_ENABLED = "0"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$Main = Join-Path $Root "cmd/nginx-report"
$Go   = "C:\Program Files\Go\bin\go.exe"

if (-not (Test-Path -LiteralPath $Go)) {
    $GoCmd = Get-Command go -ErrorAction SilentlyContinue
    if (-not $GoCmd) {
        Write-Host "[ERROR] Go compiler not found. Install Go 1.21+ or edit `$Go at the top of this script." -ForegroundColor Red
        exit 1
    }
    $Go = $GoCmd.Source
}

# --- target platform ---------------------------------------------------------
if ($Windows) {
    $env:GOOS   = "windows"
    $env:GOARCH = "amd64"
    $Out        = Join-Path $Root "nginx_weekly_report_win.exe"
    $Label      = "Windows amd64"
} else {
    $env:GOOS   = "linux"
    $env:GOARCH = "amd64"
    $Out        = Join-Path $Root "nginx_weekly_report"
    $Label      = "Linux amd64"
}

# --- clean old artifact of the same target -----------------------------------
if ($Clean) {
    # also drop legacy names so stale binaries do not linger
    $legacy = @($Out)
    if ($Windows) { $legacy += (Join-Path $Root "nginx_weekly_report.exe") }
    foreach ($p in $legacy) {
        if (Test-Path -LiteralPath $p) {
            Remove-Item -LiteralPath $p -Force
            Write-Host "[INFO] Removed old artifact: $(Split-Path -Leaf $p)" -ForegroundColor Yellow
        }
    }
}

# --- build -------------------------------------------------------------------
Write-Host "[INFO] Building: $Label" -ForegroundColor Cyan
Write-Host "[INFO] Output  : $Out" -ForegroundColor Cyan
Push-Location $Root
try {
    & $Go build -trimpath -ldflags "-s -w" -o $Out $Main
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[ERROR] Build failed, exit code $LASTEXITCODE" -ForegroundColor Red
        exit $LASTEXITCODE
    }
}
finally {
    Pop-Location
}

$size = (Get-Item -LiteralPath $Out).Length
Write-Host "[OK] Build succeeded: $Out ($([math]::Round($size/1KB,1)) KB)" -ForegroundColor Green

# --- optional test run with mock log -----------------------------------------
if ($Run) {
    if (-not $Windows) {
        Write-Host "[INFO] Linux build cannot run here, skip test run (deploy to server)." -ForegroundColor Yellow
        Write-Host "[DONE]" -ForegroundColor Green
        exit 0
    }

    $ParentDir = Split-Path -Parent $Root
    $MockLog   = Join-Path $ParentDir "mock_access.log"
    if (-not (Test-Path -LiteralPath $MockLog)) {
        Write-Host "[WARN] mock_access.log not found next to the repo, skip test run." -ForegroundColor Yellow
        Write-Host "[DONE]" -ForegroundColor Green
        exit 0
    }

    # Pick the date from the first log line so "daily" runs on real data.
    $date = $null
    $firstLine = Get-Content -LiteralPath $MockLog -TotalCount 1
    if ($firstLine -match '"@timestamp":\s*"(\d{4}-\d{2}-\d{2})') {
        $date = $Matches[1]
    }
    if (-not $date) {
        Write-Host "[WARN] Cannot parse a date from mock_access.log, skip test run." -ForegroundColor Yellow
        Write-Host "[DONE]" -ForegroundColor Green
        exit 0
    }

    $ReportDir = Join-Path $Root "test_reports"
    Write-Host "[INFO] Test run (daily, date $date) ..." -ForegroundColor Cyan
    & $Out -mode daily -date $date -logdir $ParentDir -pattern "mock_access.log" -reportdir $ReportDir
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[WARN] Test run exited with code $LASTEXITCODE" -ForegroundColor Yellow
    }
}

Write-Host "[DONE]" -ForegroundColor Green
