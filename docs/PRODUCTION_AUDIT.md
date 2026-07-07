# Rover Production Audit — Project Launcher, Validation, Ports, Proxy & Security

**Date:** 2026-07-07
**Audited commit:** `eddbbd4` (Redesign web UI and restore terminal output after reload)
**Scope:** Everything involved in "register a program → validate it's a server → start it on the right port → reach it from a phone over Tailscale/LAN → do all of this safely."

This document is (1) a fact-checked description of what the code does today, (2) a
ranked list of bugs/risks found, (3) answers to the open design questions
(does the proxy need its own auth? what does the UI actually show?), and
(4) a concrete design + roadmap to make this production-grade.
All claims cite `file:line` at the audited commit.

---

## 1. Current behavior — verified facts

### 1.1 Single point of entry

Rover is already a single point of entry in the product sense: one binary, one
listen address (`--addr`, default `:2278`), one UI, and all project lifecycle
operations go through authenticated endpoints registered in
`internal/server/server.go:538-547`:

| Endpoint | Purpose |
|---|---|
| `POST /api/projects` | register a project (`name`, `start_cmd`, `port`) |
| `PUT /api/projects/{name}` | change port and/or start command |
| `POST /api/projects/{name}/start` | start (optional one-off `port` override) |
| `POST /api/projects/{name}/stop` | stop |
| `GET /api/projects/{name}/stream` | SSE console stream |
| `PUT /api/projects/{name}/proxy` | toggle reverse proxy |

It is **not** a single point of entry in the *network* sense: every started
project with proxy enabled gets its **own extra listener on a random port**
(`startProxy`, `internal/launcher/launcher.go:183-204`). So "one port to
firewall" is not true today — see §3 and §5.

### 1.2 What can be registered

Anything. `start_cmd` is a free-form string executed via `sh -c` (Windows:
`cmd /C`) with `cwd = <projectsRoot>/<name>` (`launcher.go:286-294`,
`launcher.go:723`). The file picker (`ListEligibleFiles`, `launcher.go:593`)
only *suggests* commands for `.py/.sh/.js/.ts/.go/.rb/.php/.pl/.lua/.bat/.ps1`
files; the stored command is arbitrary.

The `--allow` allowlist and the interactive-command guard apply **only** to
`/api/exec` sessions (`server.go:674-693`) — project start commands bypass
both. (Not necessarily wrong, but it must be a deliberate, documented choice;
today it is an accident.)

### 1.3 "Is this actually a server?" validation

- **At registration** (`AddProject` → `ValidateProject`,
  `launcher.go:640-712`): rover launches the composed command for up to
  **15 seconds** and scans stdout/stderr for the regex `https?://\S+`. First
  match wins → registration succeeds. No match in 15 s → error
  `"validation timed out: no URL detected in 15 seconds"`.
- **At start** (`Start`, `launcher.go:230`): **no server validation at all.**
  Only a port-availability check, then spawn. The URL is *assumed* to be
  `http://127.0.0.1:<port>` (`launcher.go:280-282`) and later overwritten by
  any URL-looking string the process prints (`captureOutput`,
  `launcher.go:498-508`).

**This is a log-scraping heuristic, not a server check.** Consequences:

- A program that merely prints `http://anything` passes registration
  (false positive).
- A real HTTP server that doesn't log its URL (very common: gunicorn with
  `--log-level warning`, most Go services, anything logging to a file) fails
  registration (false negative).
- A server that takes >15 s to boot (big Node/Vite apps, model-loading Python
  apps) always fails registration.
- Nothing ever verifies that something is *listening* on the port, before or
  after the proxy starts forwarding to it.

### 1.4 Port injection into the command

`composeStartCmd` (`launcher.go:36-47`): substitutes `{port}` if present;
otherwise appends ` --port <n>` **unless** the string already contains the
substring `--port`. Problems:

- Servers configured via `-p`, `PORT=` env var, or positional args get a
  bogus trailing `--port 8080` (many programs crash on unknown flags; others
  silently ignore it and start on their *own* default port — rover then
  proxies to the wrong/dead port).
- Substring match means `--portfolio-mode` suppresses injection.
- The port is **not exported as `PORT` env var**, which is the de-facto
  standard most frameworks honor.

### 1.5 "Already started / port occupied" handling — the dangerous part

At `Start` (`launcher.go:256-276`):

