# Installs the ts client (tamsyam) from the latest GitHub release.
#   irm https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.ps1 | iex
# Env: TS_VERSION=0.1.0 to pin a version, TS_INSTALL_DIR to change the target dir.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

    $repo = 'Mi113r/tamsyam'
    $dir = if ($env:TS_INSTALL_DIR) { $env:TS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tamsyam' }

    if (-not [Environment]::Is64BitOperatingSystem) { throw '32-bit Windows is not supported' }
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }

    $version = if ($env:TS_VERSION) { $env:TS_VERSION } else {
        (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
    }
    $version = $version.TrimStart('v')

    $archive = "tamsyam_${version}_windows_$arch.zip"
    $base = "https://github.com/$repo/releases/download/v$version"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading ts $version (windows/$arch)..."
        Invoke-WebRequest "$base/$archive" -OutFile (Join-Path $tmp $archive) -UseBasicParsing
        Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

        $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" }
        $expected = ($line -split '\s+')[0]
        $actual = (Get-FileHash (Join-Path $tmp $archive) -Algorithm SHA256).Hash.ToLower()
        if (-not $expected -or $expected -ne $actual) { throw "checksum mismatch for $archive" }

        Expand-Archive (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x')
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Copy-Item (Join-Path $tmp 'x\ts.exe') (Join-Path $dir 'ts.exe') -Force
    } finally {
        Remove-Item -Recurse -Force $tmp
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $dir) {
        [Environment]::SetEnvironmentVariable('Path', ((@($userPath, $dir) | Where-Object { $_ }) -join ';'), 'User')
        $env:Path += ";$dir"
        Write-Host "Added $dir to your PATH. Open a new terminal to use ts everywhere."
    }
    Write-Host "Installed $(& (Join-Path $dir 'ts.exe') version) to $dir"
}
