# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