1. `portAvailable` tries to bind `127.0.0.1:<port>` (`launcher.go:50-57`).
2. If occupied and rover has no tracked process for this project name, it
   assumes the occupant is an orphan and **kills it without confirmation**:
   `lsof -ti :<port>` → `kill -9` on Unix; `netstat`/`taskkill /F` on Windows
   (`killProcessOnPort`, `launcher.go:904-952`).
3. If the kill fails, or the project is already tracked, `Start` returns
   `ErrPortInUse`; the HTTP layer replies `409 {port_in_use:true}`
   (`server.go:859-868`) and the UI prompts for a one-off alternative port
   (`server.go` embedded JS, `startProject`).

Verified problems:

- **P0 — indiscriminate kill.** `lsof -ti :<port>` matches *any* process with
  a socket on that port — including **clients merely connected to it** (no
  `-sTCP:LISTEN` filter) and completely unrelated services you started by
  hand. Clicking *Start* can `kill -9` your database, another user's process
  (kill fails, but only after trying), or a browser holding a connection.
- **Same behavior at rover's own startup**: `ensurePortFree`
  (`cmd/serve.go:125-147`) kill -9s whatever holds rover's port before
  binding. Starting rover can silently murder an unrelated service on 2278.
- **TOCTOU race**: the port can be taken between the `portAvailable` check
  and the app's actual bind; the app then fails to bind but rover still
  reports 202 "starting" and shows *Running*.
- `portAvailable` only tests `127.0.0.1`. A server bound solely to a
  specific external interface (e.g. only the Tailscale IP) is invisible to
  the check *and* unreachable by the proxy (which targets `127.0.0.1`,
  `launcher.go:193`).
- **No "adopt" option.** If the very app you registered is already running
  (started manually), rover cannot recognize it or attach to it — it can only
  kill it or fail.
- Registration (`AddProject`) never checks port availability at all, so
  `ValidateProject` may launch the app against an occupied port → the app
  fails to bind → confusing "no URL detected" error (or worse, the app prints
  its URL *before* failing to bind and validation falsely passes).

### 1.6 Process lifecycle — confirmed bugs

- **P0 — crashed projects show "Running" forever.** Nothing removes
  `m.procs[name]` when the child exits on its own; only `Stop()` deletes it
  (`launcher.go:356-364`). After a crash, `ListRunning`/`GetRunning` still
  report the project, `/api/projects` returns `is_running: true`, the UI
  renders a green *Running* pill on reload, `Start` returns "already
  running", and the only way out is clicking *Stop* on a dead process.
  (Clients streaming at crash time do get a `done` event, but reconnecting
  or reloading clients never learn the process died.)
- **P0 — zombie processes.** `cmd.Wait()` is never called for launched
  projects (only `wg.Wait()` on the output scanners, `launcher.go:524`).
  Every exited project leaves a `<defunct>` zombie until rover itself exits.
  `Stop` also kills the process group without reaping.
- Exit code / failure reason is never captured or exposed anywhere.
- On rover restart, the `procs` map is gone → previously launched projects
  become untracked orphans (still running, still holding their ports) — which
  is precisely what feeds the kill-on-port heuristic above. There is no
  pidfile/registry of running children and no re-adoption on startup.
- `StopAll` on shutdown (`server.go:580-583`) kills all launched projects —
  reasonable, but combined with the above it means "restart rover" =
  "restart every app" and any crash of rover orphans them all.

### 1.7 URL / port auto-detection quality

- Regex `https?://\S+` over every output line; in `captureOutput` the **last**
  URL printed wins and keeps overwriting `info.URL` (`launcher.go:498-508`) —
  any URL in a log line (an API call target, a docs link, an error mentioning
  a URL) replaces the real one.
- Port re-parse from the URL uses `strings.LastIndex(url, ":")` then `Atoi`
  of the remainder (`launcher.go:503-507`): fails for any URL with a path or
  trailing slash (`http://127.0.0.1:8000/docs` → `Atoi("8000/docs")` fails →
  port silently not updated). Same flaw in `ValidateProject`
  (`launcher.go:702-707`).
