param(
    [Parameter(Mandatory = $true)]
    [string]$Executable
)

$ErrorActionPreference = "Stop"
$resolvedExecutable = (Resolve-Path -LiteralPath $Executable).Path
$objdump = (Get-Command objdump -ErrorAction Stop).Source
$imports = & $objdump -p $resolvedExecutable |
    Select-String -Pattern '^\s*DLL Name:\s*(.+)$' |
    ForEach-Object { $_.Matches[0].Groups[1].Value.Trim().ToLowerInvariant() }

if ($LASTEXITCODE -ne 0) {
    throw "Failed to inspect Windows runtime dependencies for $resolvedExecutable"
}

$forbidden = @(
    "libgcc_s_seh-1.dll",
    "libstdc++-6.dll",
    "libwinpthread-1.dll"
)
$unexpected = @($imports | Where-Object { $_ -in $forbidden })
if ($unexpected.Count -gt 0) {
    throw "Executable requires non-system MinGW runtime DLLs: $($unexpected -join ', ')"
}

Write-Host "Windows runtime dependency check passed: no external MinGW runtime DLLs."
