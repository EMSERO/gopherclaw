# Contributing to GopherClaw

Thanks for your interest in contributing to GopherClaw!

## Getting Started

1. Fork and clone the repo
2. Install Go 1.26+
3. Build: `go build -o gopherclaw ./cmd/gopherclaw`
4. Run tests: `go test -race ./...`

## Development

- Run lint: `golangci-lint run ./...`
- CI enforces 78% test coverage, race detection, and zero lint issues
- Target Go 1.26 — use stdlib patterns over third-party abstractions

## Pull Requests

1. Create a feature branch from `main`
2. Keep changes focused — one feature or fix per PR
3. Add tests for new functionality
4. Ensure `go test -race ./...` and `golangci-lint run ./...` pass
5. Write a clear PR description explaining what and why

## Reporting Issues

- Use GitHub Issues
- Include: Go version, OS, config (redact secrets), logs, and steps to reproduce

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Handle all errors explicitly — no `_` on error returns
- Keep dependencies minimal — prefer stdlib when practical
- Use `internal/` packages to keep the API surface small

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
