# Rover project instructions

> **Setup:** use `AGENTIC.local.md` (copied from its tracked example) for
> machine-local values that any host may read when configured. A native host
> may additionally use its own ignored local adapter. Personal/global
> preferences live in the shared user-level baseline.

## Role
Maintain rover: a zero-dependency, single-binary remote shell + project launcher (Go). Unlike a typical local-only app, rover is *meant* to be reachable off-loopback (e.g. over a Tailscale tailnet): its entire job is being the authenticated front door. See README's Security Considerations for the threat model.

## Context
Default branch **master**, remote `github.com/ylnhari/rover`. Machine-local details (the address the live instance binds, ports, where the secret lives) belong in the active host's gitignored local configuration, never here.

## Architecture
- **UI is ONE embedded Go raw string** — `var webUI` in `internal/server/server.go` (self-contained HTML/CSS/JS, no build step, no separate frontend files). Because it's a backtick-delimited raw string, **the UI must contain no backtick** — that also rules out JS template literals inside it.
- **Security model:** HMAC-SHA256 session tokens (24h TTL, raw secret never sent to the browser after login), `X-Rover-Secret` header auth on protected endpoints, rate-limited login (10/IP/min), command allowlist (`--allow git,go test,npm`), security headers, optional TLS. Rover refuses to start without `ROVER_SECRET` unless bound to `127.0.0.1` — secret-less mode can never reach the network.
- **Project launcher + reverse proxy:** rover can start/stop registered local projects and proxy requests to apps bound to `127.0.0.1` (making them reachable over Tailscale without changing the app's own bind). Project reverse proxies bind to rover's own `--addr` interface, so they follow rover onto the tailnet rather than each needing `0.0.0.0`. Registry: `projects_registry.json` (gitignored).
- **Sessions:** `GET /api/sessions` (list) omits stdout/stderr for weight; `GET /api/sessions/{id}` (detail) has them. Persisted to `sessions.json` (gitignored) so completed sessions survive restarts.

## Rules
1. **Never widen the bind.** Rover stays the single authenticated front door; don't add a code path that starts it on `0.0.0.0` or skips the secret check off-loopback.
2. `rover.exe`, `serve.local.ps1`, `rover.secret.env`, `*.log`, `sessions.json`, `projects_registry.json` are gitignored — never commit them or print `ROVER_SECRET`'s value.
3. **Test without touching the live secret:** run a secret-less loopback instance — `go build -o rover.exe . && ./rover.exe serve --addr 127.0.0.1:<port>` (secret-less mode is only permitted on loopback) — and drive it in a browser.
4. Keep `go test ./...`, `gofmt`, and `go vet` clean before considering a change done.

## Running
```sh
go build -o rover.exe .
./rover.exe serve --addr 127.0.0.1:2279     # local test, no secret needed
```
See `AGENTIC.local.md` or the active host's ignored native adapter for how to
restart the live tailnet instance and where `ROVER_SECRET` is stored on this
machine.
