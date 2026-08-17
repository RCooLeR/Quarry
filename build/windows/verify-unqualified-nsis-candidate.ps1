[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$InstallerPath,

    [Parameter(Mandatory = $true)]
    [string]$BinaryPath,

    [Parameter(Mandatory = $true)]
    [string]$BootstrapperPath,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$')]
    [string]$ProductVersion,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^\d+\.\d+\.\d+$')]
    [string]$PlatformVersion,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[1-9]\d*$')]
    [string]$BuildNumber,

    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-fA-F]{40}$')]
    [string]$BuildCommit,

    [Parameter(Mandatory = $true)]
    [string]$BuildDate,

    [Parameter(Mandatory = $true)]
    [string]$ExpectedNsisVersion,

    [Parameter(Mandatory = $true)]
    [string]$EvidencePath
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Get-PEMachine {
    param([Parameter(Mandatory = $true)][string]$LiteralPath)

    $stream = [System.IO.File]::Open(
        $LiteralPath,
        [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read,
        [System.IO.FileShare]::Read
    )
    try {
        $reader = [System.IO.BinaryReader]::new($stream)
        if ($reader.ReadUInt16() -ne 0x5A4D) {
            throw "File does not have an MZ header: $LiteralPath"
        }
        $stream.Position = 0x3C
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 0 -or $peOffset -gt ($stream.Length - 6)) {
            throw "File has an invalid PE header offset: $LiteralPath"
        }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) {
            throw "File does not have a PE signature: $LiteralPath"
        }
        return $reader.ReadUInt16()
    }
    finally {
        $stream.Dispose()
    }
}

function Assert-VersionInfo {
    param(
        [Parameter(Mandatory = $true)]$Item,
        [Parameter(Mandatory = $true)][string]$ExpectedProductVersion,
        [Parameter(Mandatory = $true)][string]$ExpectedNumericVersion
    )

    $version = $Item.VersionInfo
    if ($version.ProductVersion -ne $ExpectedProductVersion) {
        throw "$($Item.Name) ProductVersion '$($version.ProductVersion)' does not match '$ExpectedProductVersion'"
    }
    if ($version.FileVersion -ne $ExpectedProductVersion) {
        throw "$($Item.Name) FileVersion '$($version.FileVersion)' does not match '$ExpectedProductVersion'"
    }
    $fileNumeric = "$($version.FileMajorPart).$($version.FileMinorPart).$($version.FileBuildPart).$($version.FilePrivatePart)"
    $productNumeric = "$($version.ProductMajorPart).$($version.ProductMinorPart).$($version.ProductBuildPart).$($version.ProductPrivatePart)"
    if ($fileNumeric -ne $ExpectedNumericVersion -or $productNumeric -ne $ExpectedNumericVersion) {
        throw "$($Item.Name) numeric versions '$fileNumeric'/'$productNumeric' do not match '$ExpectedNumericVersion'"
    }
}

function Assert-Unsigned {
    param([Parameter(Mandatory = $true)]$Item)

    $signature = Get-AuthenticodeSignature -LiteralPath $Item.FullName
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::NotSigned) {
        throw "$($Item.Name) Authenticode status is '$($signature.Status)'; this workflow must remain explicitly unsigned until signing is qualified"
    }
}

$installer = Get-Item -LiteralPath $InstallerPath -ErrorAction Stop
$binary = Get-Item -LiteralPath $BinaryPath -ErrorAction Stop
$bootstrapper = Get-Item -LiteralPath $BootstrapperPath -ErrorAction Stop
if ($installer.PSIsContainer -or $installer.Length -eq 0) {
    throw "NSIS candidate is not a non-empty regular file: $InstallerPath"
}
if ($binary.PSIsContainer -or $binary.Length -eq 0) {
    throw "Quarry payload is not a non-empty regular file: $BinaryPath"
}
if ($installer.Name -ne "quarry-amd64-installer.exe") {
    throw "Unexpected NSIS output name '$($installer.Name)'; expected 'quarry-amd64-installer.exe'"
}
if ($binary.Name -ne "quarry.exe") {
    throw "Unexpected Windows payload name '$($binary.Name)'; expected 'quarry.exe'"
}

$expectedNumericVersion = "$PlatformVersion.$BuildNumber"
Assert-VersionInfo -Item $installer -ExpectedProductVersion $ProductVersion -ExpectedNumericVersion $expectedNumericVersion
Assert-VersionInfo -Item $binary -ExpectedProductVersion $ProductVersion -ExpectedNumericVersion $expectedNumericVersion

$payloadMachine = Get-PEMachine -LiteralPath $binary.FullName
if ($payloadMachine -ne 0x8664) {
    throw ("quarry.exe PE machine is 0x{0:x4}; expected AMD64 (0x8664)" -f $payloadMachine)
}

Assert-Unsigned -Item $installer
Assert-Unsigned -Item $binary

& "$PSScriptRoot/verify-webview2-bootstrapper.ps1" -Path $bootstrapper.FullName
$bootstrapSignature = Get-AuthenticodeSignature -LiteralPath $bootstrapper.FullName

$nsisVersion = (& makensis /VERSION | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $nsisVersion -ne $ExpectedNsisVersion) {
    throw "makensis version '$nsisVersion' does not match pinned '$ExpectedNsisVersion'"
}

$evidenceParent = Split-Path -Parent $EvidencePath
if ([string]::IsNullOrWhiteSpace($evidenceParent)) {
    throw "EvidencePath must include a parent directory"
}
New-Item -ItemType Directory -Force -Path $evidenceParent | Out-Null

$evidence = [ordered]@{
    schemaVersion = 1
    qualificationStatus = "unqualified-non-release-ci-evidence"
    target = "windows-10-or-11-x64-validation"
    buildCommit = $BuildCommit.ToLowerInvariant()
    buildDate = $BuildDate
    productVersion = $ProductVersion
    numericVersion = $expectedNumericVersion
    nsisVersion = $nsisVersion
    artifact = [ordered]@{
        name = $installer.Name
        sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $installer.FullName).Hash.ToLowerInvariant()
        authenticodeStatus = "NotSigned"
    }
    payload = [ordered]@{
        name = $binary.Name
        machine = "AMD64"
        sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $binary.FullName).Hash.ToLowerInvariant()
        authenticodeStatus = "NotSigned"
    }
    webView2Bootstrapper = [ordered]@{
        distribution = "Microsoft-signed Evergreen online bootstrapper"
        sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $bootstrapper.FullName).Hash.ToLowerInvariant()
        signerSubject = $bootstrapSignature.SignerCertificate.Subject
        signerThumbprint = $bootstrapSignature.SignerCertificate.Thumbprint
    }
}

$evidence | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $EvidencePath -Encoding utf8NoBOM
Write-Host "Verified unsigned Windows x64 NSIS validation candidate and wrote $EvidencePath"
