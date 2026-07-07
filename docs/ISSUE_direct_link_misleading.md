# Issue: the "direct" project link is a guess, not a verified fact

**Date:** 2026-07-07
**Audited commit:** `3be6999` (Make Stop wait for the reaper so exit state is recorded when it returns)
**Scope:** The two links the dashboard shows per running project — the blue "direct"
link (`running_url`) and the green "proxy" link (`proxy_url`) — and whether the
blue one can be trusted for an arbitrary registered project.

This document: (1) how the two links are produced today, (2) why the blue one is
unreliable across the general space of projects rover can register (not just one
project's app), (3) a security angle for hardened deployments, (4) three fix
options ranked by robustness, not yet implemented. All claims cite `file:line`.

---

## 1. How the two links are actually produced today

### 1.1 The proxy link (green, `proxy_url`) — reliable by construction

`internal/launcher/launcher.go:396`:
```go
rp.info.ProxyURL = fmt.Sprintf("http://%s:%d", proxyURLHost(m.bindHost), pport)
```
This is always rover's own bind interface plus the dedicated proxy port rover
itself opened (`launcher.go:327`, `net.Listen` on `m.bindHost`). Rover's proxy
(`httputil.NewSingleHostReverseProxy`, `launcher.go:334`) always forwards
internally to `127.0.0.1:<targetPort>` — which is guaranteed reachable *from
rover's own process*, regardless of what interface the child app actually bound.

On top of that, the frontend rewrites the proxy link's hostname to whatever host
the browser used to reach rover, on every render:
```js
// server.go:2210-2213
function displayUrl(u){
try{const p=new URL(u);p.hostname=location.hostname;return p.toString();}catch(e){return u;}
}
```
So the proxy link is correct from localhost, LAN, or Tailscale alike — it adapts
to the viewer.

### 1.2 The direct link (blue, `running_url`) — inferred, not verified

Two ways it gets a value, both unreliable:

**(a) Scraped from the app's own stdout.** `launcher.go:884-887` scans output for
anything matching `probeURLRe` (`https?://\S+`, defined `probe.go:104`), and if
the matched text mentions `127.0.0.1`/`localhost`, rewrites it to rover's own
bind IP:
```go
// launcher.go:265-270
func replaceLoopback(url string) string {
	ip := localIP()
	if ip == "localhost" { return url }
	return loopbackURLRe.ReplaceAllString(url, "${1}"+ip)
}
```
This assumes the app's printed banner reflects where it's actually reachable.
Many frameworks print `127.0.0.1` in their startup banner even when bound to
`0.0.0.0` (classic dev-server behavior) — and many print nothing at all.

**(b) No log match — hardcoded fallback**, `launcher.go:606-607`:
```go
if rp.info.URL == "" {
    rp.info.URL = fmt.Sprintf("http://127.0.0.1:%d", port)
}
```
Critically, this fallback is **never rewritten for the viewer**, not
server-side (no `replaceLoopback` call here) and not client-side either —
`urlHtml` renders `p.running_url` raw with no `displayUrl()` call
(`server.go:2138`), unlike the proxy link (`server.go:2139`, which does call
`displayUrl()`). So a phone viewing the dashboard over Tailscale, for an app
that printed no self-referencing URL, is shown a link that literally reads
`http://127.0.0.1:<port>`. Clicking it from the phone hits the *phone's own*
loopback — guaranteed wrong, not merely unreliable.

---

## 2. Why this is a general problem, not a one-project issue

Enumerating the actual space of apps a user can register (not assuming any
particular one):

| App behavior | What the blue link shows | Actually correct? |
|---|---|---|
| Binds loopback only, prints a `127.0.0.1` URL in its logs | Rewritten to rover's bind IP (§1.2a) | **No** — dead link off-host |
| Binds loopback only, prints no URL | Raw `127.0.0.1`, never rewritten (§1.2b) | **No** — wrong for every remote viewer, worse than the row above |
| Binds `0.0.0.0`/LAN, log text happens to say `127.0.0.1` | Rewritten to rover's bind IP | Correct, but by accident |
| Binds a specific non-loopback interface rover doesn't share | Whatever the regex found, untouched | Unverified either way |

None of these branches check the one fact that determines correctness: **what
address the socket is actually bound to.** Rover never asks the OS; it only
parses text the child process happened to print. Since rover is meant to front
arbitrary third-party programs, this can't be tuned per-app — it needs to be
right for programs whose author never anticipated running behind rover at all.

Confirmed via source audit that today's registered projects are split exactly
along this fault line — 6 of 7 bind loopback-only (Dream Job Prep, creator-engine,
investments, photo-vault, productivity, plus a newly-registered `opencode`
entry with an explicit `--hostname 127.0.0.1`), and one (`family-finance-app`)
binds `0.0.0.0` for real via a `--host` flag its own code genuinely honors —
i.e. this isn't a hypothetical split, it already exists in one real registry.

---

## 3. Security angle for hardened deployments

Rover has a `--proxy-auth on|off|auto` mode (`cmd/serve.go:37`) that gates the
proxy behind rover's own login. If a project's app is *also* reachable directly
(bound `0.0.0.0`), showing its direct link as a clickable, visually-equal link
right next to the authenticated proxy link implies it's an equally valid way
in — when it's actually a silent bypass of the auth gate the operator just
turned on. The UI shouldn't present an auth-bypassing path with the same visual
weight as the authenticated one.

---

## 4. Fix options, ranked by robustness

**Not implemented — for discussion / a future change.**

1. **Ground truth via socket introspection (the real fix).** After launching
   the child process, rover already has its PID. Query the OS for what
   address:port that PID is actually listening on (Windows:
   `GetExtendedTcpTable` / `netstat`-equivalent; Linux: `/proc/net/tcp` +
   inode→PID match) instead of trusting scraped banner text. Show a clickable
   direct link only when the bind is genuinely non-loopback, and route it
   through the same `displayUrl()` client-side rewrite the proxy link already
   gets. This is correct for every row in the table in §2 because it stops
   guessing and checks reality.

2. **Cheap stopgap, still correct in spirit.** Stop rendering the direct link
   as a clickable link at all when `proxy_enabled` is true; render it as plain
   informational text ("app reports `127.0.0.1:3000`"), and make the proxy
   link the sole primary call-to-action ("Open (via rover)"). Avoids the OS
   socket-table work entirely by just not promising something rover can't
   verify.

3. **Minimum viable / lowest effort.** Keep both links, but (a) route the
   direct link through the same `displayUrl()` rewrite for consistency, and
   (b) label them honestly — "App's reported address (may only work on the
   rover host)" vs. "Proxy (works from anywhere rover is reachable) —
   recommended." Doesn't fix reachability, just removes the silent trap where
   a dead-looking-alive link is presented with no caveat.

