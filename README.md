# iron-forest

iron-forest is a **backlog-to-pull-request factory**. It polls an issue
backlog, and for each unit of work it runs an AI agent in an ephemeral git
worktree, gates the resulting diff, commits and pushes a branch, and opens a
pull request that closes the issue when merged.

The repo is dogfooded: iron-forest's own GitHub issues are its backlog.

## How it works

On each pass over the backlog, an item is a unit of work when all of these hold:

- the issue is open
- the issue is not parked (`forest:wip` label) or failed (`forest:failed` label)
- no open pull request already references the issue

For each item the pipeline runs, serialized one at a time:

1. **claim** — label the issue so another pass skips it
2. **worktree** — create an ephemeral git worktree at the exact base SHA of the default branch
3. **agent** — run the chew agent (opencode) in that worktree with the issue text as the task
4. **gate** — verify the agent did not touch protected paths (`.forest/`, `forest.yaml`)
5. **publish** — commit, push a branch, and open a pull request
6. **record** — append a run record and close the issue

## Commands

The `forest` binary lives in `main.go`. Run it from the repository root:

- `forest list` — print the current backlog (`#N<TAB>title` for each eligible issue)
- `forest once <issue>` — chew a single issue end to end (claim, worktree, agent, gate, publish, record)
- `forest chew` — poll the backlog forever, one item at a time

## Running the agent

iron-forest drives [opencode](https://opencode.ai) as a subprocess. Its
configuration lives in `forest.yaml`:

- `repo` — the GitHub repository whose issues form the backlog
- `poll_interval_seconds` — how long `forest chew` sleeps between passes
- `agent.harness` — the agent runtime (opencode)
- `agent.model` — the provider-qualified model id
- `agent.system_prompt` — the chew agent prompt at `agents/chew.md`
- `agent.protected` — paths the agent must not modify

The agent is invoked per issue as `opencode` in a fresh worktree, so each run
starts from a clean checkout. The issue number and body are appended to the
system prompt as the task to implement, and the agent writes its results to the
worktree (plus a `report.json` that is never committed).
