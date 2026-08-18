# Contributing to Secret Media Bot

Thank you for your interest in contributing!

## Development Guidelines

1. **Prerequisites**: Go 1.26+, Docker / Podman (optional for PostgreSQL integration tests).
2. **Code Standards**:
   - Run `gofmt -w .` before committing.
   - Run `go vet ./...` and `golangci-lint run`.
   - Ensure all tests pass: `go test -race ./...`.
3. **Architecture and Privacy Invariants**:
   - Plaintext secret data or media must never be logged.
   - All persistence paths storing secret material must use AES-256-GCM authenticated encryption through `internal/secretcrypto`.
   - In-memory buffers holding secret material must be zeroed immediately after use with `secretcrypto.Zero(...)`.
4. **Pull Requests**:
   - Write clear commit messages.
   - Include unit and integration tests covering new logic or bug fixes.
