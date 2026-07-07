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
- **Real server validation** — registration and every start probe the port (TCP connect, then an HTTP request) until the app actually listens; the URL is only advertised once confirmed
- **Truthful, viewer-aware links** — a URL renders as a clickable link only when it will work *from where you're looking*: the proxy link everywhere, a direct link only after rover dialed the app's socket and got a connection, and loopback addresses clickable only in a browser on the rover host itself (informational text on other devices). See [Dashboard links](#dashboard-links--which-url-opens-what-from-where)
- **Web / TCP / task project kinds** — HTTP servers get links + proxy; a raw TCP listener gets an honest "TCP · no proxy" chip instead of a proxy link that could only fail; port-less workers/scripts get console-only cards. See [Project kinds](#project-kinds-web-tcp-task)
- **Honest lifecycle** — children are reaped on exit (no zombies); a crash shows **Failed (exit N)**, never a stale "Running"
- **Safe port conflicts** — an occupied port is never freed by a silent kill: rover identifies the listener (PID, name, cmdline) and offers **Adopt** / confirmed **Kill & start** / another port
- **Adopt running servers** — attach rover (tracking + proxy) to an app already listening on the project's port, e.g. one you started by hand
- **Live console streaming** — view project logs in real time via SSE (bounded in-memory history)
- **Reverse proxy (default ON)** — Rover can proxy requests to apps bound to `127.0.0.1`, making them accessible via Tailscale/LAN without changing app configuration. Proxy ports are stable across restarts (bookmarkable); the proxy stops forwarding the moment the app dies. Toggle per-project in the dashboard.
- **Persistent registry** — projects are saved to `projects_registry.json` (git-ignored, atomic 0600 writes)
- **Clean shutdown** — all launched projects are killed when Rover exits

### Security
- **HMAC-SHA256 session tokens** — login returns a signed, time-limited token (24h TTL); the raw secret is never stored in the browser
- **`X-Rover-Secret` header auth** — all protected endpoints require the token header
- **Secret required off-loopback** — Rover refuses to start without a secret unless bound to `127.0.0.1`; secret-less mode can never expose the host to the network
- **Proxy auth (`--proxy-auth auto|on|off`)** — project proxies can require the same rover login via a signed HttpOnly cookie; `auto` keeps them open on loopback/Tailscale binds (where the network is the boundary) and gates them everywhere else
- **Rate-limited login** — 10 attempts per IP per minute
- **Command allowlist** — `--allow git,go test,npm` blocks everything else, **including project start commands**
- **No blind kills** — neither project start nor rover's own startup ever `kill -9`s a port occupant without explicit confirmation (`--takeover-port` / confirmed PID)
- **Security headers** — `X-Frame-Options`, `X-Content-Type-Options`, `Content-Security-Policy`
- **Optional TLS** — `--tls-cert` / `--tls-key`
- **Structured audit log** — every exec, login, project change, adopt and confirmed kill is logged with IP and timestamp

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
  --no-command-guard           allow interactive/GUI/stateful commands (default: blocked)
  --proxy-auth     auto|on|off require rover login for project proxies (default: auto —
                               off on loopback/Tailscale binds, on elsewhere)
  --takeover-port              kill whatever holds rover's own port instead of failing
                               with the occupant's identity (default: fail and name it)
  --validation-timeout dur     how long registration/start probes wait for the app
                               to start listening                    (default: 30s)
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

## How commands run (and their limits)

Each command in the **Terminal** tab runs as a **separate, non-interactive process** on the machine where rover runs — wrapped as `cmd /C <command>` (Windows) or `sh -c <command>` (Unix). Worth knowing:

- **No input / no TTY.** stdin is not connected and no terminal is allocated, so anything that prompts (passwords, confirmations) or needs a terminal — `vim`, `top`, `python`/`node` with no script, `ssh`, `git rebase -i`, `git commit` without `-m` — would **hang until the timeout**. rover **blocks these by default** (see below); you can't type into a running command.
- **Each command is a fresh shell.** Nothing persists between commands — `cd`, `export`, `source`/venv activation affect only that one run. Chain instead: `cd app && npm test`.
- **It runs on the host, not your browser.** A command that opens a file/app/browser tab (`start`, `open`, `xdg-open`, `chrome`, `code`, `notepad`, …) does so on the **rover host's desktop** — you won't see it, and GUI apps produce no output. Foreground apps block until closed; detached ones keep running on the host.
- **Terminal commands can't be stopped from the UI.** They run until they exit or hit `--exec-timeout` (default 10m) or `--max-output` (default 1MB). For long-running servers/watchers, use the **Projects** tab (it has start/stop) instead.
- **Interactive / GUI / stateful commands are blocked by default.** rover rejects commands that can't work in this model — interactive programs (editors, REPLs, password prompts), GUI/file/browser launchers, and non-persistent `cd`/`export`/venv-activation — with a clear reason (HTTP `422`), and the UI flags them as you type. It's a heuristic on the first token, so **long-running servers/watchers are *not* blocked** (use the Projects tab for those). Pass `--no-command-guard` to disable the guard and allow everything.

None of this applies to ordinary non-interactive, log-producing work (builds, tests, scripts, git with flags).

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
| DELETE | `/api/sessions` | ✓ | Clear saved command history (running sessions untouched) |
| GET | `/api/sessions/{id}` | ✓ | Get session detail (stdout, stderr, exit code) |
| GET | `/api/sessions/{id}/stream` | ✓ | SSE real-time output stream |
| GET | `/api/config` | ✓ | Get exec timeout and max output |
| PUT | `/api/config` | ✓ | Update exec timeout and max output |
| GET | `/api/projects` | ✓ | List registered projects. Per running project: `running_url` (app-reported, informational), `direct_url` (present only when socket-verified reachable on rover's interface), `proxy_url`, `kind` (`web`/`tcp`/`task`). The response carries an `X-Rover-Local-Viewer: 1\|0` header telling the client whether this request came from the rover host itself |
| POST | `/api/projects` | ✓ | Add a project (validates by starting it). `port: 0` or omitted registers a port-less **task** |
| DELETE | `/api/projects/{name}` | ✓ | Remove a project from the registry |
| GET | `/api/projects/dirs` | ✓ | List available unregistered directories |
| GET | `/api/projects/{name}/files` | ✓ | List eligible start files in a directory |
| POST | `/api/projects/{name}/start` | ✓ | Start a project; `409` + occupant identity on port conflict; accepts `{"kill_occupant":true,"confirm_pid":N}` |
| POST | `/api/projects/{name}/stop` | ✓ | Stop a running project (detaches from adopted ones) |
| POST | `/api/projects/{name}/adopt` | ✓ | Adopt a process already listening on the project's port |
| GET | `/api/projects/{name}/stream` | ✓ | SSE live console output (`ready`, `exit`, `proxy` lifecycle events) |
| PUT | `/api/projects/{name}/proxy` | ✓ | Toggle reverse proxy on/off for a project (applies on next start) |
| GET | `/api/proxy-cookie` | ✓ | Set the signed proxy-auth cookie for this browser |
| | `http://<rover-addr>:<proxy-port>/` | * | Per-project reverse-proxy listener on a stable dedicated port; gated by the rover cookie when `--proxy-auth` is on |

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

### What the rover password protects — and what it doesn't

The secret gates **control**: every `/api/*` endpoint (exec, project start/stop/add,
config). It does **not**, by itself, gate the **data** your proxied apps serve — each
proxied project listens on its own port. Two mechanisms cover that gap:

1. **Proxies bind rover's own interface** (the host part of `--addr`), so a proxy is
   never reachable from a network rover itself is not.
2. **`--proxy-auth`** additionally requires the rover login (via a signed HttpOnly
   cookie) before a proxy forwards anything. `auto` (the default) resolves to **off**
   on loopback and Tailscale (100.64.0.0/10) binds — there the network layer already
   authenticates devices — and **on** for any other bind (LAN, `0.0.0.0`). Unauthenticated
   browsers are redirected to rover's login once per 24 h; API clients can call
   `GET /api/proxy-cookie`.

### Deployment shapes

| Bind (`--addr`) | Who can reach rover & proxies | Guidance |
|---|---|---|
| `127.0.0.1:2278` | this machine only | secret optional, proxy auth pointless (auto=off) |
| `<tailscale-ip>:2278` | your tailnet devices | **recommended for phone→PC**; secret required, proxy auth auto=off (WireGuard device auth is the boundary) |
| `:2278` / LAN IP | everyone on every network you join | secret required; proxy auth auto=**on**; rover warns loudly if you force it off |

### Other rules that always hold

- Rover **refuses to start** without a secret when bound off-loopback.
- The `--allow` allowlist and the command guard apply to exec commands **and** project
  start commands (enforced when a project is added or its command edited).
- An occupied port is **never freed by a silent kill** — not at project start (the UI
  asks, with the occupant's PID/name, and the kill is only executed if the PID still
  matches) and not at rover's own startup (fails with the occupant's identity unless
  you pass `--takeover-port`).
- A project's URL is only advertised — and its proxy only starts forwarding — after
  rover has confirmed something is listening on the port; the proxy is torn down the
  moment the tracked process exits, so it can never forward tailnet traffic to a
  stranger process that later grabs the port.
- A **direct** (non-proxy) link is advertised only after rover verified the app's
  socket on its own interface by dialing it — and **never while `--proxy-auth` is
  on**: the UI will not present an unauthenticated path around a gate you enabled.
- Enable TLS if traffic crosses an untrusted network; rotate the secret periodically
  (existing tokens and proxy cookies become invalid immediately).

---

## Development

```sh
# Run tests (the launcher suite spins up real helper HTTP servers)
go test ./... -v -count=1 -timeout 180s

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
3. Select a start file (`.py`, `.sh`, `.bat`, `.js`, `.ts`, `.go`, etc.) and a port —
   or **leave the port empty** to register a port-less **task** (a worker or script
   with no web UI; see [Project kinds](#project-kinds-web-tcp-task))
4. Click **"Validate & Add"** — Rover launches the command (with the port passed as
   `--port <n>`, a `{port}` placeholder substitution, **and** the `PORT` env var) and
   **probes the port until something actually listens** (TCP connect, then one HTTP
   request — any response, even a 4xx, proves it speaks HTTP; a listener that never
   answers HTTP is classified `tcp`). Timeout: `--validation-timeout`, default 30 s.
   Port-less tasks are instead given a short grace run and fail registration only if
   they die non-zero right away.
5. On success the project is saved to `projects_registry.json`. On failure you get the
   real reason: the app's exit code and an output tail, or "nothing listened on port N".

The registry file is personal and git-ignored — each installation has its own.

The same probe runs on **every start**: the status pill only turns *Running* once the
listener is confirmed (until then it is *Starting*), a crash shows *Failed (exit N)*,
and if the registered port is occupied you choose between **Adopt** (attach to the
process already serving there), **Kill & start** (executed only after you confirm the
exact PID), or a one-off alternative port.

Each project in the registry includes a `proxy_enabled` field (default `true`). When enabled, Rover allocates a dedicated listener that reverse-proxies to the project's local port. **The proxy binds to the same interface Rover itself listens on** (the host portion of `--addr`), so a proxied app is never reachable from a network Rover is not — bind Rover to your Tailscale IP and the proxies follow. The proxy port is **allocated once and persisted** (`proxy_port` in the registry), so the proxy URL survives restarts and can be bookmarked on your phone. The proxy only forwards while the tracked process is alive, and can require the rover login (`--proxy-auth`). The proxy URL is shown in the dashboard next to the running project, with the hostname rewritten to whatever host your browser used to reach rover — so the link works from the tailnet, the LAN, or localhost alike.

**Supported extensions:** `.py` `.sh` `.bat` `.ps1` `.js` `.ts` `.go` `.rb` `.php` `.pl` `.lua`

---

## Dashboard links — which URL opens what, from where

Every link in rover's UI is a **verified claim, never a guess**. A running project
card can show up to three address elements:

| Element | Looks like | When it appears | Where it works |
|---|---|---|---|
| **Proxy link** | ⎐ `http://<rover-host>:<proxy-port>` (green) | project is proxy-enabled and speaks HTTP | **anywhere rover itself is reachable** — phone, laptop, any device. This is the link to bookmark on your phone. |
| **Direct link** | ↗ `http://<rover-host>:<app-port>` (blue) | only after rover **dialed the app's socket on its own interface and got a connection** — i.e. the app genuinely binds `0.0.0.0` or rover's interface, not just loopback. Never shown while `--proxy-auth` is on (it would silently bypass the login gate). | any device that can reach rover's interface |
| **Local address** | ⌂ `http://127.0.0.1:<port>` | the app binds (or reports) loopback only | **clickable only when your browser runs on the rover host itself.** On any other device it renders as inert grey text marked *(host only)* — tapping it there would hit *that device's own* loopback and fail. |

**How rover tells the host apart from your phone:** not by the URL — the host
machine and a phone may both open the dashboard via the same address (e.g. a
VPN/tailnet IP). Rover checks the **TCP source address of each request**: a browser
on the host connects from the machine's own IP or loopback, every other device
connects from its own address. (`X-Forwarded-For` is honored only on loopback
connections — for setups like `tailscale serve` — so a remote client can't spoof
host status.)

Clickable proxy/direct links are additionally rewritten to the hostname **your
browser used to reach rover**, so the same link works from localhost, the LAN, or a
tailnet without per-network configuration.

**Which link should you click?**

- **Phone / any remote device** → the green **proxy** link.
- **Browser on the rover host** → the ⌂ **local** link is the shortest path (browser
  → app directly, no proxy hop); the proxy link works there too.
- **Performance:** the proxy adds one same-machine loopback hop — well under a
  millisecond, with streamed bodies and WebSocket/SSE support. Real-world latency is
  dominated by your network path, not the proxy.

---

## Project kinds: web, tcp, task

The validation probe classifies every project, and both the UI and the proxy adapt:

| Kind | How it's detected | What you get |
|---|---|---|
| `web` | listens on its port **and answers an HTTP request** (any status code counts) | status pill, links, reverse proxy |
| `tcp` | listens on its port but never answers HTTP (a database, a game server, a custom protocol) | a **"TCP · no proxy"** chip instead of a proxy link — rover's reverse proxy is HTTP-only and could only return errors for it |
| `task` | registered **without a port** (leave the port field empty) | a console-only card: start/stop, live log streaming, exit status. No URL chrome at all. |

Details worth knowing:

- `web` is **sticky**: one failed HTTP check during a slow boot does not take the
  proxy down for a project that already proved it speaks HTTP. If a `tcp` project
  later answers HTTP, it is upgraded to `web` and the registry updated.
- Projects registered **before kinds existed** (no `kind` field in the registry)
  keep the old behavior — always proxied — so upgrading rover changes nothing for
  an existing installation.
- Two port-less tasks never conflict with each other; port uniqueness is only
  enforced between real ports.

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
A: On **graceful shutdown** (Ctrl+C), Rover kills all child processes via `taskkill /F /T` (Windows) or `kill -9` (Unix); adopted projects are detached, not killed. On **forced kill** (`SIGKILL`, `Stop-Process -Force`, taskkill without `/T`), child processes become orphans — always use graceful shutdown when possible. An orphaned server can be re-attached after restart via **Adopt**.

**Q: The project's port is already in use — what happens?**  
A: Rover never kills the occupant silently. It identifies the listener (PID, process name, command line) and the UI offers three explicit choices: **Adopt** it (attach rover's tracking and proxy to the running process), **Kill & start** (the kill only executes if the PID you confirmed still holds the port), or a one-off alternative port.

**Q: Can I run interactive commands (REPLs, editors, password prompts)?**  
A: No — and rover **blocks them by default** with an HTTP `422` and a reason, since they'd just hang (no stdin/TTY). Use non-interactive forms (e.g. `git commit -m`, `npm init -y`). To override the guard entirely, start rover with `--no-command-guard`.

**Q: Why didn't my `cd` (or `export` / venv activate) affect the next command?**  
A: Every command runs in a fresh shell. Combine them in one command with `&&`, e.g. `cd app && npm test`.

**Q: I ran a command that opens a browser/app and nothing appeared in my browser.**  
A: It opened on the machine running rover, not on your device — GUI launches aren't useful over rover. See [How commands run](#how-commands-run-and-their-limits).

**Q: Which project link should I open — there are two?**  
A: On a phone or any remote device, the green **⎐ proxy** link (bookmarkable — its port is stable across restarts). In a browser on the rover host itself, the **⌂ local** link opens the app directly with no proxy hop; the proxy link works there too. See [Dashboard links](#dashboard-links--which-url-opens-what-from-where).

**Q: Why is the app's `127.0.0.1` address grey text instead of a link on my phone?**  
A: Because it would not work there: `127.0.0.1` on your phone is the phone itself. Rover shows it as information and makes it clickable only for browsers it verified are running on the rover host (by the request's TCP source address). Use the proxy link from other devices.

**Q: Does the reverse proxy slow my apps down?**  
A: Not noticeably. The extra hop is a same-machine loopback forward (sub-millisecond); bodies are streamed, and SSE/WebSockets pass through. Your Wi-Fi/VPN latency dwarfs it. On the rover host you can skip it entirely via the local link.

**Q: Can I register something that isn't a web server — a worker, a script?**  
A: Yes — leave the port field empty when adding it. It becomes a **task**: rover starts/stops it and streams its console, with no port probing and no links. See [Project kinds](#project-kinds-web-tcp-task).

**Q: My app listens on a port but rover shows "TCP · no proxy".**  
A: The validation probe connected to the port but got no HTTP response, so the app was classified `tcp` (e.g. a database or custom protocol). Rover's reverse proxy is HTTP-only, so it honestly declines to offer a proxy link. If the app actually is an HTTP server that was just slow to answer during validation, start it once more — the moment a start-time probe sees HTTP it is upgraded to `web` permanently.
