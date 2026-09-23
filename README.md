<p align="center">
  <img src="docs/logo.svg" alt="aiproxy" width="420">
</p>

# aiproxy

[![CI](https://github.com/al-maisan/aiproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/al-maisan/aiproxy/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/al-maisan/aiproxy.svg)](https://pkg.go.dev/github.com/al-maisan/aiproxy)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

An OpenAI-compatible reverse proxy that prefers a **primary** model provider and
transparently falls back to a **secondary** one when the primary's quota is
exhausted.

Subscriptions such as [OpenCode Go](https://opencode.ai/docs/go/) include a
generous monthly allowance per model, but they reset and can be exhausted.
Pay-as-you-go providers such as OpenRouter are always available. `aiproxy` lets
a client point at one endpoint and get *"use the subscription first, spill to
OpenRouter on quota"* — without changing the client.

## How it works

```
client ──▶ aiproxy ──▶ primary (routed models)
                     └─▶ fallback (on quota, connection error, or unrouted models)
```

- For a **routed** model the request is sent to the primary. Fields understood
  only by the fallback (for example OpenRouter's `provider` routing options) are
  stripped first.
- If the primary reports **quota exhaustion** (configurable status codes and body
  patterns) or is unreachable, the **original** request is forwarded verbatim to
  the fallback, preserving client routing options.
- **Unrouted** models go straight to the fallback.
- Responses are streamed, so SSE and long completions pass through unchanged.

## Install

```sh
go install github.com/al-maisan/aiproxy/cmd/aiproxy@latest
```

Prebuilt binaries for Linux, macOS and Windows are published on the
[releases page](https://github.com/al-maisan/aiproxy/releases).

## Quick start

```sh
aiproxy -init                 # write the default config to ~/.config/aiproxy/config.toml
$EDITOR ~/.config/aiproxy/config.toml
aiproxy                       # listen on 127.0.0.1:8787
```

Related flags:

| Flag            | Purpose                                                        |
| --------------- | -------------------------------------------------------------- |
| `-init`         | Write the default config (to `-config`, else the user location)|
| `-print-config` | Print the default config to stdout                             |
| `-force`        | Overwrite an existing file when using `-init`                  |
| `-config PATH`  | Explicit config file                                           |
| `-listen ADDR`  | Override the listen address                                    |
| `-version`      | Print version and exit                                         |

Point your client at `http://127.0.0.1:8787/v1`. For OpenCode you only need to
change the OpenRouter provider's base URL — every model option keeps working:

```jsonc
{
  "provider": {
    "openrouter": {
      "options": { "baseURL": "http://127.0.0.1:8787/v1" }
    }
  }
}
```

## Configuration

The full annotated default is always available via `aiproxy -print-config`.
Top-level keys:

| Key            | Description                                                        |
| -------------- | ------------------------------------------------------------------ |
| `listen`       | `host:port` to bind (default `127.0.0.1:8787`)                     |
| `client_token` | When set, require `Authorization: Bearer <token>` on proxied routes|
| `strip_fields` | Body fields removed before calling the primary                     |

`[log]` — `level` (`debug\|info\|warn\|error`), `format` (`text\|json`).

`[server]` — `read_header_timeout`, `body_read_timeout`,
`upstream_header_timeout`, `dial_timeout`, `idle_conn_timeout`,
`shutdown_timeout`, `max_body_bytes`, `fallback_on_connection_error`.

`[upstreams.primary]` / `[upstreams.fallback]` — `name`, `chat_url`,
`models_url`, `api_key`. Key sources: `literal:<v>`, `env:<VAR>`, `file:<path>`,
or `opencode-auth:<provider>` (read from the OpenCode auth file).

`[quota]` — `statuses` (default `402, 429`) and `patterns` (regular expressions
matched against error bodies; deliberately specific to avoid false positives).

`[[routes]]` — `requested` (model id the client sends), `primary` (model id at
the primary), optional `fallback` (model id at the fallback; defaults to
`requested`).

## Security

- Binds to loopback by default. Set `client_token` before exposing it; readiness
  and proxied routes then require the token (liveness stays open). Binding a
  non-loopback address without `client_token` logs a warning.
- API keys are resolved from environment, files, or the OpenCode auth store and
  are never logged. The client's `Authorization` header is never forwarded
  upstream; a request whose upstream key cannot be resolved fails instead.
- Client token comparison is constant-time; ambient credentials (`Cookie`,
  `X-Forwarded-*`), hop-by-hop headers and headers named in `Connection` are
  stripped in both directions.
- Request bodies are size-limited (and read within `body_read_timeout`) and
  upstream calls are bounded by timeouts.
- TLS certificates are always verified. The generated config is written `0600`,
  and the config is only loaded from explicit, XDG, home or `/etc` locations —
  never the working directory.

## Development

```sh
make test    # go test -race ./...
make cover   # coverage summary
make lint    # golangci-lint
make build   # bin/aiproxy
```

## License

Apache-2.0. See [LICENSE](LICENSE).
