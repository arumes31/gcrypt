# install-mesa.ps1 — fetch a software OpenGL (Mesa3D llvmpipe) renderer for gcrypt.
#
# Why: Fyne (gcrypt's GUI) renders with OpenGL. Remote Desktop sessions, many
# VMs, and machines with broken GPU drivers have no usable hardware OpenGL, so
# the flyout window can't open. Mesa3D's "llvmpipe" is a pure-software OpenGL
# implementation that runs on the CPU and works everywhere.
#
# What this does: downloads a reviewed Mesa3D for Windows (MinGW build) from
# pal1000/mesa-dist-win, extracts it, and places the 64-bit DLLs into a "mesa"
# folder next to gcrypt.exe. At runtime gcrypt detects inadequate OpenGL (RDP /
# VM / bad drivers), copies these DLLs beside the exe and relaunches, so the GUI
# renders in software automatically. On a normal GPU machine the bundled Mesa is
# ignored and hardware OpenGL is used.
#
# Usage:   pwsh -ExecutionPolicy Bypass -File .\install-mesa.ps1
#
# Requires 7-Zip (7z.exe) on PATH or at "C:\Program Files\7-Zip\7z.exe". If you
# don't have it, install with:  winget install 7zip.7zip

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$mesaDir = Join-Path $root "mesa"
$mesaVersion = "26.2.0"
$assetName = "mesa3d-$mesaVersion-release-mingw.7z"
$assetSha256 = "533df99ef01c777406f4c4f656a3ab149f03216abd69397807b212f88717cac9"
$assetUrl = "https://github.com/pal1000/mesa-dist-win/releases/download/$mesaVersion/$assetName"

function Find-7Zip {
    $cmd = Get-Command 7z.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $default = "C:\Program Files\7-Zip\7z.exe"
    if (Test-Path $default) { return $default }
    return $null
}

$sevenZip = Find-7Zip
if (-not $sevenZip) {
    throw "7-Zip not found. Install it (winget install 7zip.7zip) and re-run, or extract a Mesa release manually into '$mesaDir'."
}

$headers = @{ "User-Agent" = "gcrypt-install-mesa" }

$tmp = Join-Path $env:TEMP ("mesa-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$archive = Join-Path $tmp $assetName

try {
    Write-Host "Downloading reviewed Mesa $mesaVersion asset..."
    Invoke-WebRequest -Uri $assetUrl -OutFile $archive -Headers $headers -MaximumRedirection 3

    $actualSha256 = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha256 -cne $assetSha256) {
        throw "Mesa archive checksum mismatch. Expected $assetSha256, received $actualSha256."
    }

    Write-Host "Checksum verified. Extracting..."
    & $sevenZip x -y "-o$tmp" $archive | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "7-Zip extraction failed with exit code $LASTEXITCODE." }

    # The MinGW build extracts x64\ and x86\ folders; we want the 64-bit DLLs.
    $x64 = Get-ChildItem -Path $tmp -Recurse -Directory | Where-Object { $_.Name -eq "x64" } | Select-Object -First 1
    if (-not $x64) { throw "Could not find an 'x64' folder in the extracted Mesa archive." }

    # llvmpipe software OpenGL only needs these two DLLs.
    $needed = @("opengl32.dll", "libgallium_wgl.dll")
    New-Item -ItemType Directory -Force -Path $mesaDir | Out-Null
    $copied = 0
    foreach ($name in $needed) {
        $src = Join-Path $x64.FullName $name
        if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { throw "Expected Mesa DLL not found: $name" }
        Copy-Item -LiteralPath $src -Destination (Join-Path $mesaDir $name) -Force
        $copied++
    }
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Done. Installed $copied Mesa DLL(s) into: $mesaDir" -ForegroundColor Green
Write-Host "gcrypt will now fall back to software OpenGL automatically when hardware OpenGL is unavailable (e.g. over RDP)."
Write-Host "Tip: to force software OpenGL always, copy mesa\opengl32.dll (and the other mesa\*.dll) next to gcrypt.exe."