- `replaceLoopback` (`launcher.go:149-155`) rewrites
  `localhost/127.0.0.1/0.0.0.0` in the *displayed* URL to the first
  non-loopback IPv4 (`localIP`, `launcher.go:121-132`). Two problems:
  - If the app is bound to `127.0.0.1` only, the rewritten
    `http://<lan-ip>:<port>` link shown in the UI is **unreachable** — the
    reachable link is the proxy URL. The UI shows both, one of which is a lie.
  - `localIP` returns the first non-loopback IPv4, which on machines with
    Docker/VPN/virtual adapters is frequently the wrong interface
    (e.g. `172.17.0.1`), and on a phone connected via Tailscale the LAN IP is
    unreachable anyway. The correct host for links is *the host the browser
    already used to reach rover* (`window.location.hostname` /
    `r.Host`) — rover never uses it.

### 1.8 The reverse proxy & mobile reachability

- Created only inside `Start()` for registered projects with
  `proxy_enabled: true` — rover does **not** (and cannot today) proxy servers
  it didn't launch.
- `proxy_enabled` defaults to **true** for new projects (`launcher.go:753`)
  and a registry migration sets it true for old entries missing the field
  (`launcher.go:860-880`).
- The proxy binds to the same interface rover listens on
  (`SetBindHost`/`startProxy`, `launcher.go:179-191`) — good containment
  design: bind rover to the Tailscale IP and proxies are tailnet-only; bind
  rover to `127.0.0.1` and proxies are loopback-only. **But** the default
  `--addr :2278` has an empty host → proxies bind **all interfaces**.
- The proxy targets `http://127.0.0.1:<port>` (`launcher.go:193`), so it
  works whether the app binds `0.0.0.0` or `127.0.0.1` (both are reachable
  via loopback). It does *not* work for an app bound solely to a non-loopback
  interface.
- **Random port every start** (`net.Listen(..., "0")`) — mobile bookmarks and
  home-screen shortcuts break on every restart.
- **Proxy start failure is non-fatal and invisible**: logged only
  (`launcher.go:343-345`); the API/UI simply omit `proxy_url` with no error
  surfaced.
- The proxy starts forwarding immediately, with **no readiness check** on the
  target and no health monitoring: if the app is still booting, died, or a
  *different* process later grabs the port, the proxy serves 502s or —
  worse — **forwards tailnet traffic to whatever stranger process now owns
  the port**.
