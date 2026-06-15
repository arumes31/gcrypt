# gcrypt

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
![Windows](https://img.shields.io/badge/Platform-Windows%2010%2B-green.svg)
![Go](https://img.shields.io/badge/Go-1.26.4%2B-blue.svg)

**gcrypt** is a Windows desktop Google Drive sync client with client-side encryption. All files are encrypted locally before being uploaded to Google Drive, ensuring that no plaintext data ever leaves your machine.

## New in gcrypt v2

### ✨ Multi-Sync Support
Sync multiple directory pairs simultaneously. Configure and manage independent sync relationships from the system tray menu.

### 🛠️ Tray Menu Settings UI
Full settings configuration directly from the system tray—no config file editing required. Adjust global settings and per-pair options on the fly.

### 🔐 Enhanced Security
- Industry-standard **Argon2id** for key derivation (64 MiB memory, 3 iterations)
- Per-file unique keys with **AES-256-GCM** encryption
- File path binding via SHA-256 AAD to prevent relocation attacks
- Memory-protected master key using Windows VirtualLock

### 📊 Real-Time Status Monitoring
Visual status indicators for each sync pair (Idle, Syncing, Scanning, Error, Paused). Monitor global and per-directory sync statistics.

## Table of Contents

- [Features](#features)
- [Security Model](#security-model)
- [Installation](#installation)
- [Usage](#usage)
- [Configuration](#configuration)
- [Architecture](#architecture)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

---

## Features

- **End-to-End Encryption** — Files are encrypted with AES-256-GCM before upload. Each file gets a unique Data Encryption Key (DEK), wrapped by a Key Encryption Key (KEK) derived from your passphrase.
- **Multi-Sync Support** — Sync multiple directory pairs simultaneously, each with independent configuration.
- **System Tray Integration** — Monitor status, manage sync pairs, and configure settings from the Windows system tray.
- **Real-Time Sync** — Automatically detects local file changes and syncs them to Google Drive.
- **Per-File Encryption Keys** — Each file uses a unique DEK, ensuring key isolation.
- **Filename Encryption** — Filenames are encrypted with deterministic nonce based on file path, enabling safe syncing while preserving searchability.
- **Auto-Start with Windows** — gcrypt starts automatically with your session.
- **Batch Operations** — Pause/Resume All, Sync All Now for quick management.
- **Configurable Sync Intervals** — Per-pair and global sync interval settings.
- **Large File Support** — Configurable maximum file size limits.
- **Comprehensive Logging** — Detailed logs with configurable levels and rotation.

---

## Security Model

### Key Derivation (Master Key)
```
Passphrase + Salt (16 bytes)
     │
     ▼
Argon2id (memory=64 MiB, iterations=3, parallelism=4)
     │
     ▼
Master Key (256 bits / 32 bytes)
```

### Per-File Encryption (Data Encryption Key)
Each file gets its own unique DEK:
```
Random DEK (256 bits)
     │
     ▼
Encrypt with Master Key (AES-256-GCM)
     │
     └─► Encrypted DEK (48 bytes: 32 ciphertext + 16 tag)
     └─► DEK Nonce (12 bytes, random)
```

### File Content Encryption
```
File Content + DEK + FilePath
     │
     ▼
Hash FilePath (SHA-256) → AAD
     │
     ▼
Generate Content Nonce (12 bytes, random)
     │
     ▼
Encrypt with AES-256-GCM (AAD = SHA-256(filePath))
     │
     └─► Content Nonce (12 bytes, stored in header)
     └─► Ciphertext (includes 16-byte GCM tag)
```

### Encrypted File Format
```
┌─────────────────────────────────────────────────────────┐
│ Magic (6 bytes):  "GCRYPT"                              │
│ Version (2 bytes): 0x0001                               │
│ Encrypted DEK (48 bytes): AES-GCM ciphertext + tag      │
│ DEK Nonce (12 bytes): Random nonce for DEK decryption   │
│ Content Nonce (12 bytes): Random nonce for file content │
├─────────────────────────────────────────────────────────┤
│ Ciphertext (variable): File content + 16-byte GCM tag   │
└─────────────────────────────────────────────────────────┘
```

### Security Features

| Feature | Implementation |
|---------|---------------|
| Key Derivation | Argon2id (RFC 9106) |
| Encryption | AES-256-GCM (NIST SP 800-38D) |
| Master Key Protection | Windows VirtualLock + NOOVERWRITE |
| Nonce Generation | crypt/rand (OS entropy) |
| File Binding | SHA-256(filePath) as AAD |
| Filename Encryption | AES-256-GCM with deterministic nonce |

---

## Installation

### Prerequisites

- Windows 10 or later (64-bit)
- Google account with Drive access
- Go 1.26.4 or later (for building from source)

### Building from Source

```bash
# Clone the repository
git clone https://github.com/yourusername/gcrypt.git
cd gcrypt

# Build the application
go build -o gcrypt.exe ./cmd/gcrypt/

# Run setup (first time only)
./gcrypt.exe -setup
```

---

## Usage

### Starting gcrypt

#### From Command Line
```bash
# Run with default config
./gcrypt.exe

# Run with custom config path
./gcrypt.exe -config C:\path\to\config.yaml

# Run first-time setup
./gcrypt.exe -setup
```

#### Auto-Start
gcrypt automatically adds itself to Windows startup. Enable/disable in:
- Settings → Apps → Startup → gcrypt
- Or via tray menu: Settings → Auto-start with Windows

### System Tray Interface

gcrypt runs in the background with a system tray icon:

| Icon | Status |
|------|--------|
| 🟢 Green | Idle (all systems normal) |
| 🔵 Blue | Syncing in progress |
| 🟡 Yellow | Scanning files |
| 🔴 Red | Error (check log) |
| ⚪ Gray | Paused |

#### Tray Menu Structure
```
📊 Status: Idle              Selected folder: Syncing
📁 Files synced: 1,234       Last sync: 5 minutes ago

Sync Pairs ─────────────────
  🟢 Documents          🟢 Downloads
  ▶ Pause               ▶ Pause
  🔄 Sync Now           🔄 Sync Now
  📂 Open Folder        📂 Open Folder
  ─────────             ─────────
  ⚠️ Remove*            ⚠️ Remove*

─────────────────────────────
⏸️ Pause All
🔄 Sync All Now

Settings ───────────────────
  ☑ Auto-start with Windows
  ☐ Start minimized
  ─────────
  Sync Interval: [10s ▼]
  Max File Size: [100 MB ▼]
  Log Level: [Info ▼]
  Log Max Size: [10 MB ▼]
  Log Backups: [3 ▼]

─────────────────────────────
📂 Open Primary Folder
📋 View Log
─────────────────────────────
❌ Quit
```

---

## Configuration

### Config File Location
`%APPDATA%\gcrypt\config.yaml`

### V2 Configuration Format

```yaml
version: 2
sync_pairs:
  - id: "uuid-v4-here"
    local_dir: "C:\\Users\\username\\Documents"
    drive_folder_id: "drive-folder-id"
    enabled: true
    sync_interval: 30
app:
  auto_start: true
  log_level: "info"
  max_file_size: 104857600
```

---

## Architecture

### High-Level Architecture
The application consists of a **SyncManager** coordinating multiple independent **Engines**. Each engine manages a specific local-to-remote directory pair.

### Database Schema
SQLite is used for local state tracking, mapping local files to encrypted remote objects.

---

## Troubleshooting

### Common Issues
1. **Red Icon**: Authentication expired or network issue. Re-authenticate via Tray.
2. **Yellow Icon Stays**: Check logs for disk permission errors.
3. **Database Error**: Try deleting `.gcrypt-state/*.db` and re-syncing.

---

## Development

### Project Structure
- `cmd/gcrypt/`: Application entry point and setup wizard.
- `internal/sync/`: Core sync engine and multi-pair manager.
- `internal/crypto/`: AES-GCM and Argon2id implementations.
- `internal/drive/`: Google Drive API integration.
- `internal/service/`: Windows system tray and OS integration.

### Environment & Tools
- **Go Version**: Requires **Go 1.26.4+**.
- **Linting**: Uses `golangci-lint`.
- **Security**: Uses `gosec`, `govulncheck`, and `gitleaks`.

### Advanced Commands
```bash
# Run with Race Detector
go test -race ./...

# Run Performance Benchmarks
go test -bench=. ./internal/sync/

# Generate Coverage Report
go test -coverprofile=coverage.txt ./...
```

---

## Contributing
We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.
For security-related issues, please refer to our [Security Policy](SECURITY.md).

---

## License
MIT License - Copyright (c) 2024 gcrypt contributors

---

## Support
For issues and feature requests, please [open an issue](https://github.com/yourusername/gcrypt/issues).
Join our [Discord server](https://discord.gg/gcrypt) for real-time help.

---
*gcrypt v2.0.0 - The encrypted Google Drive sync client for Windows*