For an OSS tool fronting arbitrary third-party apps, (1) is the architecturally
correct answer — it replaces inference with a real check. (2) is the pragmatic
middle ground if socket introspection is out of scope for now. (3) is a
band-aid but strictly better than the current behavior.

---

## 5. Resolution (implemented 2026-07-07)

Option (1) with option (3)'s rendering, plus follow-ups:

- **Socket-level verification, cheaper than table parsing:** after the
  readiness probe confirms a listener, rover *dials* the port on its own
  advertised interface (`verifyDirectURL`, `launcher.go`). A successful TCP
  connect is exactly the fact the link claims. Stdlib-only, cross-platform.
  The result is the new `direct_url` field; it is never set when `--proxy-auth`
  is on (§3: no advertised auth bypass).
- **`replaceLoopback` removed.** Scraped banner URLs stay verbatim as
  informational text; only `direct_url` carries a verified host.
- **Viewer-aware local links:** the server tells the UI whether the request
  came from the rover host itself (`requestFromHostMachine`, TCP source vs own
  interface addresses, X-Forwarded-For honored only from loopback). On-host
  viewers get a clickable loopback link; remote viewers get muted
  "(host only)" text. This works even when laptop and phone use the same
  bind-address URL.
- **Kinds:** projects are classified `web`/`tcp`/`task` by the validation
  probe; the proxy is not started for `tcp` listeners (it could only 502).
  Legacy registry entries without a kind keep the old always-proxy behavior.
  Port-less `task` projects are registrable from the UI (console-only cards).
