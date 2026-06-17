# install-mesa.ps1 — fetch a software OpenGL (Mesa3D llvmpipe) renderer for gcrypt.
#
# Why: Fyne (gcrypt's GUI) renders with OpenGL. Remote Desktop sessions, many
# VMs, and machines with broken GPU drivers have no usable hardware OpenGL, so
# the flyout window can't open. Mesa3D's "llvmpipe" is a pure-software OpenGL
# implementation that runs on the CPU and works everywhere.
#
# What this does: downloads the latest Mesa3D for Windows (MinGW build) from
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

Write-Host "Querying latest Mesa3D Windows release..."
$headers = @{ "User-Agent" = "gcrypt-install-mesa" }
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/pal1000/mesa-dist-win/releases/latest" -Headers $headers

# Prefer the self-contained MinGW build (no MSVC runtime dependency).
$asset = $release.assets | Where-Object { $_.name -like "*release-mingw.7z" } | Select-Object -First 1
if (-not $asset) {
    $asset = $release.assets | Where-Object { $_.name -like "*.7z" } | Select-Object -First 1
}
if (-not $asset) { throw "No suitable Mesa .7z asset found in the latest release." }

$tmp = Join-Path $env:TEMP ("mesa-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$archive = Join-Path $tmp $asset.name

Write-Host "Downloading $($asset.name) ($([math]::Round($asset.size/1MB,1)) MB)..."
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $archive -Headers $headers

Write-Host "Extracting..."
& $sevenZip x -y "-o$tmp" $archive | Out-Null

# The MinGW build extracts x64\ and x86\ folders; we want the 64-bit DLLs.
$x64 = Get-ChildItem -Path $tmp -Recurse -Directory | Where-Object { $_.Name -eq "x64" } | Select-Object -First 1
if (-not $x64) { throw "Could not find an 'x64' folder in the extracted Mesa archive." }

# llvmpipe software OpenGL only needs these two DLLs; the rest of the Mesa
# distribution (Vulkan, video, OpenCL, the ~180 MB clon12compiler.dll, ...) is
# irrelevant for gcrypt and would just bloat the install.
$needed = @("opengl32.dll", "libgallium_wgl.dll")
New-Item -ItemType Directory -Force -Path $mesaDir | Out-Null
$copied = 0
foreach ($name in $needed) {
    $src = Join-Path $x64.FullName $name
    if (-not (Test-Path $src)) { throw "Expected Mesa DLL not found: $name" }
    Copy-Item -Path $src -Destination (Join-Path $mesaDir $name) -Force
    $copied++
}

Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "Done. Installed $copied Mesa DLL(s) into: $mesaDir" -ForegroundColor Green
Write-Host "gcrypt will now fall back to software OpenGL automatically when hardware OpenGL is unavailable (e.g. over RDP)."
Write-Host "Tip: to force software OpenGL always, copy mesa\opengl32.dll (and the other mesa\*.dll) next to gcrypt.exe."
