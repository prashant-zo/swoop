# swoop installer for Windows
#   PowerShell:  irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex
#   cmd.exe:     powershell -c "irm https://raw.githubusercontent.com/prashant-zo/swoop/main/install.ps1 | iex"
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'   # the progress bar makes downloads very slow in Windows PowerShell 5.1
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$repo = 'prashant-zo/swoop'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "swoop_windows_$arch.exe"
$base = "https://github.com/$repo/releases/latest/download"
$dir = Join-Path $env:LOCALAPPDATA 'Programs\swoop'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("swoop-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $dir, $tmp | Out-Null

try {
    $exe = Join-Path $tmp $asset
    $sums = Join-Path $tmp 'checksums.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $exe
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums

    $line = Get-Content $sums | Where-Object { ($_ -split '\s+')[1] -eq $asset } | Select-Object -First 1
    if (-not $line) { throw "no checksum listed for $asset" }
    $expected = ($line -split '\s+')[0].ToLower()
    $actual = (Get-FileHash -Algorithm SHA256 -Path $exe).Hash.ToLower()
    if ($expected -ne $actual) { throw "checksum mismatch, refusing to install" }
    Write-Host "  downloaded $asset (checksum verified)"

    Copy-Item -Force $exe (Join-Path $dir 'swoop.exe')
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not (($userPath -split ';') -contains $dir)) {
    $newPath = if ([string]::IsNullOrEmpty($userPath)) { $dir } else { "$userPath;$dir" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$dir"
    Write-Host "  added $dir to your PATH (open a new terminal if 'swoop' is not found)"
}
Write-Host "  installed: $dir\swoop.exe ($(& (Join-Path $dir 'swoop.exe') -v))"
Write-Host "  run: swoop"
