# Changelog

- 2026-08-06: Replaced the pull-request state machine, label-as-state ownership, and cost accounting. Three independent Flows coordinate through repository state, while Verdicts and checks are stored as git notes. The Builder publishes unreviewed branches; the Verifier reviews and merges them on its own clock.
