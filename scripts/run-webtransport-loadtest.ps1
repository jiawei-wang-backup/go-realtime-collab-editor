[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ApiUrl,
    [Parameter(Mandatory = $true)][string]$Endpoints,
    [Parameter(Mandatory = $true)][string]$CaFile,
    [Parameter(Mandatory = $true)][string]$Origin,
    [int]$Clients = 50,
    [int]$Documents = 200,
    [TimeSpan]$Ramp = ([TimeSpan]::Zero),
    [TimeSpan]$Online = ([TimeSpan]::FromMinutes(1)),
    [TimeSpan]$EditInterval = ([TimeSpan]::Zero),
    [int]$EditCount = 0,
    [string]$Prefix = 'wt-load'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not (Test-Path -LiteralPath $CaFile -PathType Leaf)) {
    throw "CA file does not exist: $CaFile"
}
if ([string]::IsNullOrWhiteSpace($env:WT_LOAD_PASSWORD)) {
    throw 'Set WT_LOAD_PASSWORD in the current PowerShell process; do not pass credentials as a command-line argument.'
}
if ($Clients -lt 1 -or $Documents -lt 1 -or $Online -le [TimeSpan]::Zero -or $Ramp -lt [TimeSpan]::Zero -or $EditInterval -lt [TimeSpan]::Zero -or $EditCount -lt 0) {
    throw 'Invalid load settings.'
}

$env:WT_LOAD_API_URL = $ApiUrl
$env:WT_LOAD_ENDPOINTS = $Endpoints
$env:WT_LOAD_CA_FILE = (Resolve-Path -LiteralPath $CaFile).Path
$env:WT_LOAD_ORIGIN = $Origin
$env:WT_LOAD_CLIENTS = [string]$Clients
$env:WT_LOAD_DOCUMENTS = [string]$Documents
function ConvertTo-GoDuration([TimeSpan]$Value) {
    if ($Value -eq [TimeSpan]::Zero) { return '0s' }
    return ('{0}s' -f $Value.TotalSeconds.ToString([Globalization.CultureInfo]::InvariantCulture))
}

$env:WT_LOAD_RAMP = ConvertTo-GoDuration $Ramp
$env:WT_LOAD_ONLINE = ConvertTo-GoDuration $Online
$env:WT_LOAD_EDIT_INTERVAL = ConvertTo-GoDuration $EditInterval
$env:WT_LOAD_EDIT_COUNT = [string]$EditCount
$env:WT_LOAD_PREFIX = $Prefix

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$log = Join-Path $env:TEMP "collab-webtransport-load-$stamp.log"
Write-Output "Starting Go WebTransport load client: clients=$Clients documents=$Documents online=$Online"
Write-Output "Log: $log"

& go run .\cmd\webtransport-loadtest 2>&1 | Tee-Object -FilePath $log
$exitCode = $LASTEXITCODE
Write-Output "ExitCode: $exitCode"
if ($exitCode -ne 0) {
    throw "WebTransport load client failed with exit code $exitCode. See $log"
}
