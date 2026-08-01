# Investigation: a proxied project rejecting requests from a phone

> Status: **RESOLVED.** Root cause identified (two layers, below) and fixed in
> Rover's per-project reverse proxy (`transparentRewrite` in
> `internal/launcher/launcher.go`). Verified end-to-end against the live
> photo-vault backend. This document is kept as the design record.

## 1. Context: what Rover is and how it is used here

Rover is a zero-dependency Go remote shell + project launcher. Its job is to be a
single authenticated "front door" to apps running on the host machine, reachable
from a phone (or any remote client) without each app having to bind to `0.0.0.0`.

- Apps bind to `127.0.0.1` (loopback). Rover launches them and runs a **reverse
  proxy** per project, bound to *Rover's own* listen address (`--addr`). Because
  Rover binds to the tailnet/routeable interface, the proxy — and thus the app —
  becomes reachable from the phone over the tailnet without the app itself being
  exposed on `0.0.0.0`.
- The intended contract is **transparent, connection-method agnostic** exposure:
  a loopback app should behave for a phone-over-tailnet client exactly as it does
  for a laptop-direct client, with no per-app configuration.

## 2. Symptom (reproduction)

Project `photo-vault` (FastAPI/uvicorn on `127.0.0.1:8768`, proxied). Opened from
the laptop directly (`127.0.0.1:8768`) it works. Opened from a phone through the
proxy URL (`http://<tailnet-ip>:<proxy-port>`) it failed — first with **"Invalid
host header"**, and after that was worked around, with every tab showing
**"unauthorized"** and all counts zero/stale. Same server, same data.

## 3. Root cause — TWO independent layers

Both stem from the stock reverse proxy leaking the external client's identity to
the backend, which a security-conscious loopback app then acts on.

### Layer 1 — Host header → "Invalid host header" (400)

`httputil.NewSingleHostReverseProxy`'s default director forwards the client's
original `Host` (e.g. the tailnet IP / `*.ts.net` name) unchanged. photo-vault's
`TrustedHostMiddleware` (`src/api.py:109`) rejects any `Host` not on its allowlist
(`_BASE_HOSTS`), returning `400 "Invalid host header"`. The laptop's `Host` is
`localhost`/`127.0.0.1` (allowed); the phone's is the external host (not allowed).
This was originally worked around app-side with `PV_ALLOWED_HOSTS`.

### Layer 2 — X-Forwarded-For → token withheld → "unauthorized" (401)

This is the one `PV_ALLOWED_HOSTS` does **not** fix, and the actual cause of the
"unauthorized" screens.

- Go's `ReverseProxy` adds `X-Forwarded-For: <phone-ip>`.
- photo-vault's server is uvicorn, whose proxy-header handling is **on by default**
  and trusts forwarding headers from a loopback peer (`forwarded_allow_ips`
  defaults to `127.0.0.1`, and Rover connects from `127.0.0.1`). So uvicorn sets
  `request.client.host` to the forwarded phone IP — a **non-loopback** value.
- photo-vault deliberately hands its per-install bearer token (injected into
  `index.html` / served by `GET /api/token`) **only to a loopback client**
  (`security.is_loopback_client`, `src/api.py:1566`, `:1552`). Seeing a non-loopback
  client, it withholds the token. The SPA then has no token, so every `/api/*`
  call returns `401 {"detail":"unauthorized"}` → tabs "unauthorized", counts zero.

Confirmed by diffing the served page: direct loopback returns the index **with**
`window.__PV_TOKEN__` injected (len 700); through the proxy it returned the same
page **without** it (len 617).

## 4. Whose bug is it?

Neither app is wrong in isolation. photo-vault behaves exactly as designed — it
intentionally refuses full access to a client it can see is remote (DNS-rebinding
/ remote-access defense). Rover, however, is *supposed to be transparent* and
was not: it leaked the external Host and client IP to the backend, letting the
backend detect and reject the remote caller. **The fix therefore belongs in
Rover**, so the contract ("a loopback app behaves the same however the client
reached it") actually holds — for photo-vault and for any future strict/loopback-
aware backend, with no per-app config.

## 5. Fix (implemented)

`transparentRewrite(target)` in `internal/launcher/launcher.go` replaces the
stock director. For every proxied request it presents a genuine loopback request
to the backend:

- **Host** → rewritten to the loopback target (`127.0.0.1:<port>`), which is
  always on a strict-host allowlist. Fixes Layer 1; retires the `PV_ALLOWED_HOSTS`
  workaround.
- **X-Forwarded-For / Forwarded** → not sent, and any client-supplied value is
  stripped (it would be spoofed). The backend sees its true peer — Rover, on
  loopback. Fixes Layer 2.
- **X-Forwarded-Host** → set to the real external host, and **X-Forwarded-Proto**
  to the scheme, so backends that build absolute URLs can still learn the external
  origin the standard, opt-in way.

Deliberate trade-off: the backend can no longer distinguish an external caller on
its own, so **Rover becomes the sole authenticator for proxied apps**. That is
already Rover's role — it is the single authenticated front door, and its proxies
bind only to the interface Rover itself is exposed on (here a Tailscale CGNAT
address, where the tailnet is the trust boundary).

Compatibility note: an app that builds absolute redirect URLs purely from the
`Host` header and ignores `X-Forwarded-Host` would now emit loopback URLs. This is
the standard reverse-proxy trade-off; such apps should honor `X-Forwarded-Host`.
(9router, checked, redirects with relative paths and is unaffected.)

## 6. Verification

- Unit test `TestProxyPresentsLoopbackRequest` (`launcher_test.go`): asserts the
  upstream `Host` is the loopback target, no `X-Forwarded-For` reaches the backend,
  and `X-Forwarded-Host` carries the original external host.
- Live, against the real photo-vault: through the proxy the index now injects
  `__PV_TOKEN__` (len 700, identical to direct loopback), `GET /api/people` with
  the injected token returns `200` (and `401` without), and a bogus incoming
  `Host` is accepted (Host rewrite working). `go test ./...`, `gofmt`, `go vet`
  all clean.
