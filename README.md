# ScoutTrace

A local, open-source CLI and MCP proxy that makes LLM tool-call observability trivial.

ScoutTrace transparently wraps MCP servers, captures structured metadata about each tool call (with privacy-first redaction defaults), and forwards events to **any HTTP endpoint you configure**. The default destination is [WebhookScout](https://www.webhookscout.com), but ScoutTrace is destination-agnostic — point it at your own webhook, an internal sink, a local file, or `stdout`.

> **Status:** Pre-alpha. The MVP CLI implementation exists in this repository and is installable from the Applexica Homebrew tap; signed standalone release binaries are still pending.

## Installation

### Quick install

```sh
brew tap Applexica/tap
brew install scouttrace
scouttrace version
```

The recommended macOS/Linux install path is the Applexica Homebrew tap. Source installation remains available for development and for platforms where Homebrew is not preferred.

### Homebrew tap (macOS and Linux)

```sh
brew tap Applexica/tap
brew install scouttrace
scouttrace version
```

To upgrade later:

```sh
brew update
brew upgrade scouttrace
```

### Source install

The commands below build the `scouttrace` binary locally from this repository and put it on your `PATH`.

### Prerequisites

- Git
- Go 1.22 or newer
- A terminal/shell for your platform

Verify Go is available:

```sh
go version
```

#### macOS

Install prerequisites with Homebrew if needed:

```sh
brew install git go
```

Build and install ScoutTrace:

```sh
git clone https://github.com/Applexica/ScoutTrace.git
cd ScoutTrace
go test ./...
go build -o scouttrace ./cmd/scouttrace
mkdir -p "$HOME/.local/bin"
mv scouttrace "$HOME/.local/bin/scouttrace"
```

Add `~/.local/bin` to your shell path if it is not already there:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

Verify installation:

```sh
scouttrace version
scouttrace --help
```

#### Linux

Install prerequisites. Debian/Ubuntu:

```sh
sudo apt-get update
sudo apt-get install -y git golang-go
```

Fedora/RHEL:

```sh
sudo dnf install -y git golang
```

Arch Linux:

```sh
sudo pacman -S --needed git go
```

Build and install ScoutTrace:

```sh
git clone https://github.com/Applexica/ScoutTrace.git
cd ScoutTrace
go test ./...
go build -o scouttrace ./cmd/scouttrace
mkdir -p "$HOME/.local/bin"
mv scouttrace "$HOME/.local/bin/scouttrace"
```

Add `~/.local/bin` to your shell path if needed:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

Verify installation:

```sh
scouttrace version
scouttrace --help
```

#### Windows

Install prerequisites:

1. Install Git for Windows: <https://git-scm.com/download/win>
2. Install Go 1.22 or newer: <https://go.dev/dl/>
3. Open PowerShell and verify both are available:

```powershell
git --version
go version
```

Build ScoutTrace:

```powershell
git clone https://github.com/Applexica/ScoutTrace.git
cd ScoutTrace
go test ./...
go build -o scouttrace.exe ./cmd/scouttrace
```

Install it into a user-local bin directory:

```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\bin" | Out-Null
Move-Item .\scouttrace.exe "$env:USERPROFILE\bin\scouttrace.exe" -Force
```

Add that directory to your user `PATH` for future shells:

```powershell
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$Bin = "$env:USERPROFILE\bin"
if ($UserPath -notlike "*$Bin*") {
  [Environment]::SetEnvironmentVariable("Path", "$UserPath;$Bin", "User")
}
```

Close and reopen PowerShell, then verify installation:

```powershell
scouttrace version
scouttrace --help
```

#### Build without installing

If you only want to try ScoutTrace from a checkout:

```sh
go run ./cmd/scouttrace --help
go run ./cmd/scouttrace version
```

On Windows PowerShell, use the same commands.

### First setup after installation

For a safe local-only setup that sends captured events to stdout instead of the network:

```sh
scouttrace init --hosts none --destination stdout --yes
scouttrace doctor
```

For WebhookScout, use the portal-generated setup token or your WebhookScout configuration once those provisioning endpoints are available:

```sh
scouttrace init --destination webhookscout --setup-token <portal-setup-token> --yes
scouttrace doctor
```

> Do not paste API keys into MCP host config files. ScoutTrace config stores credential references such as `env://...` or `keychain://...`, not raw secrets.

### Updating from source

```sh
cd ScoutTrace
git pull
go test ./...
go build -o scouttrace ./cmd/scouttrace
mv scouttrace "$HOME/.local/bin/scouttrace"
```

On Windows, rebuild `scouttrace.exe` and move it back to `%USERPROFILE%\bin`.

### Future package managers

The following non-Homebrew install paths are planned but not yet published:

```sh
winget install WebhookScout.ScoutTrace
scoop install scouttrace
```

Until those packages exist, use Homebrew or the source installation steps above.

## Quick start

```sh
scouttrace init --hosts none --destination stdout --yes
scouttrace doctor
```

`scouttrace init` creates `~/.scouttrace/config.yaml`. Host patching can then be enabled with `scouttrace hosts patch` for supported MCP hosts (Claude Desktop, Claude Code, Cursor, Windsurf, Continue, Hermes). You can preview what will be captured before any network egress with `scouttrace preview --json`.

## Privacy & trust

- **No network egress without an explicit destination.** `stdout`, `file://...`, or a custom HTTP URL are first-class alternatives to WebhookScout.
- **Redaction on by default.** The `strict` profile strips well-known secret patterns and PII, normalizes paths, and truncates oversized payloads.
- **Capture-level deny.** Fields you don't capture can't leak, even if a redaction rule has a bug.
- **Inspectable everywhere.** Every state-changing command supports `--dry-run`, `--print-config`, and `--diff`. `scouttrace undo` reverts host patches from on-disk backups.
- **No hidden phone-home.** Self-telemetry is off by default; auto-update is never performed.

See [§17 Security & Threat Model](./docs/PRD.md#17-security--threat-model) and [§13 Redaction & Capture Policies](./docs/PRD.md#13-redaction--capture-policies) for details.

## Documentation

- [**Product Requirements Document**](./docs/PRD.md) — full spec: CLI taxonomy, config schema, payload schema, redaction policies, host-config patching, security model, MVP milestones, acceptance criteria, and testing strategy.
- [**Technical Design Document**](./docs/TECHNICAL_DESIGN.md) — implementation-level companion to the PRD: package layout, process model, wire-protocol details, queue schema, host-patching algorithms, and step-by-step testing procedures.

## License

Apache-2.0. See [LICENSE](./LICENSE).
