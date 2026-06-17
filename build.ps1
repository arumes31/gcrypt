# Builds gcrypt as a Windows GUI (tray) application.
#
# The `-H=windowsgui` linker flag marks the binary as a GUI subsystem app so
# Windows never creates a console window for it. (At runtime gcrypt also hides
# an owned console as a fallback for plain `go build` output — see
# cmd/gcrypt/console_windows.go.)
#
# cgo note: gcrypt's GUI uses Fyne, which needs cgo (GLFW/OpenGL) on Windows.
# Go 1.26.x feeds this machine's MinGW gcc 16.x a clang-only flag
# (-Qunused-arguments) that gcc rejects, breaking cgo. build/gccwrap strips that
# flag and forwards to the real gcc; this script points CC at it. The first
# build is slow (~15-20 min) because go-gl's OpenGL bindings are a huge C file;
# subsequent builds are cached and fast.

param(
    [string]$Output = "gcrypt.exe"
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

# Build the gcc wrapper (pure Go, no cgo) if missing or out of date.
$wrap = Join-Path $root "tools\gccwrap\gccwrap.exe"
$wrapSrc = Join-Path $root "tools\gccwrap\main.go"
$wrapStale = (-not (Test-Path $wrap)) -or `
    ((Get-Item $wrapSrc).LastWriteTime -gt (Get-Item $wrap).LastWriteTime)
if ($wrapStale) {
    Write-Host "Building gcc wrapper..."
    go build -o $wrap (Join-Path $root "tools\gccwrap")
    if ($LASTEXITCODE -ne 0) { throw "failed to build gccwrap" }
}
$env:CC = $wrap
$env:CGO_ENABLED = "1"

Write-Host "Building $Output (windowsgui subsystem, CC=$env:CC)..."
go build -ldflags "-H=windowsgui -s -w" -o $Output ./cmd/gcrypt
if ($LASTEXITCODE -ne 0) {
    throw "build failed"
}
Write-Host "Done: $Output"
