# Contributing

Thanks for your interest in `aiproxy`.

## Development

Requirements: Go 1.23+.

```sh
git clone https://github.com/al-maisan/aiproxy
cd aiproxy
make test    # go test -race ./...
make lint    # golangci-lint (v2)
make build   # bin/aiproxy
```

## Pull requests

1. Fork the repository and create a topic branch.
2. Keep changes focused; add tests for new behaviour.
3. Ensure `make test` and `make lint` pass locally.
4. Use clear commit messages (Conventional Commits welcome).
5. Open a pull request describing the change and its motivation.

## Reporting bugs

Open an issue using the bug report template. Include the `aiproxy -version`
output, the relevant configuration (redact secrets), and steps to reproduce.

For security issues, follow [SECURITY.md](SECURITY.md) instead of opening a
public issue.
