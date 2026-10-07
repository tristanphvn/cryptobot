<#
  One-command setup for Windows:  .\scripts\setup.ps1 [-SkipModel] [-NoStart]

  1. checks Docker (required) and Go / Node (optional, for local dev)
  2. creates .env from .env.example with a random DB password and
     operator token (an existing .env is never overwritten)
  3. downloads Go modules and npm packages when Go / Node are present
  4. builds and starts the stack with docker compose
  5. pulls the Ollama model and waits until the dashboard answers

  Safe to re-run. Everything stays bound to 127.0.0.1 by default.
  If scripts are blocked: powershell -ExecutionPolicy Bypass -File .\scripts\setup.ps1
#>
param([switch]$SkipModel, [switch]$NoStart)
# Not 'Stop': Windows PowerShell 5.1 turns any stderr line of a native
# command (docker / npm progress, warnings) into a terminating error.
# Native calls are checked through $LASTEXITCODE instead.
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path -Parent $PSScriptRoot)

function Step($m) { Write-Host "`n==> $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "    + $m" -ForegroundColor Green }
function Warn($m) { Write-Host "    ! $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "error: $m" -ForegroundColor Red; exit 1 }
function Has($c)  { [bool](Get-Command $c -ErrorAction SilentlyContinue) }
function RandomHex([int]$bytes) {
  $b = New-Object byte[] $bytes
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
  -join ($b | ForEach-Object { $_.ToString('x2') })
}
# Value of KEY as docker compose sees it: a process environment variable
# wins over .env (last assignment), then the default.
function EnvValue($key, $default) {
  $fromEnv = [Environment]::GetEnvironmentVariable($key)
  if ($fromEnv) { return $fromEnv }
  $line = Get-Content .env -ErrorAction SilentlyContinue | Where-Object { $_ -match "^$key=" } | Select-Object -Last 1
  if ($line) { $v = $line.Substring($key.Length + 1); if ($v) { return $v } }
  return $default
}
# True when the native command exits 0; all of its output is discarded.
function Probe($exe, [string[]]$argv) {
  & $exe @argv 2>&1 | Out-Null
  return ($LASTEXITCODE -eq 0)
}
function Run($exe, [string[]]$argv) {
  & $exe @argv
  if ($LASTEXITCODE -ne 0) { Die "$exe $($argv -join ' ') failed (exit $LASTEXITCODE)" }
}

Step 'Checking prerequisites'
if (-not (Has docker)) { Die 'Docker is required: https://docs.docker.com/get-docker/' }
if (-not (Probe docker @('compose', 'version'))) { Die 'Docker Compose v2 is required (docker compose ...)' }
if (-not (Probe docker @('info')))              { Die 'Docker is installed but not running - start Docker Desktop' }
Ok "docker $(docker version --format '{{.Server.Version}}'), compose $(docker compose version --short)"
$haveGo = Has go; $haveNode = Has node
if ($haveGo)   { Ok "$((go version).Split(' ')[2]) (toolchain go1.26.6 auto-downloads if needed)" } else { Warn 'Go not found - skipped (only needed to run the backend outside docker)' }
if ($haveNode) { Ok "node $(node --version)" } else { Warn 'Node not found - skipped (only needed to run the frontend outside docker)' }

Step 'Environment file'
if (Test-Path .env) {
  Ok '.env exists - left untouched'
} else {
  if (-not (Test-Path .env.example)) { Die '.env.example is missing' }
  $pw = RandomHex 24; $token = RandomHex 32
  $content = (Get-Content .env.example) `
    -replace '^POSTGRES_PASSWORD=.*', "POSTGRES_PASSWORD=$pw" `
    -replace '^APPROVAL_TOKEN=.*', "APPROVAL_TOKEN=$token"
  # UTF-8 without BOM so docker compose parses the first line.
  [System.IO.File]::WriteAllLines((Join-Path (Get-Location) '.env'), $content, (New-Object System.Text.UTF8Encoding $false))
  Ok 'created .env with a random POSTGRES_PASSWORD and APPROVAL_TOKEN'
}

if ($haveGo) {
  Step 'Go modules'
  Push-Location backend; try { Run go @('mod', 'download'); Run go @('build', './...') } finally { Pop-Location }
  Ok 'backend builds'
}
if ($haveNode) {
  Step 'npm packages'
  Push-Location frontend; try { Run npm @('ci', '--no-audit', '--no-fund') } finally { Pop-Location }
  Ok 'frontend dependencies installed'
}

# Fail early with the variable to change instead of a docker bind error.
if (-not $NoStart -and -not (docker compose ps -q 2>&1 | Where-Object { $_ -is [string] -and $_ })) {
  Step 'Checking host ports'
  foreach ($p in @(@('BACKEND_PORT', '8081'), @('FRONTEND_PORT', '3002'), @('POSTGRES_PORT', '5432'), @('OLLAMA_PORT', '11434'))) {
    $port = [int](EnvValue $p[0] $p[1])
    $busy = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
    if ($busy) { Die "port $port is in use - set $($p[0]) to a free port in .env and re-run" }
  }
  Ok 'ports free'
}

if ($NoStart) { Step 'Done (-NoStart)'; Write-Host '    Start later with: docker compose up -d --build'; exit 0 }

Step 'Building and starting the stack (first build takes a few minutes)'
Run docker @('compose', 'up', '-d', '--build')
Ok 'containers started'

$model = EnvValue 'OLLAMA_MODEL' 'llama3'
if (-not $SkipModel) {
  Step "Pulling Ollama model '$model' (~4-5 GB the first time)"
  do { Start-Sleep 2 } until (Probe docker @('compose', 'exec', '-T', 'ollama', 'ollama', 'list'))
  Run docker @('compose', 'exec', '-T', 'ollama', 'ollama', 'pull', $model)
  Ok 'model ready'
} else {
  Warn "model pull skipped - run: docker compose exec ollama ollama pull $model"
}

$backendPort = EnvValue 'BACKEND_PORT' '8081'
$frontendPort = EnvValue 'FRONTEND_PORT' '3002'
Step 'Waiting for the services'
foreach ($url in @("http://127.0.0.1:$backendPort/health", "http://127.0.0.1:$frontendPort/")) {
  $up = $false
  for ($i = 0; $i -lt 90 -and -not $up; $i++) {
    try { Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5 | Out-Null; $up = $true } catch { Start-Sleep 2 }
  }
  if (-not $up) { Die "$url did not answer - check: docker compose logs --tail=100" }
  Ok $url
}

$mode = EnvValue 'EXECUTION_MODE' 'dry-run'
$note = if ($mode -eq 'live') { 'LIVE - real orders on Binance' } else { 'no real orders' }
Write-Host @"

  CryptoBot is running in $mode mode ($note).

    Dashboard  http://localhost:$frontendPort
    API        http://localhost:$backendPort/health

  Operator token (paste into Settings / the approval card): see APPROVAL_TOKEN in .env
  Stop: docker compose down      Logs: docker compose logs -f backend
"@
