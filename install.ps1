<#
.SYNOPSIS
    Installs the biga CLI on Windows from its GitHub releases.

.DESCRIPTION
    irm https://raw.githubusercontent.com/biga-app/cli/main/install.ps1 | iex

    The download is checked against the release's checksums.txt, and when
    cosign is installed, checksums.txt is checked against its Sigstore
    signature. The install directory is added to your user PATH.

    Environment:
      BIGA_VERSION      version to install, e.g. 1.0.0 (default: the latest)
      BIGA_INSTALL_DIR  directory to install into
                        (default: %LOCALAPPDATA%\Programs\biga)
      BIGA_RELEASE_URL  base URL serving a release's files, for testing
#>
$ErrorActionPreference = 'Stop'
# Progress bars make Invoke-WebRequest many times slower in Windows PowerShell.
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version Latest

$repo = 'biga-app/cli'

# Windows PowerShell 5.1 does not enable TLS 1.2 by default.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# Throws rather than exits: under `irm | iex` this runs in the user's own
# session, which `exit` would close.
function Fail([string]$message) {
    throw "biga install: $message"
}

# The OS architecture, not the process's: an x64 PowerShell can run on ARM64.
try {
    $osArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
} catch {
    # .NET Framework before 4.7.1; PROCESSOR_ARCHITEW6432 is set in a 32-bit
    # process on a 64-bit OS.
    $osArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
}
switch ($osArch) {
    { $_ -in 'X64', 'AMD64' } { $arch = 'amd64' }
    'Arm64' { $arch = 'arm64' }
    default { Fail "unsupported CPU: $osArch" }
}

$version = $env:BIGA_VERSION
if (-not $version) {
    try {
        $release = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'biga-install' }
        $version = $release.tag_name
    } catch {
        Fail "could not find the latest release: $_"
    }
}
$version = $version.TrimStart('v')
$tag = "v$version"

$base = if ($env:BIGA_RELEASE_URL) { $env:BIGA_RELEASE_URL } else { "https://github.com/$repo/releases/download/$tag" }
$archive = "biga_${version}_windows_${arch}.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("biga-" + [System.Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading biga $version for windows/$arch..."
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')
    } catch {
        Fail "could not download $base/$archive`: $_"
    }

    $expected = Get-Content (Join-Path $tmp 'checksums.txt') |
        ForEach-Object { $hash, $name = $_ -split '\s+', 2; if ($name -eq $archive) { $hash } } |
        Select-Object -First 1
    if (-not $expected) { Fail "$archive is not listed in checksums.txt" }
    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash
    if ($actual -ne $expected) { Fail "checksum mismatch for $archive; refusing to install" }

    if ((Get-Command cosign -ErrorAction SilentlyContinue) -and -not $env:BIGA_RELEASE_URL) {
        $bundle = Join-Path $tmp 'checksums.txt.sigstore.json'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt.sigstore.json" -OutFile $bundle
        & cosign verify-blob `
            --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" `
            --certificate-oidc-issuer https://token.actions.githubusercontent.com `
            --bundle $bundle (Join-Path $tmp 'checksums.txt') 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { Fail 'the release signature does not verify; refusing to install' }
        Write-Host 'Verified the release signature.'
    }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'out') -Force

    $dir = if ($env:BIGA_INSTALL_DIR) { $env:BIGA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\biga' }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $target = Join-Path $dir 'biga.exe'
    try {
        Copy-Item -Path (Join-Path $tmp 'out\biga.exe') -Destination $target -Force
    } catch {
        Fail "could not write $target (is biga still running?): $_"
    }
    Write-Host "Installed biga $version to $target"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = if ($userPath) { $userPath -split ';' } else { @() }
    if ($entries -notcontains $dir) {
        [Environment]::SetEnvironmentVariable('Path', (($entries + $dir) -join ';'), 'User')
        Write-Host "Added $dir to your user PATH; open a new terminal to use biga."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
