# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).
Rover uses [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Changed
- **Projects now use an explicit, registered port instead of self-discovery.** When
  adding a project you supply the port; rover stores it and passes it to the app at
  launch as `--port <port>` (or substitutes a `{port}` placeholder in the start command).
- Starting a project **fails fast if its port is already in use** rather than launching
  blindly; the web UI then prompts for a one-off alternate port for that start only,
  leaving the registered default unchanged.

### Added
- **Command guard (on by default):** the Terminal tab now rejects commands that cannot
  work in rover's fresh, non-interactive, host-side shell — interactive editors/REPLs/
  pagers, password prompts, GUI/file/browser launchers, `git commit` without `-m`,
  `git rebase -i`, `npm init` without `-y`, and non-persistent `cd`/`export`/venv
  activation — with HTTP `422` and a reason. Long-running servers/watchers are not
  blocked. Disable with `--no-command-guard`. The UI flags these live as you type.
- `PUT /api/projects/{name}` and an **Edit Port** action to change a project's default
  port at any time.
- `port_in_use` (HTTP 409) response on start so clients can offer an override.
- **Reverse proxy for projects (default ON).** Each project has a `proxy_enabled` toggle.
  When enabled, Rover serves `GET /proxy/{name}/` which reverse-proxies to the project's
  local port. This allows apps bound to `127.0.0.1` (the secure default) to be accessed
  from Tailscale/LAN without per-project `--host 0.0.0.0` configuration. Toggle on/off
  per project via `PUT /api/projects/{name}/proxy` or the Projects tab in the web UI.
- `PUT /api/projects/{name}/proxy` endpoint to toggle per-project reverse proxy.
- `proxy_enabled` field in `ProjectInfo` (serialized to `projects_registry.json`).

## [0.1.0] — 2026-06-23

### Added
- Session-based remote command execution with real-time SSE streaming
- Project launcher: start, stop, and monitor local server projects from the browser
- Dark-theme chat-style web UI (mobile-first, no build step)
- HMAC-SHA256 signed session tokens with 24-hour TTL (stateless, no DB)
- `X-Rover-Secret` header auth for all protected endpoints
- `--allow` flag: comma-separated command prefix allowlist
- Structured logging via `log/slog` (text default, JSON with `--log-format json`)
- Session persistence to `sessions.json` (survives restarts)
- Audit log for every command execution and project lifecycle event
- TLS support via `--tls-cert` / `--tls-key`
- Configurable execution timeout (`--exec-timeout`) and output cap (`--max-output`)
- Security headers: `X-Content-Type-Options`, `X-Frame-Options`, `Content-Security-Policy`
- Rate limiting on the login endpoint (10 attempts / IP / minute)
- Path traversal protection in the project file browser
- Cross-platform: Linux (amd64/arm64), macOS (amd64/arm64), Windows (amd64/arm64)
- GitHub Actions CI: vet, race-detected tests, cross-compile matrix
- GitHub Actions release workflow: tagged releases with pre-built binaries

[Unreleased]: https://github.com/ylnhari/rover/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ylnhari/rover/releases/tag/v0.1.0
