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

## Race state

[`internal/race`](internal/race) is the race state: a pure state machine
that takes `salsa` packets one at a time and gives, for each, its frame and the
events it gives rise to, `session.started`, `lap.completed` and
`session.ended`, as the `contracts` messages. It opens a session, closes a lap
and ends a session exactly where v1 did, with v1's rules ported from its
`state.py`, `engine.py` and `store.py` at v1.0.0 (paddock#33).

- **Session type.** v1's `SessionDetector` (`sessionkind.py`), cut to the
  kind: a lap race, solo laps, a timed session with a field, or solo timed,
  from the lap total and the field of cars. `session.started` carries the
  verdict on the session's first frame, and `session.ended`'s summary the
  final one, which can differ (paddock#34). Outside a field, `position` and
  `final_position` are left out, where v1 stored GT7's 1; `finishing_position`
  is set only for a lap race that ended at the flag.
- **End reasons.** At the flag, quit, restarted, the next session started, the
  input ended (`EndOfInput`, the end of a recording) and ecu stopped (`Stop`).

- **No clock, no NATS, no files.** Each packet comes with the time ecu received
  it, which becomes its frame's `ce-time`; a session's `started_at` is its
  first frame's. The caller gives the ULID generator: `race.New(ulid.Make)`
  in production, a fixed clock and seeded entropy in tests and replays, so the
  same packets give the same outputs, ids included. depguard in
  [`.golangci.yml`](.golangci.yml) fails the lint on an import of NATS, the
  network or the file system in the package.
- **Ids.** The machine mints a run id when it is made; frames are
  `<run>:frame:<seq>`, with `seq` from 1. Each session gets a ULID when it
  starts, and its events' ids follow `contracts`' `subjects.yaml`;
  `internal/bus` publishes the events under ids with the `<src>` in them
  ([Publishing](#publishing)).
- **Tests.** The tests replay v1's recordings from `salsa`'s `testdata/v1/` and
  compare the sessions, laps and final session kind with what v1 1.0.0 stored
  for each, in
  [`internal/race/testdata/golden/`](internal/race/testdata/golden), which
  `make_golden.py` there says how to make.
- **Benchmark.** `go test -run '^$' -bench Replay ./internal/race` replays
  every recording and reports the p50, p99 and max time of a step, per frame
  and per session start, lap close and session end.

## Publishing

[`internal/bus`](internal/bus) puts the race state's outputs on NATS
(paddock#34), and is the only package that imports nats.go (depguard in
[`.golangci.yml`](.golangci.yml)).

| Subject | What | Captured by |
| --- | --- | --- |
| `twr.v1.<src>.frame.gt7` | every frame | `TELEMETRY` |
| `twr.v1.<src>.frame.gt7.lo` | the same frames, at most one per 50 ms of frame time (20 Hz) | none |
| `twr.v1.<src>.evt.<sid>.session.started` | a session began | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.lap.completed` | a lap ended | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.ended` | a session ended | `EVENTS` |

- **Messages.** Each is the `contracts` message in protobuf
  JSON, with every CloudEvents header from `contracts`' `ce` package:
  `ce-specversion`, `ce-id`, `ce-type`, `ce-source` (`/twr/ecu/gt7`), `ce-time`
  (when ecu received the packet) and `ce-datacontenttype`, and `Nats-Msg-Id`
  equal to `ce-id`. A frame's id is `<run>:frame:<seq>`; an event's is
  `<src>:<sid>:session:started`, `<src>:<sid>:lap:<n>:completed` or
  `<src>:<sid>:session:ended`, so two replays under different sources are not
  deduplicated against each other. `session.started`'s `source` is the `<src>`.
- **Nothing waits on the network.** The step loop encodes its outputs and
  hands them to one goroutine, which owns the connection, without waiting. A
  frame that finds the hand-over full (1,024 frames, about 17 s) is dropped and
  counted, as is every frame while the connection is down. Only that goroutine
  waits on a server that stops reading (up to 2 s a write, then the client
  drops the connection and tries again).
- **Events, in order and acknowledged.** Frames are core publishes; events are
  published through JetStream, one at a time: the next goes only once
  `EVENTS` has acknowledged the one before, and one that is not acknowledged
  within 5 s, or could not be sent, is sent again after `ReconnectWait`. So
  events reach `EVENTS` in the order they happened, and none is lost while ecu
  runs. Up to 2,048 wait, 1,024 in the publisher's queue and 1,024 in the
  hand-over to it; beyond that an event is dropped, counted and logged as an
  error, and the step does not wait. At the end ecu waits up to 5 s for the
  queue, and no longer.
- **Copies.** A copy sent again that `EVENTS` already stored is dropped there
  within its 2 min duplicate window. A copy can still be stored twice in two
  cases: an acknowledgement is lost and the connection then stays down for more
  than 2 min, or the connection stays up but JetStream does not answer for more
  than 2 min (the stream unavailable, say). And a core subscriber on an event
  subject sees every copy sent. So every consumer, live core subscribers as
  well as logbook (whose writes are upserts), treats `ce-id` as the event's
  identity.
- **ecu needs `EVENTS`.** Without the stream (a bare development server, not
  a deployment, which has it from paddock's bootstrap), each event waits 5 s
  for an acknowledgement and is sent again, so events stall and core
  subscribers see copies. ecu logs a warning, once, the first time an event is
  not acknowledged.
- **Configuration.** `--nats` or `TWR_NATS_URL` (`nats://127.0.0.1:4222`), and
  `--src` or `TWR_SRC` (`live`): `live`, `parity` or `replay-<run>`, the run of
  letters, digits, `_` and `-`. Anything else, such as a `.`, `*`, `>` or white
  space, stops ecu at start. ecu logs the server (without credentials) and the
  source at start, and the frames, 20 Hz frames and events published and
  dropped at the end.
- **Tests.** The tests run the `nats-server` release paddock pins, downloaded
  once into the user's cache and checked against its sha256 (or the binary
  `TWR_TEST_NATS_SERVER` names), with the `TELEMETRY` and `EVENTS` definitions
  of paddock's `deploy/nats/streams` in
  [`internal/bus/testdata/streams`](internal/bus/testdata/streams). `go test
  -run '^$' -bench EncodeFrame ./internal/bus` measures a frame's encoding.

## Checks

`make check` runs what CI runs; [`AGENTS.md`](AGENTS.md) says what that is and
what a new clone runs first.

## Conventions

The ones every Go component shares, from the
[`chassis`](https://github.com/those-who-run/chassis) template:

- **Configuration from the environment**, read once at start-up into a typed
  struct; an unset variable takes its default and a value that can't be read
  stops the process. The example reads `ADDR` (`:8080`) and
  `LOG_LEVEL` (`info`).
- **Logging with `log/slog`, as JSON** on standard output, one object per line,
  with key-value pairs.
- **A health endpoint**, `GET /healthz`, which answers 200 while the process can
  serve.

## From the template

This repository was made from `chassis` with `copier copy`, and
`.copier-answers.yml` records the release it came from and the answers given.
Renovate proposes each new `chassis` release as a PR that runs
`copier update`; chassis's README has the rest.

## Releases

Merging the release PR tags `vX.Y.Z`, and the release workflow publishes
`ghcr.io/those-who-run/twr-ecu:X.Y.Z` once the maintainer approves it in
the `release` environment.
