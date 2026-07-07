# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).
Rover uses [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Security
- **The direct link is now a verified fact, and never an auth bypass.** The blue
  "direct" project link used to be inferred from the app's log banner (with loopback
  hostnames rewritten to a guessed local IP) or fall back to a raw `127.0.0.1` URL —
  misleading or dead for remote viewers. Rover now *dials the app's socket on its own
  interface* after the readiness probe and only advertises `direct_url` when the
  connection succeeds; scraped banner URLs are kept verbatim as information. While
  `--proxy-auth` is on, no direct link is ever advertised (it would silently bypass
  the login gate the operator enabled).

- **No more silent kills on occupied ports.** Starting a project on a busy port used to
  `kill -9` whatever held it (matching even mere clients via `lsof` without a LISTEN
  filter); rover's own startup did the same to its port. Both now identify the
  *listening* process (PID, name, command line) and refuse: project start returns
  `409` with the occupant so the UI can offer **Adopt / confirmed Kill / another port**
  (the kill is executed only if the confirmed PID still holds the port and is never
  rover itself); rover startup fails with the occupant named unless `--takeover-port`.
- **Proxy auth (`--proxy-auth auto|on|off`).** Project reverse proxies — previously
  always unauthenticated — can now require the rover login via a signed HttpOnly
  cookie (set at login; unauthenticated browsers are redirected to rover to log in
  once per 24 h). `auto` (default) keeps proxies open on loopback and Tailscale
  (100.64.0.0/10) binds, where the network layer is the auth boundary, and turns the
  gate on for LAN/all-interfaces binds; rover warns loudly when proxies are exposed
  unauthenticated off-tailnet.
- **Proxies stop forwarding when the app dies.** The proxy is torn down on child exit
  and refuses requests for a dead target, so it can never forward traffic to a
  stranger process that later grabs the port.
- **`--allow` and the command guard now cover project start commands** (enforced at
  registration and when a command is edited), not just `/api/exec`.
- **Project name validation** in `AddProject` (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, no
  path separators) closes a path-traversal hole in the launch working directory; the
  registry is now written atomically with `0600` permissions.
- **Secret is now required unless bound to loopback.** Rover previously ran in an
  unauthenticated "secret-less mode" while still binding all interfaces; it now refuses
  to start without `--secret`/`$ROVER_SECRET` unless `--addr` is a loopback host. This
  closes silent unauthenticated remote command execution on shared networks.
- **Project reverse proxies now bind to Rover's own interface** instead of `0.0.0.0`.
  A proxied app can no longer be reached from a network Rover itself is not exposed to;
  bind Rover to your Tailscale IP and the proxies are private to your tailnet.

### Changed
- **Real server validation.** Registering a project no longer greps stdout for a URL
  for 15 s: rover launches the command and probes the port (TCP connect until it
  listens, then one HTTP request) with a configurable `--validation-timeout`
  (default 30 s). Failures report the app's exit code and an output tail. The same
  probe runs at every start: the UI shows *Starting* until the listener is confirmed,
  the URL is only advertised after confirmation, and the proxy starts only then.
- **Truthful lifecycle.** Launched children are reaped (`Wait`) on exit — no more
  zombies; exit codes are recorded and exposed (`last_exit` in `/api/projects`, an
  `exit` SSE event); a crashed project shows **Failed (exit N)** instead of a
  permanent stale *Running*.
- **Stable proxy ports.** Each project's proxy port is allocated once, persisted in
  the registry (`proxy_port`), and reused on every start, so phone bookmarks survive
  restarts. Proxy start failures are surfaced on the project card instead of being
  logged silently.
- Project URLs shown in the UI use the host the browser actually reached rover on
  (instead of guessing a LAN IP); the port is injected as `--port`/`{port}` **and**
  the standard `PORT` env var, and a `--portfolio`-style flag no longer suppresses
  injection.
- Project console history is now a bounded buffer (512 KB with truncation marker).
- **Web UI redesign.** Refreshed the whole dark theme — cohesive palette, depth,
  type hierarchy, a centered reading column for the Terminal, status pills / chips for
  projects, a real empty state, and toast notifications. All native browser dialogs
  (`alert` / `prompt` / `confirm`) are replaced with in-app toasts and modals
  (Edit Port, Edit Command, Remove-project confirm, port-conflict resolution). No new
  dependencies or build step; still a single embedded document.
- **Terminal history restores command output after a reload.** The lightweight
  `/api/sessions` list omits stdout/stderr; the UI now lazily fetches each completed
  session's detail so its output reappears instead of showing only the exit code.
- **Projects now use an explicit, registered port instead of self-discovery.** When
  adding a project you supply the port; rover stores it and passes it to the app at
  launch as `--port <port>` (or substitutes a `{port}` placeholder in the start command).

### Fixed
- Registry saves retry the atomic rename briefly: on Windows a virus scanner
  holding a just-written file could make back-to-back saves (kind, then proxy port)
  silently drop an update.

### Added
- **Viewer-aware local links.** The dashboard knows whether *your* browser runs on
  the rover host (decided per request from the TCP source address vs the machine's
  own interfaces; `X-Forwarded-For` honored only from loopback so it can't be
  spoofed): on the host, a loopback-only app's `⌂ 127.0.0.1:<port>` renders
  clickable; on a phone or another machine the same address renders as inert
  "(host only)" text instead of a dead link. Exposed as the `X-Rover-Local-Viewer`
  header on `GET /api/projects`.
- **Project kinds (`web` / `tcp` / `task`).** The validation probe's HTTP
  classification is persisted as `kind`: HTTP servers behave as before; a listener
  that never answers HTTP is `tcp` and gets a "TCP · no proxy" chip instead of a
  proxy link that could only fail (rover's proxy is HTTP-only); `web` is sticky so
  one failed HTTP check during a slow boot doesn't take the proxy down, and upgrades
  are persisted. Registry entries from before kinds existed keep the old
  always-proxy behavior.
- **Port-less task projects.** Leaving the port empty when adding a project
  registers a worker/script: validated by a short grace run (registration fails only
  on an immediate non-zero exit), console-only card, no port probing, no proxy, and
  no port-uniqueness conflicts between tasks.
- **Adopt** (`POST /api/projects/{name}/adopt`): attach rover's tracking and reverse
  proxy to a process already listening on the project's port (started manually or
  orphaned by a previous rover run). Stop on an adopted project detaches without
  killing.
- `GET /api/proxy-cookie` to mint the proxy-auth cookie for the current browser;
  `?next=` login redirect flow from gated proxies back to the app.
- `DELETE /api/sessions` and a confirm dialog on "clear history": deletes saved
  command history from disk, leaving running sessions untouched.
- `docs/PRODUCTION_AUDIT.md`: the audit that drove all of the above.
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
  When enabled, Rover allocates a dedicated port as a reverse-proxy to the project's
  local port, avoiding path-prefix conflicts. This allows apps bound to `127.0.0.1`
  (the secure default) to be accessed from Tailscale/LAN without per-project
  `--host 0.0.0.0` configuration. Toggle on/off per project via
  `PUT /api/projects/{name}/proxy` or the Projects tab in the web UI.
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
