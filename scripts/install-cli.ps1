# Windows PowerShell 5.1+; no administrator rights required.
& {
    $ErrorActionPreference = 'Stop'
    if ($env:OS -ne 'Windows_NT') { throw 'Use install.sh on macOS.' }
    $architecture = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $architecture = $env:PROCESSOR_ARCHITEW6432 }
    if ($architecture -ne 'AMD64') { throw 'This installer requires 64-bit Windows on Intel or AMD.' }

    $oldProtocol = [Net.ServicePointManager]::SecurityProtocol
    $temporary = $null
    $staged = $null
    try {
        [Net.ServicePointManager]::SecurityProtocol = $oldProtocol -bor [Net.SecurityProtocolType]::Tls12
        $tag = $env:MARIONETTE_VERSION
        if (-not $tag) {
            $release = Invoke-RestMethod -Uri 'https://api.github.com/repos/h053698/marionette/releases/latest' -Headers @{ 'User-Agent' = 'marionette-installer' }
            $tag = $release.tag_name
        }
        if ($tag -like 'v*') { $tag = "cli-$tag" }
        if ($tag -notmatch '^cli-v[0-9][A-Za-z0-9._-]*$') { throw "Invalid release version: $tag" }
        $temporary = Join-Path ([IO.Path]::GetTempPath()) ("marionette-install-" + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $temporary | Out-Null
        $asset = 'marionette-cli-windows-amd64.zip'
        $archive = Join-Path $temporary $asset
        $checksums = Join-Path $temporary 'SHA256SUMS'
        $base = "https://github.com/h053698/marionette/releases/download/$tag"
        Write-Host "Downloading Marionette $tag for Windows..."
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
        Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $checksums
        $pattern = '(?m)^([a-fA-F0-9]{64})[ \t]+\*?' + [Regex]::Escape($asset) + '\r?$'
        $checksum = [Regex]::Match([IO.File]::ReadAllText($checksums), $pattern)
        if (-not $checksum.Success -or (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $checksum.Groups[1].Value) {
            throw 'SHA-256 verification failed; installation was not changed.'
        }
        $unpacked = Join-Path $temporary 'unpacked'
        Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
        $installDir = $env:MARIONETTE_INSTALL_DIR
        if (-not $installDir) {
            $installDir = Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) 'Marionette\bin'
        }
        $installDir = [IO.Path]::GetFullPath($installDir)
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $staged = Join-Path $installDir ('.marionette-' + [Guid]::NewGuid().ToString('N') + '.exe')
        Copy-Item -LiteralPath (Join-Path $unpacked 'marionette-cli-windows-amd64\marionette.exe') -Destination $staged
        & $staged --version
        if ($LASTEXITCODE -ne 0) { throw 'Downloaded CLI could not run.' }
        Move-Item -LiteralPath $staged -Destination (Join-Path $installDir 'marionette.exe') -Force
        $staged = $null

        function Has-InstallPath([string] $Value) {
            foreach ($entry in ($Value -split ';')) {
                if ([Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"')).TrimEnd('\') -ieq $installDir.TrimEnd('\')) { return $true }
            }
            return $false
        }
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not (Has-InstallPath $userPath)) {
            $newPath = if ($userPath) { "$installDir;$userPath" } else { $installDir }
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        }
        if (-not (Has-InstallPath $env:Path)) { $env:Path = "$installDir;$env:Path" }
        Write-Host "Installed: $installDir\marionette.exe"
        Write-Host 'PATH registered. Run: marionette --version'
    }
    finally {
        [Net.ServicePointManager]::SecurityProtocol = $oldProtocol
        if ($staged -and (Test-Path -LiteralPath $staged)) { Remove-Item -LiteralPath $staged -Force }
        if ($temporary -and (Test-Path -LiteralPath $temporary)) { Remove-Item -LiteralPath $temporary -Recurse -Force }
    }
}
