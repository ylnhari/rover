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
- `PUT /api/projects/{name}` and an **Edit Port** action to change a project's default
  port at any time.
- `port_in_use` (HTTP 409) response on start so clients can offer an override.

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