- No `X-Forwarded-For`/`X-Forwarded-Proto` handling beyond
  `httputil.NewSingleHostReverseProxy` defaults; no WebSocket-specific config
  (Go's ReverseProxy does pass through Upgrade, so plain WS works).

### 1.9 Security posture (verified)

Good and already in place:

- HMAC-SHA256 stateless login tokens, 24 h TTL, constant-time comparisons
  (`internal/auth/auth.go`); raw secret only sent at login.
- Secret **required** when bound off-loopback; secret-less mode refuses to
  start unless bound to loopback (`cmd/serve.go:47-58`); empty host (`:2278`)
  correctly counts as non-loopback (`isLoopbackBind`, `cmd/serve.go:112-123`).
- Login rate-limit 10/min/IP (`server.go:37,54-68`); security headers + CSP
  (`server.go:492-499`); optional TLS ≥1.2; SSE write deadlines correctly
  disabled per-stream via `http.NewResponseController` while keeping the
  global 30 s `WriteTimeout` for normal requests.
- Auth via custom header `X-Rover-Secret` is inherently CSRF-resistant; no
  CORS headers are set, so browsers can't read cross-origin responses.

Gaps found:

- **P0 — proxy listeners are completely unauthenticated**
  (`startProxy` wraps a bare `httputil.NewSingleHostReverseProxy`; no
  middleware). Rover's password protects the rover API only. See §3 for the
  design decision.
- **P1 — path traversal in project `name`.** `AddProject` does
  `dir = filepath.Join(m.projectsRoot, name)` (`launcher.go:723`) with no
  validation — `"name": "../../somewhere"` escapes the projects root and the
  command runs with that cwd. (An authed caller has arbitrary exec anyway via
  `start_cmd`, so this is containment/hygiene, not a new capability — but a
  production product validates names: `[A-Za-z0-9._-]+`, no separators, no
  leading dot.) `ListEligibleFiles` *does* do this containment check
  correctly (`launcher.go:594-598`) — `AddProject` should match it.
- **P1 — token in query string for SSE** (`?secret=<token>`,
  `server.go:467-472` + UI): necessary for `EventSource`, but tokens can leak
  into intermediary logs; acceptable on a tailnet, should move to
  cookie-based auth for SSE in production hardening.
- **P2** — registry written world-readable `0644` next to the executable
  (`launcher.go:893`; sessions file is `0600` — inconsistent). Registry also
  silently resets to empty on unreadable/corrupt JSON (`launcher.go:848-854`)
  and the next successful `AddProject`/`RemoveProject` **overwrites the file**
  → total registry loss from one bad write. No backup, no versioning, no
  atomic write (`os.WriteFile` can tear on crash).
- **P2** — `killProcessOnPort` on Windows parses `netstat` with a
  `Contains(":"+port+" ")` match that can hit the *foreign* address column.

### 1.10 What the UI actually shows (per project card)

From `renderProjects` + `/api/projects` (`server.go:810-845` and embedded JS):

| UI element | Source | Accuracy issues |
|---|---|---|
| Status pill *Running/Stopped* (+transient *Starting*) | `is_running` = presence in `procs` map | **Wrong after a crash** (stays Running, §1.6). *Failed* style exists in CSS but is never derivable from the API — there is no failed/exit-code state. |
| `Port <b>N</b>` chip | registered port, overwritten by runtime-detected port when running | Runtime detection silently fails for URLs with paths (§1.7), so it usually just shows the registered port; can show a *detected* port that differs from where the proxy actually points. |
| `↗ running_url` link | assumed `http://127.0.0.1:<port>`, replaced by last URL scraped from logs, loopback rewritten to first LAN IP | Frequently unreachable from the phone (loopback-bound app, wrong interface chosen, §1.7); misleading next to the proxy link. |
| `⎐ proxy_url` link | `http://<proxyURLHost>:<random-port>` | Host may be the wrong interface (LAN IP while you're on Tailscale, §1.7); port changes every start; absent (silently) if proxy failed to start. |
| *Proxy ON/OFF* button | registry `proxy_enabled` | Toggling while running does not start/stop the live proxy — takes effect next start; UI doesn't say so. |
| Start command, description | registry | `description` is hardcoded `"Active"` at registration (`launcher.go:752`) — meaningless value shown to the user. |
| Console output | SSE stream, replayed from an unbounded in-memory buffer | `rp.output` grows without limit for chatty servers (`launcher.go:86,484-486`) — memory leak over long runs; exec sessions have `--max-output`, projects have nothing. |
| Start/Stop buttons | `is_running` | Start on a crashed-but-tracked project → "already running" error toast. |

Other UI-visible behaviors: after `Start` the UI optimistically flips to
*Running* on HTTP 202 without any readiness signal; `determineStatus` only
distinguishes running/stopped; the 409 port-in-use prompt is the only
conflict surface (it never tells you *what* is occupying the port, and the
silent kill in §1.5 happens *before* you'd ever see it).

---

## 2. Ranked findings summary

**P0 (must fix before calling it production):**
1. Unauthenticated proxy listeners expose every proxied app to whatever
   network rover is bound to (default: all interfaces). §1.8/§3
2. Silent `kill -9` of arbitrary port occupants at project start and at rover
   startup; lsof matching non-listeners. §1.5
3. Crashed projects permanently shown/treated as running; no exit status;
   zombie processes (no `Wait`). §1.6
4. No real "is it a server" validation at start; proxy forwards blind,
   including to stranger processes that reuse the port. §1.3/§1.8

**P1:**
5. Port injection heuristic breaks non-`--port` servers; no `PORT` env. §1.4
6. Registration validation = 15 s log-regex; false positives/negatives. §1.3
7. Project-name path traversal in `AddProject`. §1.9
8. Random proxy ports break mobile bookmarks; proxy failure invisible. §1.8
9. Misleading URLs in UI (loopback rewrite to wrong interface; last-URL-wins
   scraping; port parse fails on paths). §1.7/§1.10
10. No orphan re-adoption after rover restart; restart = mass kill. §1.6
11. `--allow`/command-guard don't apply to project commands (undocumented). §1.2

**P2:**
12. Registry: non-atomic writes, silent reset on corruption, `0644`, lives
    next to the binary. §1.9
13. Unbounded project console buffer. §1.10
14. Hardcoded `"Active"` description; *failed* pill never reachable;
    proxy toggle while running is a silent no-op. §1.10
15. SSE token in query string. §1.9
16. `AddProject` doesn't pre-check port availability before validation. §1.5

---

## 3. Design question: rover has a password — do the proxies need their own auth?

**Threat model:** the proxy port is reachable by anyone who can reach the
interface it binds. Three deployment shapes:

1. **Rover bound to `127.0.0.1`** — proxies are loopback-only. Auth adds
   nothing. (Also mostly pointless: the apps are already local.)
2. **Rover bound to a Tailscale IP** — reachable only by devices in your
   tailnet. Tailscale itself authenticates devices (WireGuard keys), so the
   *network* is the auth boundary. For a personal tailnet, unauthenticated
   proxies are an acceptable, deliberate choice — this is exactly how
   `tailscale serve` behaves.
3. **Rover bound to `0.0.0.0` (the default `:2278`) or a LAN IP** — proxies
   are open to everyone on every network the machine joins (coffee-shop
   Wi-Fi, office LAN). Rover's password protects `/api/*` but **not** the
   apps; a dev server with an eval console or an unauthenticated admin UI is
   fully exposed. This is the dangerous default.

**Recommendation (defense in depth, not either/or):**

- Keep "proxy binds rover's interface" (already implemented) as the primary
  containment. Additionally **warn loudly at startup and in the UI when
  rover is bound to `0.0.0.0`/LAN with proxies enabled.**
- Add an optional but **default-on-off-loopback** proxy auth gate: a signed,
  HttpOnly **cookie** (same HMAC scheme as `internal/auth`) scoped to the
  host. Flow: the dashboard's ⎐ link points at an authenticated rover
  endpoint `GET /api/projects/{name}/proxy-link` which 302s to the proxy URL
  while setting the cookie (or the proxy itself redirects unauthenticated
  browsers to rover's login and back). Cost: one redirect once per 24 h per
  device; apps needing raw programmatic access can use a per-project
  `?proxy_token=` bootstrap or the operator can set `--proxy-auth=off`.
- Config: `--proxy-auth = auto | on | off`, where `auto` = *off* when the
  bind interface is loopback or a Tailscale CGNAT address (`100.64.0.0/10`),
  *on* otherwise. This directly answers "we may not need it": on your
  tailnet, `auto` keeps today's zero-friction behavior; on any other bind it
  protects you by default.
- Never proxy when the tracked child process is dead (fixes the
  port-squatter forwarding hole regardless of auth).

**Why not "no auth is fine because rover has a password":** the password
gates *control* (start/stop/exec). The proxies gate *data* (whatever the apps
serve). Different assets, different doors. On a pure tailnet they coincide;
on any other network they don't.

---

## 4. Target design — "best production version"

### 4.1 Registration (single, reliable entry point)

`POST /api/projects` becomes a **two-phase, informative flow**:

1. **Static checks:** name matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`;
   directory exists under projects root (reuse the `ListEligibleFiles`
   containment check); port in `1024–65535`; port not already assigned;
   optional `port_strategy: "flag" | "placeholder" | "env" | "none"` chosen
   by the user (default `env`+`flag`), replacing the fragile substring
   heuristic. Always export `PORT=<n>` (and `HOST=127.0.0.1` opt-in) into the
   child env regardless of strategy.
2. **Live probe (the real validation):** if the port is occupied, stop and
   report *who* occupies it (see 4.3) instead of launching. Otherwise launch
   the command and, in parallel with log scraping:
   - poll `TCP connect 127.0.0.1:<port>` until success (server is
     *listening*), then
   - issue `GET http://127.0.0.1:<port>/` — **any** HTTP response (including
     4xx/5xx) proves it speaks HTTP.
   - Configurable `validation_timeout` (default 30 s, flag/UI field), and the
     probe result is *structured*:
     `{listening: bool, http: bool, status: int, detected_url, detected_port,
     boot_ms, log_tail}`.
   - Early-exit with the child's exit code + stderr tail if it dies during
     validation — "exited with code 2: `python: No module named flask`"
     instead of "no URL detected".
   - If the process listens on a **different** port than requested (compare
     via `lsof -p <pid>`-style lookup of the child's own sockets), report the
     mismatch and offer to register the detected port.

Registration succeeds only on `listening == true` (with `http` recorded; a
raw-TCP server can still be registered with an explicit
`"type": "tcp"` acknowledgment). Everything else returns the structured
report so the UI can show *why* it failed.

### 4.2 Start / runtime

- Same probe machinery on every start: state machine
  `starting → running (listening confirmed) → failed (exit code) | stopped`.
  `POST .../start` still returns 202, but the SSE stream (and
  `/api/projects`) carries the state transitions; the UI pill only turns
  *Running* on confirmation and shows *Failed (exit 1)* with the log tail
  otherwise.
- `cmd.Wait()` in a goroutine per child: reap the process, record exit
  code/time, delete from `procs`, broadcast a terminal
  `{type:"exit", code:N}` event, and tear down that project's proxy.
- Persist running children (`pid`, `pgid`, `port`, start time, start-cmd
  hash) to a `runtime_state.json`; on rover startup, re-scan: children still
  alive and matching → **re-adopt** (reattach proxy; console history restarts
  fresh, clearly marked); dead → mark stopped. Rover restart stops being a
  mass-kill/orphan event, and `--stop-projects-on-exit=true|false` becomes an
  explicit choice (default true, as today).
- Bounded console ring buffer per project (e.g. 512 KB) with truncation
  marker, mirroring `--max-output` for exec sessions.

### 4.3 Port-conflict & "already running" policy (no more silent kills)

On any occupied target port (registration or start):

1. Identify the occupant with a **listener-only** query
   (`lsof -sTCP:LISTEN -ti :<port>` + `/proc/<pid>/{comm,cmdline}`;
   `Get-NetTCPConnection`-equivalent parsing on Windows): pid, process name,
   command line, owning user, and whether it is a rover-tracked child or a
   re-adoptable previous child (pid recorded in `runtime_state.json`).
2. Return it structured:
   `409 {port_in_use: true, occupant: {...}, options: [...]}` — never kill.
3. UI presents explicit choices, each mapping to a confirmed API action:
   - **Adopt** (occupant matches this project's recorded child or the user
     asserts it's the same app): attach proxy + tracking to the existing
     listener, no restart. Satisfies "if started, tell me what port and reuse
     it."
   - **Restart on this port** → `POST .../start {kill_occupant: true,
     confirm_pid: <pid>}` — kill is executed only with the pid echoed back
     (guards against TOCTOU killing a different process), only if it is a
     listener, never pid 1/self, and is fully audit-logged. This implements
     "restart it at the right port **after user confirmation**."
   - **Use another port** (existing one-off override flow, kept).
4. `ensurePortFree` at rover startup gets the same treatment: default becomes
   *fail with a clear message naming the occupant*; `--takeover-port` opts
   into the old kill behavior.

### 4.4 Mobile reachability (Tailscale / LAN)

- **Stable proxy ports:** allocate once per project (e.g. sequential from a
  `--proxy-port-base`, or user-set), persist in the registry, reuse on every
  start. Bookmarks survive restarts.
- **Correct link host:** build displayed URLs from the request the browser
  actually made — `window.location.hostname` (or `r.Host` server-side) — not
  from `localIP()` guessing. If you reached rover at
  `http://100.x.y.z:2278`, the proxy link is `http://100.x.y.z:<proxyport>`.
  Delete the loopback-rewrite of scraped URLs (§1.7); instead label the
  direct URL honestly: shown as reachable only when the app's bind covers
  non-loopback (detectable from the child's listen socket), otherwise shown
  as "local only — use proxy link."
- **Optional single-port mode** (true network single-entry):
  `/p/{name}/` path-mounted proxying on rover's own port for apps that
  tolerate a path prefix (with `X-Forwarded-Prefix`), falling back to
  per-port proxies for apps that don't. One firewall rule, one Tailscale
  serve target.
- Health surfaced: proxy card shows target state (listening / down); proxy
  start failure becomes a visible error on the card, not a log line.
- Works for both app bind modes by design: proxy targets `127.0.0.1`, which
  covers `0.0.0.0`-bound and `127.0.0.1`-bound apps alike (verified §1.8);
  document the one unsupported case (app bound only to a non-loopback IP)
  and detect it in the probe ("listening on 100.x only — proxy will not
  work; bind your app to 127.0.0.1 or 0.0.0.0").

### 4.5 Security hardening list

1. Proxy auth per §3 (`--proxy-auth auto|on|off`, signed cookie, redirect
   flow; tailnet `auto` = off).
2. Stop proxying on child death (kill the forwarding hole).
3. Name validation / containment in `AddProject` (§1.9).
4. Apply `--allow` prefixes (and optionally the command guard) to
   `start_cmd` at registration/update time, or add an explicit
   `--allow-projects` — either way, document the boundary.
5. Registry & runtime-state: move to a proper config dir
   (`os.UserConfigDir()/rover/`), atomic write (temp file + rename), `0600`,
   keep one `.bak`, refuse to overwrite on unparseable existing file (return
   an error naming the file instead of silently starting empty).
6. Cookie-based session auth for the UI (HttpOnly, SameSite=Strict) so SSE
   stops passing tokens in query strings; keep header auth for API clients.
7. Audit log entries for: project add/update/remove, start/stop, adopt,
   confirmed kills (pid, occupant cmdline, requesting IP), proxy toggles.
8. Startup banner + UI banner when proxies are enabled on a
   non-loopback, non-tailnet bind without proxy auth.

### 4.6 UI truth-telling (fixes for §1.10)

- Status pill driven by the real state machine incl. **Failed (exit N)**;
  crashed ≠ Running ever.
- Port chip: show registered port; add "listening on N" badge only from the
  probe, not from log-scraped URLs.
- Links: one **Open** button that always points at the *reachable* URL
  (proxy URL when proxy on / app loopback-bound; direct URL only when
  actually reachable), host taken from `window.location`. Secondary
  copy-to-clipboard for the other URL, labeled.
- Proxy toggle while running either applies live (start/stop the listener)
  or clearly says "applies on next start".
- Replace hardcoded `"Active"` description with user-supplied description
  (optional field in Add dialog).
- Add-project dialog shows the structured validation report (boot time,
  probe result, log tail) on success *and* failure.

---

## 5. Failure-scenario matrix (target behavior)

| # | Scenario | Today (verified) | Target |
|---|---|---|---|
| 1 | Command isn't a server (e.g. a script that exits) | Registers if it prints any URL; else "no URL detected" after 15 s | Probe fails with exit code + stderr tail; registration refused with reason |
| 2 | Server boots slower than timeout | Registration always fails at 15 s | Configurable timeout; TCP-listen probe usually succeeds long before logs; report `boot_ms` |
| 3 | Server ignores `--port` and starts on its own port | Silently registered/proxied against wrong port → dead proxy | Mismatch detected via child's actual listen socket; offer detected port |
| 4 | Port occupied by unrelated process | **kill -9 without asking** (start) / confusing validation failure (registration) | 409 + occupant identity + Adopt / Confirm-kill / Other-port choices |
| 5 | Same app already running (started manually or orphaned from previous rover) | Killed or "port in use" | Recognized via runtime-state / user confirmation → **adopt**, report its port |
| 6 | Port grabbed between check and bind (race) | App fails, UI shows Running | Probe fails → state *Failed*, occupant re-queried, same 409 flow |
| 7 | Project crashes while running | Shows **Running forever**; zombie process | Reaped via `Wait`; state *Failed (exit N)*; proxy torn down; optional restart policy |
| 8 | Rover restarts | All children killed on clean exit; orphans on crash; tracking lost | Runtime-state persistence → re-adopt or clean report; explicit stop-on-exit flag |
| 9 | Proxy port allocation fails | Logged only; UI silently shows no proxy link | Card shows proxy error; retry button |
| 10 | App dies, another process takes its port | Proxy forwards tailnet traffic to the stranger | Proxy stops at child exit; forwarding gated on tracked-child-alive |
| 11 | App bound only to non-loopback IP | Invisible to port check; proxy 502s | Detected in probe; clear guidance shown |
| 12 | Registry file corrupted / unwritable | Silently treated as empty; next save wipes it; or opaque save error | Refuse + name the file; atomic writes; `.bak` restore path |
| 13 | Phone on tailnet, app on `127.0.0.1` | Works *if* user clicks the proxy link; the other displayed link is dead | Single Open button, always-reachable URL from `window.location` host |
| 14 | Rover on `0.0.0.0`, hostile LAN | Proxied apps fully open, no warning | `--proxy-auth auto` gates them; explicit banner |
| 15 | Very chatty server, long uptime | Unbounded console buffer growth | Ring buffer with truncation marker |
| 16 | Two projects registered on one port | Blocked at registration (works today: `launcher.go:732-736`) | Keep; also surfaced in UI port editor |
| 17 | User clicks Start twice fast | Second gets "already running" (map guard, works today) | Keep; button also disabled optimistically |
| 18 | `secret` unset on non-loopback bind | Refuses to start (works today) | Keep |

---

## 6. Agreed work items (confirmed with the owner)

> **Implementation status (2026-07-07):** all six items below, plus the
> prerequisite P0 lifecycle fixes (reaping/`Wait`, exit codes, *Failed* state,
> bounded console buffer) and the P1 hygiene items (project-name validation,
> atomic `0600` registry writes, `PORT` env + `--port` word-boundary fix), are
> **implemented and tested** on `master`. See CHANGELOG "Unreleased" for the
> user-facing summary. Not implemented (out of committed scope): runtime-state
> persistence / re-adoption *across rover restarts* (§4.2 — manual Adopt covers
> the common case), single-port `/p/{name}/` path-mounted proxy mode (§4.4),
> cookie-based UI session auth for SSE (§4.5 item 6), and registry
> corruption-refusal/`.bak` (§4.5 item 5, partially done via atomic writes).

- [x] **1. Real HTTP validation** — actually probe `http://127.0.0.1:<port>`
  (TCP-connect poll until listening, then an HTTP request; configurable
  timeout) instead of regex-matching a URL in stdout. Applies **both** at
  registration and at runtime after start, and must pass before the URL is
  advertised or the proxy starts forwarding. [§4.1, §4.2]
- [x] **2. Safe port-conflict handling** — stop blindly `kill -9`-ing
  whatever occupies a port (project start *and* rover's own startup).
  Detect the conflict, report exactly which process holds the port (pid,
  name, cmdline, user), and require explicit user confirmation (or an
  explicit flag) before killing — plus keep the existing alternate-port
  path. [§4.3]
- [x] **3. Detect "already running elsewhere" and adopt** — if something
  already serves valid HTTP on the project's port (e.g. the same app started
  manually or orphaned by a previous rover run), tell the user what port it
  is on and offer to **adopt/proxy** it — or restart it on the right port
  after confirmation — rather than kill-and-restart by default. [§4.3, §4.2]
- [x] **4. Proxy hardening** — the unauthenticated proxy ports are the main
  exposure: add optional proxy auth (`--proxy-auth auto|on|off`, signed
  cookie reusing rover's HMAC scheme; `auto` = off on loopback/tailnet
  binds, on elsewhere), keep the per-project toggle, make proxy ports stable
  across restarts, and stop the proxy from forwarding when the tracked
  project process has died (so it never proxies a stranger process that
  later grabs the port). [§3, §4.4, §4.5]
- [x] **5. Apply the `--allow` allowlist / command guard to project
  `start_cmd` too** — not just `/api/exec`; enforce at registration and
  update time (or add an explicit `--allow-projects`), and document the
  boundary either way. [§4.5 item 4]
- [x] **6. Docs/README updates** — rewrite the security section to state
  precisely what is and isn't exposed: what the rover password protects
  (control API) vs. what it does not (proxy ports / app data), tailnet vs.
  LAN vs. loopback guidance, and the new conflict/validation semantics.
  [§3, §4.5 item 8]

Beyond these six, the P0 lifecycle-correctness fixes in §2 (crashed projects
shown as running, zombie processes, no exit codes) are prerequisites for
items 1–4 and are scheduled first in §7.

---

## 7. Suggested implementation order

1. **Lifecycle correctness** (P0, no design controversy): `cmd.Wait` +
   reaping, `procs` cleanup on exit, exit-code state, *Failed* in API/UI,
   bounded console buffer. Small, self-contained, unblocks everything else.
2. **Kill-policy rework** (P0): listener-only occupant identification,
   structured 409 with occupant info, confirmed-kill endpoint
   (`kill_occupant + confirm_pid`), adopt flow, `ensurePortFree` →
   fail-by-default + `--takeover-port`.
3. **Real validation probe** (P0/P1): TCP+HTTP probe shared by registration
   and start; structured report; `PORT` env + `port_strategy`; configurable
   timeout; use probe state to drive the UI state machine and proxy gating.
4. **Proxy hardening** (P0/P1): stop-on-death, stable ports, request-host
   URLs, visible errors, `--proxy-auth auto|on|off` with signed-cookie gate.
5. **Registry & state robustness** (P1/P2): config-dir location, atomic
   writes, corruption refusal, runtime-state persistence + re-adoption.
6. **UI truth pass + polish** (P2): single Open button, honest labels,
   descriptions, live proxy toggle semantics, validation report dialog.
7. Docs: README security section rewrite (what the password does and does
   not protect; tailnet guidance), CHANGELOG.

Each step is independently shippable and testable; 1–3 remove the actively
dangerous behaviors, 4 closes the network exposure, 5–7 are productization.

---

*Audit performed against the pushed state of `master` (`eddbbd4`). No source
files were modified; this document is the only change.*
