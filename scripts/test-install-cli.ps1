param([string] $Fixtures = (Join-Path $PSScriptRoot '..\build\cli-release'))
$ErrorActionPreference = 'Stop'
$Fixtures = (Resolve-Path -LiteralPath $Fixtures).Path
$sandbox = Join-Path ([IO.Path]::GetTempPath()) ('marionette-install-test-' + [Guid]::NewGuid().ToString('N'))
$oldUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$oldPath = $env:Path
$oldInstallDir = $env:MARIONETTE_INSTALL_DIR
$oldVersion = $env:MARIONETTE_VERSION
$tamper = $false

# Replace only HTTP transport; extract and execute the actual Windows release.
function Invoke-RestMethod { param($Uri, $Headers) return @{ tag_name = 'cli-v0.0.0-test' } }
function Invoke-WebRequest {
    param([switch] $UseBasicParsing, $Uri, $OutFile)
    if ($Uri -notlike 'https://github.com/h053698/marionette/releases/download/cli-v0.0.0-test/*') { throw "Unexpected URL: $Uri" }
    $name = ($Uri -split '/')[-1]
    Copy-Item -LiteralPath (Join-Path $Fixtures $name) -Destination $OutFile
    if ($tamper -and $name -ne 'SHA256SUMS') { [IO.File]::AppendAllText($OutFile, 'corrupted') }
}

try {
    $env:MARIONETTE_VERSION = $null
    $env:MARIONETTE_INSTALL_DIR = Join-Path $sandbox "path with ' quote\bin"
    $installer = Join-Path $PSScriptRoot 'install-cli.ps1'
    & $installer
    $binary = Join-Path $env:MARIONETTE_INSTALL_DIR 'marionette.exe'
    if ((Get-Command marionette).Source -ne $binary) { throw 'Installed CLI is missing from process PATH.' }
    & marionette --version
    if ($LASTEXITCODE -ne 0) { throw 'Installed CLI did not run.' }
    $original = (Get-FileHash -LiteralPath $binary).Hash
    $savedUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($savedUserPath -split ';') -notcontains $env:MARIONETTE_INSTALL_DIR) { throw 'User PATH was not persisted.' }

    & $installer
    if ([Environment]::GetEnvironmentVariable('Path', 'User') -ne $savedUserPath) { throw 'Repeated installation changed user PATH.' }
    if ((Get-FileHash -LiteralPath $binary).Hash -ne $original) { throw 'Repeated installation changed the binary.' }

    $tamper = $true
    $rejected = $false
    try { & $installer }
    catch {
        if ($_.Exception.Message -notlike '*SHA-256 verification failed*') { throw }
        $rejected = $true
    }
    if (-not $rejected) { throw 'Installer accepted a corrupted archive.' }
    if ((Get-FileHash -LiteralPath $binary).Hash -ne $original) { throw 'Failed verification replaced the binary.' }
    if ([Environment]::GetEnvironmentVariable('Path', 'User') -ne $savedUserPath) { throw 'Failed verification changed user PATH.' }
    Write-Host 'Windows installer: install, process/user PATH, upgrade and checksum failure passed.'
}
finally {
    [Environment]::SetEnvironmentVariable('Path', $oldUserPath, 'User')
    $env:Path = $oldPath
    $env:MARIONETTE_INSTALL_DIR = $oldInstallDir
    $env:MARIONETTE_VERSION = $oldVersion
    if (Test-Path -LiteralPath $sandbox) { Remove-Item -LiteralPath $sandbox -Recurse -Force }
}
