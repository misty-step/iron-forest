# Changelog

- Shipped the reaction loop (R3): the factory now watches its own pull requests, re-enters on feedback or failing CI, and auto-merges when the owl approves and CI is green. (2026-08-05)
- Bounded the reaction loop: re-entry passes now honor `workflow.max_reaction_fixes`, every failed fix increments the PR's recorded fix count and writes a PR state row, and a PR at the limit is parked stalled for a human. (2026-08-05)
