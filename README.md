# Rover

[![CI](https://github.com/ylnhari/rover/actions/workflows/ci.yml/badge.svg)](https://github.com/ylnhari/rover/actions/workflows/ci.yml)
[![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-blue)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A lightweight, zero-dependency, single-binary tool that lets you run shell commands and manage local server projects remotely from a browser — with real-time streaming output, HMAC-signed session tokens, and a dark-theme chat-style UI.

```sh
# Generate a strong secret
export ROVER_SECRET=$(openssl rand -hex 32)

# Start rover
rover serve --addr :2278
```

Open [http://localhost:2278](http://localhost:2278) and log in with your secret.

---

## Features

### Remote Command Execution
- **Session-based** — every command gets a unique ID; history survives the browser closing
- **Real-time SSE** — output streams live as it's generated, line by line
- **Disconnect-safe** — close the browser mid-command and reconnect to see full output
- **Session persistence** — completed sessions survive server restarts (saved to `sessions.json`)
- **Command allowlist** — restrict which command prefixes are permitted via `--allow`
- **Chat-like UI** — dark theme, mobile-first, scrollable command→output history

### Project Launcher
- **Start/stop projects** from the browser — Python scripts, Node servers, Go programs, etc.
- **Auto URL detection** — Rover extracts the URL/port from stdout when a project starts
- **Live console streaming** — view project logs in real time via SSE
- **Persistent registry** — projects are saved to `projects_registry.json` (git-ignored)
- **Clean shutdown** — all launched projects are killed when Rover exits

### Security
- **HMAC-SHA256 session tokens** — login returns a signed, time-limited token (24h TTL); the raw secret is never stored in the browser
- **`X-Rover-Secret` header auth** — all protected endpoints require the token header
- **Rate-limited login** — 10 attempts per IP per minute
- **Command allowlist** — `--allow git,go test,npm` blocks everything else
- **Security headers** — `X-Frame-Options`, `X-Content-Type-Options`, `Content-Security-Policy`
- **Optional TLS** — `--tls-cert` / `--tls-key`
- **Structured audit log** — every exec and login event is logged with IP and timestamp

---

## Installation

### Download a binary

Grab the latest release from the [Releases page](https://github.com/ylnhari/rover/releases).

```sh
# Linux/macOS
curl -L https://github.com/ylnhari/rover/releases/latest/download/rover-linux-amd64 -o rover
chmod +x rover

# Verify checksum
sha256sum -c rover-linux-amd64.sha256
```

### Build from source

```sh
git clone https://github.com/ylnhari/rover
cd rover
go build -o rover .
```

**Requires Go 1.23+** (uses stdlib `log/slog`, new `http.ServeMux` patterns).

---

## Usage

```
rover serve [flags]

Flags:
  --addr           host:port   listen address                        (default: :2278)
  --secret         string      shared secret  (or $ROVER_SECRET)
  --allow          string      comma-separated command prefix allowlist (empty = allow all)
  --tls-cert       path        TLS certificate file
  --tls-key        path        TLS private key file
  --exec-timeout   duration    max run time per command              (default: 10m)
  --max-output     int         max output bytes per command          (default: 1MB)
  --projects-dir   path        projects root directory
  --log-format     text|json   log output format                     (default: text)
```

### Examples

```sh
# No auth — trusted network only
rover serve

# With secret from env
ROVER_SECRET=$(openssl rand -hex 32) rover serve

# Restrict allowed commands
rover serve --allow "git,go test,npm run,python"

# JSON logs (for log aggregators)
rover serve --log-format json

# TLS
rover serve --tls-cert /etc/ssl/rover.crt --tls-key /etc/ssl/rover.key
```

---

## API Reference

All protected endpoints require the `X-Rover-Secret: <token>` header, where `<token>` is the value returned by `POST /api/auth`. SSE endpoints (`/stream`) additionally accept `?secret=<token>` as a query parameter (browser `EventSource` cannot set custom headers).

| Method | Path | Auth | Description |
|--------|------|:----:|-------------|
| GET | `/` | — | Web UI |
| GET | `/ping` | — | Health check |
| GET | `/api/auth` | — | Returns `{"required": true/false}` |
| POST | `/api/auth` | — | Login — returns `{"token":"…","expires_at":"…"}` |
| GET | `/api/sessions` | ✓ | List all sessions |
| POST | `/api/sessions` | ✓ | Create and execute a command |
| GET | `/api/sessions/{id}` | ✓ | Get session detail (stdout, stderr, exit code) |
| GET | `/api/sessions/{id}/stream` | ✓ | SSE real-time output stream |
| GET | `/api/config` | ✓ | Get exec timeout and max output |
| PUT | `/api/config` | ✓ | Update exec timeout and max output |
| GET | `/api/projects` | ✓ | List registered projects |
| POST | `/api/projects` | ✓ | Add a project (validates by starting it) |
| DELETE | `/api/projects/{name}` | ✓ | Remove a project from the registry |
| GET | `/api/projects/dirs` | ✓ | List available unregistered directories |
| GET | `/api/projects/{name}/files` | ✓ | List eligible start files in a directory |
| POST | `/api/projects/{name}/start` | ✓ | Start a project |
| POST | `/api/projects/{name}/stop` | ✓ | Stop a running project |
| GET | `/api/projects/{name}/stream` | ✓ | SSE live console output |

---

## Architecture

```
Browser ──────────────────────────────────────────────► Rover (single binary)
  │  Terminal (chat UI)    POST /api/sessions             │
  │  Projects tab          GET  /api/sessions/{id}/stream │
  │                        GET  /api/projects             │
  │                        POST /api/projects/{name}/start│
  └──────────────── SSE ◄──────────────────────────────── │
                                                          │
                                          ┌───────────────┤
                                          │ SessionManager│  (in-memory + sessions.json)
                                          │ launcher.Manager │ (projects_registry.json)
                                          └───────────────┘
                                                          │
                                                          ▼
                                                   Child Processes
```

### Packages

| Package | Responsibility |
|---------|----------------|
| `cmd/` | CLI entry point, flag parsing |
| `internal/server/` | HTTP server, routes, SSE, web UI, session management |
| `internal/launcher/` | Project lifecycle, process management, URL detection |
| `internal/auth/` | HMAC-SHA256 signing, token issuance and verification |
| `internal/protocol/` | Shared request/response types |
| `internal/version/` | Build version info (injected via `-ldflags`) |

---

## Security Considerations

See [SECURITY.md](SECURITY.md) for the full threat model and responsible disclosure policy.

**Summary:**
- Always set `--secret` in any networked deployment
- Restrict port access at the firewall level — Rover is not designed for the open internet
- Use `--allow` to limit which commands can be executed
- Enable TLS if traffic crosses an untrusted network
- Rotate the secret periodically (existing tokens become invalid immediately)

---

## Development

```sh
# Run tests
go test ./... -v -count=1 -timeout 60s

# Lint (requires golangci-lint)
golangci-lint run ./...

# Build with version
make build VERSION=0.1.0

# Cross-compile all platforms
make dist VERSION=0.1.0
```

### Releasing

Tag the commit and push — the release workflow builds and uploads binaries automatically:

```sh
git tag v0.1.0
git push origin v0.1.0
```

---

## Project Launcher — How It Works

1. Click **"Add Project"** in the Projects tab
2. Select a directory from the list
3. Select a start file (`.py`, `.sh`, `.bat`, `.js`, `.ts`, `.go`, etc.)
4. Click **"Validate & Add"** — Rover runs the script for up to 15 seconds
5. If an HTTP URL is detected in stdout, the project is saved to `projects_registry.json`

The registry file is personal and git-ignored — each installation has its own.

**Supported extensions:** `.py` `.sh` `.bat` `.ps1` `.js` `.ts` `.go` `.rb` `.php` `.pl` `.lua`

---

## FAQ

**Q: Does Rover store my secret anywhere?**  
A: No. The raw secret never leaves the server. The browser stores only the signed 24-hour token in `sessionStorage` (cleared when the tab closes).

**Q: Are sessions saved across restarts?**  
A: Yes — completed sessions are persisted to `sessions.json` next to the binary. Running sessions are lost on restart.

**Q: How do I restrict which commands can run?**  
A: Use `--allow "git,go test,npm"`. Rover will reject any command that doesn't start with one of those prefixes.

**Q: Can I use Rover over the internet?**  
A: Only with TLS + a strong secret + a firewall or VPN. Read [SECURITY.md](SECURITY.md) first.

**Q: What happens to running projects when Rover shuts down?**  
A: All child processes are killed during graceful shutdown.
