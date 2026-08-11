# 0008 — Agent and check commands use a minimal process sandbox

Status: accepted, 2026-08-08

## Context

A private `HOME` and a reduced environment do not isolate a process. GitHub CLI can discover `/run/user/<uid>/bus` without `DBUS_SESSION_BUS_ADDRESS` and read an operating-system keyring. An agent with shell access can also use absolute Host paths, read same-user files, or inspect parent environments through `/proc`.

A failing `gh` command at the front of `PATH` blocks normal lookup only. It does not defend the credential boundary because a child can remove the command or invoke another path.

## Decision

Require Bubblewrap for every agent and declared check. Do not provide an unsandboxed fallback.

Start each child in new user, PID, IPC, and UTS namespaces. Keep Bubblewrap's
PID-1 reaper, and make Runner trace draining deadline-aware. Mount private
runtime trees. Expose only selected system files, the worktree, private home,
staged executables, and validated toolchains. Keep build caches private.

Build a read-only Git view from known history, references, index, and current
worktree state. Omit unrecognized common files. Keep the worktree `.git` entry
immutable. Hide configuration, Host hooks, and sibling worktree administration.

Read agent declarations and provider configuration through repository roots.
Reject escaping symlinks. Stage only strict Mint OpenRouter provider fields.
Keep a failing `gh` command as defense in depth.

Derive a private shell from each agent's strict `bash_allow` list. Point
OpenCode 1.18.11 at that shell through its documented `shell` option. Resolve
that exact version from mise instead of the ambient `PATH`; the factory's
`.mise.toml` installs the same version. Reject mismatched executables before
launch. Reject unlisted commands, shell syntax, traversal, outside paths, and
undeclared commands nested through mise before `exec`.

Copy only known system CA bundle files. Do not mount the Host `/etc/ssl`,
`/etc/pki`, or `/sys` trees.

Share the Host network namespace because OpenCode must reach Mint. Agent
permissions define intended network use, but no destination firewall enforces
them. The sandbox isolates Host files, processes, sockets, and credentials.

## Consequences

`forest selfcheck` fails when Bubblewrap is missing or user namespaces cannot start. CI installs Bubblewrap before build verification.

Absolute Host paths no longer reveal the operator home, session bus, another Run's temporary files, parent environment, or Git checkout credentials.

An allowed tool can operate only through a plain argument vector inside the
worktree. Bubblewrap remains the containment boundary for every child process
that an allowed compiler, package tool, or Git command starts.

Resolve each declared path before launch. Reject a symlinked toolchain root that escapes its validated parent. Keep every writable cache in the private run home.
