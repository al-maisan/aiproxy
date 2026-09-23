# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `server.body_read_timeout` bounds how long a client may take to send a request
  body, mitigating slow-body connection hoarding.
- A warning is logged when binding a non-loopback address without `client_token`.

### Security

- The client's `Authorization` header is no longer forwarded to an upstream when
  that upstream's API key cannot be resolved; requests fail instead of leaking
  the proxy auth token (or a client's provider key) to a third party.
- The config file is no longer discovered in the current working directory,
  preventing planted-config credential and prompt exfiltration.
- `/readyz` no longer discloses key-source paths or environment variable names.
- Headers named in the `Connection` header are now treated as hop-by-hop
  (RFC 7230 §6.1) in both directions.
- Empty `literal:` key sources are rejected; `client_token` is trimmed during
  validation.
- Requests re-encoded for the primary now preserve large integer precision.

## [0.1.0] - 2026-09-23

### Added

- OpenAI-compatible reverse proxy with primary-first routing and transparent
  fallback to a secondary upstream on quota exhaustion or connection failure.
- Configurable upstreams, per-route primary/fallback model mapping, quota
  detection (status codes and body patterns), header/field stripping, timeouts
  and body limits via a TOML config file.
- API key sources: `literal:`, `env:`, `file:` and `opencode-auth:`.
- `GET /healthz` (liveness) and `GET /readyz` (readiness, authenticated).
- Structured logging, request correlation IDs and streaming pass-through.
- `-init` / `-print-config` to generate the annotated default configuration.

[Unreleased]: https://github.com/al-maisan/aiproxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/al-maisan/aiproxy/releases/tag/v0.1.0
