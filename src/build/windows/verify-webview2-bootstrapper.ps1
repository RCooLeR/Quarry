[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Path
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$item = Get-Item -LiteralPath $Path -ErrorAction Stop
if ($item.PSIsContainer -or $item.Length -lt 1024) {
    throw "WebView2 bootstrapper is not a plausible regular executable: $Path"
}

$signature = Get-AuthenticodeSignature -LiteralPath $item.FullName
if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "WebView2 bootstrapper Authenticode status is '$($signature.Status)', expected 'Valid'"
}
if ($null -eq $signature.SignerCertificate) {
    throw "WebView2 bootstrapper has no signer certificate"
}

$subject = $signature.SignerCertificate.Subject
if ($subject -notmatch '(?:^|,\s*)(?:CN|O)=Microsoft Corporation(?:,|$)') {
    throw "WebView2 bootstrapper signer is not Microsoft Corporation: '$subject'"
}

$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $item.FullName).Hash.ToLowerInvariant()
Write-Host "Verified Microsoft-signed WebView2 online bootstrapper (SHA256 $hash)"
