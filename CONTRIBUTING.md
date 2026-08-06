# Contributing

Work from `master` and keep each change focused.

1. Run `mise exec -- go build ./...`.
2. Run `mise exec -- go vet ./...`.
3. Run `mise exec -- go test ./...`.
4. Run `./forest selfcheck` after configuration or agent changes.
5. Use the vocabulary in [AGENTS.md](AGENTS.md).
6. Update the tests and docs that describe your change.
