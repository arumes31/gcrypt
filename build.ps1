# Builds gcrypt as a Windows GUI (tray) application.
#
# The `-H=windowsgui` linker flag marks the binary as a GUI subsystem app so
# Windows never creates a console window for it. (At runtime gcrypt also hides
# an owned console as a fallback for plain `go build` output — see
# cmd/gcrypt/console_windows.go.)

param(
    [string]$Output = "gcrypt.exe"
)

$ErrorActionPreference = "Stop"

Write-Host "Building $Output (windowsgui subsystem)..."
go build -ldflags "-H=windowsgui -s -w" -o $Output ./cmd/gcrypt
if ($LASTEXITCODE -ne 0) {
    throw "build failed"
}
Write-Host "Done: $Output"
