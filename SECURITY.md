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
- The raw secret is never stored in the browser or sent over the wire after login

### `?secret=` Query Parameter
EventSource connections (SSE streams) pass the token as a `?secret=` URL query parameter because the browser EventSource API does not support custom headers. This token appears in server access logs. Keep this in mind if you share log files.

### No Multi-User Support
Rover has a single shared secret — there is no per-user authentication or RBAC. All authenticated users have full access.
