# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- `X-Amz-Security-Token` (an ambient AWS session credential) is now stripped
  from forwarded requests, alongside the other ambient credentials; a client
  operating behind AWS SIGV4 would otherwise have its session token forwarded
  to the upstream provider.

### Fixed

- Quota fallbacks now log which configured rule matched (the status code or the
  pattern) at the default `info` level, so a fallback triggered by an upstream
  `400` is diagnosable without lowering the log level. The raw response body
  remains `debug`-only, since a provider error may echo request content.

## [0.3.0] - 2026-09-23

### Added

- Request statistics: `GET /stats` (JSON) and `GET /metrics` (Prometheus) report
  per-upstream attempts/served counts and served share, fallback reasons, and
  latency measured to response completion (sum/avg/min/max, p50/p90/p99, and a
  Prometheus histogram). Configured via `[stats]` (`disabled`, `buckets`); both
  endpoints honour `client_token`.

## [0.2.0] - 2026-09-23

### Added

- `server.body_read_timeout` bounds how long a client may take to send a request
  body, mitigating slow-body connection hoarding.
- A warning is logged when binding a non-loopback address without `client_token`.

### Changed

- **Breaking:** a request is no longer served with the client's `Authorization`
  header when the target upstream's API key is missing. Clients that relied on
  sending their own key with the upstream key left unset now receive `502`
  instead. Configure the upstream key, or leave the upstream properly keyed.
- A missing or unresolvable upstream API key no longer falls back to the other
  upstream from the primary path; it fails with `502`. A missing primary key
  therefore no longer silently bills every request to the fallback.
- Request-body read deadlines now return `408` instead of `400`.

### Security

- The client's `Authorization` header is no longer forwarded to an upstream when
  that upstream's API key cannot be resolved; requests fail instead of leaking
  the proxy auth token (or a client's provider key) to a third party.
- The config file is no longer discovered in the current working directory,
  preventing planted-config credential and prompt exfiltration.
- `/readyz` no longer discloses key-source paths or environment variable names.
- Headers named in the `Connection` header are now treated as hop-by-hop
  (RFC 7230 §6.1) in both directions.
- Empty `literal:` key sources and blank `client_token` values are rejected when
  the configuration is loaded.
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

[0.3.0]: https://github.com/al-maisan/aiproxy/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/al-maisan/aiproxy/releases/tag/v0.2.0
[0.1.0]: https://github.com/al-maisan/aiproxy/releases/tag/v0.1.0
