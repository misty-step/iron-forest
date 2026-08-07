# Verify a change

Run these steps from the repository root.

1. Build with `mise exec -- go build ./...`.
2. Run `mise exec -- go vet ./...`.
3. Run `mise exec -- go test ./...`.
4. Run `./forest selfcheck` to verify the configuration and agent declarations.
5. Review the changed surface against the owning source files.
6. For Flow changes, inspect `flow.go`, `flow_builder.go`, `flow_verifier.go`, and `flow_fixer.go`.
7. For state changes, inspect `refs.go` and `notes.go`.
8. For execution changes, inspect `runner.go`, `projection.go`, `worktree.go`, `daemon.go`, and `main.go`.
9. For config changes, inspect `config.go`.
10. Run the exact command or Flow affected by the change.
11. Confirm that the change updates its own tests and operator documentation.

Keep the vocabulary defined in [AGENTS.md](AGENTS.md). No path is withheld from an agent; `docs/adr/0003` removed the protected-path list.
