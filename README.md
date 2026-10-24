# ECU

Ingest, race state and the built-in rules for Those Who Run v2: it owns
the session and publishes frames, events and callouts on NATS.
Every other component subscribes to what it publishes, and `ecu` knows
nothing about who listens ([ADR 0001](https://github.com/those-who-run/paddock/blob/main/docs/adr/0001-v2-architecture.md) in paddock).

Issues are off in this repository. Every issue, the roadmap and the decisions
(ADRs) live in [those-who-run/paddock](https://github.com/those-who-run/paddock). A pull request here
names its paddock issue: `Closes those-who-run/paddock#N.` when it meets every
acceptance criterion, `Part of those-who-run/paddock#N` otherwise
([`AGENTS.md`](AGENTS.md)).

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.
