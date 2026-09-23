# Security Policy

## Supported versions

The latest release on the `main` branch is supported with security fixes.

## Reporting a vulnerability

Please report suspected vulnerabilities privately using GitHub's
[private vulnerability reporting](https://github.com/al-maisan/aiproxy/security/advisories/new).
Do not open a public issue for security problems.

Include a description, reproduction steps, and the affected version. You can
expect an acknowledgement and a coordinated disclosure timeline.

## Threat model

`aiproxy` is a local reverse proxy:

- It binds to `127.0.0.1` by default. Only expose it on a wider interface after
  setting `client_token`.
- API keys are read from the environment, files, or the OpenCode auth store and
  are never written to logs.
- Upstream TLS certificates are always verified.
- Request bodies are size-limited, upstream calls are bounded by timeouts, and
  ambient/forwarding headers are stripped in both directions.

Operators are responsible for the secrecy and permissions of any key files and
for the network placement of the service.
