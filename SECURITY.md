# Security Policy

## Threat Model

Rover is a **single-user, self-hosted tool** designed to run on your own machine or a private network. It is **not designed** to be exposed directly to the public internet without a firewall rule or VPN.

Core assumptions:
- The operator controls who can reach the port (firewall, VPN, Tailscale, etc.)
- `--secret` is always set in any networked deployment
- TLS is enabled when traffic crosses an untrusted network

## Supported Versions

| Version | Supported |
|---------|-----------|
| latest  | ✅        |

## Reporting a Vulnerability

Please **do not** file a public GitHub issue for security vulnerabilities.

Report privately by emailing the maintainer or by opening a [GitHub Security Advisory](https://github.com/ylnhari/rover/security/advisories/new).

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (optional)

You will receive a response within 72 hours. If the issue is confirmed, a patch will be released and you will be credited (unless you prefer anonymity).

## Known Security Considerations

### Command Execution
Rover executes arbitrary shell commands. Always:
- Set `--secret` with a strong random value (e.g. `openssl rand -hex 32`)
- Restrict network access to the port at the firewall level
- Use `--allow` to limit which command prefixes are permitted
- Enable TLS (`--tls-cert` / `--tls-key`) if traffic crosses an untrusted network

### Authentication Tokens
- Session tokens are short-lived (24 hours) and HMAC-signed with the server secret
- Tokens are stored in `sessionStorage` (cleared when the browser tab closes)
- Project proxies use a purpose-separated HttpOnly credential that control APIs reject
- The raw secret is never stored in the browser or sent over the wire after login

### Project Proxies
- New projects default to `requires_auth: true`; this per-project gate is enforced even when the global `--proxy-auth` mode is off
- Rover strips its proxy cookie, browser `Origin`, client identity/forwarding headers, and inbound `X-Rover-*` headers before forwarding
- With a configured master secret, every forwarded request carries a fresh `X-Rover-Proxy` HMAC bound to its project, target, timestamp, and exact upstream method/request URI as sent to the backend
- A launched proxy-enabled project receives only its one-way project-scoped `ROVER_PROXY_VERIFY` value; inherited values are removed, and the master secret is never passed
- A backend treating the proof as authorization must use one long-lived replay-aware verifier; replay state is process-local unless backend workers explicitly share it
- HMAC verification is symmetric: a backend can mint proofs for itself with its scoped value, but cannot authenticate rover's API or another project
- Adopted processes receive no verifier environment update; any verifier provisioning for them is a separate operator-controlled trust decision

### Child Processes
- User commands, project validation probes, tasks, and launched projects do not inherit `ROVER_SECRET`, `ROVER_PROXY_VERIFY`, or environment variable names ending in `_SECRET`, `_TOKEN`, or `_KEY`; only Rover may inject a newly derived project-scoped verifier value into a launched proxy-enabled project
- Ordinary environment variables, the launcher `PORT` contract, and streaming process I/O are preserved

### `?secret=` Query Parameter
EventSource connections (SSE streams) pass the token as a `?secret=` URL query parameter because the browser EventSource API does not support custom headers. This token appears in server access logs. Keep this in mind if you share log files.

### No Multi-User Support
Rover has a single shared secret — there is no per-user authentication or RBAC. All authenticated users have full access.
