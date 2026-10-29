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
  starts, and its events' ids are `<sid>:<area>.<what>:<part>`, the
  catalogue's form in `contracts`' `subjects.yaml` without the source, which
  `internal/bus` puts in front ([Publishing](#publishing)).
- **Tests.** The tests replay v1's recordings from `salsa`'s `testdata/v1/` and
  compare the sessions, laps and final session kind with what v1 1.0.0 stored
  for each, in
  [`internal/race/testdata/golden/`](internal/race/testdata/golden), which
  `make_golden.py` there says how to make.
- **Benchmark.** `go test -run '^$' -bench Replay ./internal/race` replays
  every recording and reports the p50, p99 and max time of a step, per frame
  and per session start, lap close and session end.

## Track detection

GT7's telemetry carries no track id. [`internal/track`](internal/track) is the
track table and v1's track detector, ported rule for rule from its
`tracks.py` and `trackdetect.py` at v1.0.0 (paddock#80): a pure library that
nothing calls yet. Publishing the verdict is paddock#81.

- **The table.** v1's `tracks.csv`, unchanged and embedded in the binary: 108
  layouts, 105 with a finish line, its direction and a lap box, from gt-telem
  and Bornhall's gt7telemetry ([`NOTICE`](NOTICE)). Rows learned from our own
  laps have ids from 100001 and a lap length.
- **The detector.** Fed frames with `Observe` (the contracts frame ecu makes
  for each packet), it narrows the table with v1's facts: a finish line within
  1 km of where the car has been, the path inside the box, the line crossed in
  the right direction (GT7's lap counter first, the path only while racing and
  until the counter has shown the line), and a whole lap that fits the box and
  the length. Its verdict is `confirmed`, `probable`, `ambiguous` or `none`,
  with the candidates, the evidence in words, and the circuit when only that is
  known (Spa and Spa 24h, Fuji and Fuji Short).
- **Restarts and joins.** `Reset` on a restart or a new start; `Join` for a
  session joined part way, which names no track before a line. A menu or a
  loading screen at lap 0 starts over by itself; a pit cut-scene, which keeps
  the lap counter, doesn't.
- **Tests.** Every one of v1's 25 recordings gives v1's verdicts, change by
  change, with v1's evidence, as `make_golden.py` in
  [`internal/track/testdata/golden/`](internal/track/testdata/golden) wrote
  them from v1.0.0. The 18 recordings `salsa` doesn't carry are copied into
  [`internal/track/testdata/v1/`](internal/track/testdata/v1) with their
  provenance and checksums.

## Listening to the PS5

`ecu`, run with no subcommand, listens to the PS5 live
([`internal/live`](internal/live), paddock#106): it sends GT7's heartbeat,
reads each datagram, decodes it with `salsa` and drives the race state and the
publisher with it, exactly as a recording's packets would. The live source is a
`salsa.Source`, so the rest of ecu cannot tell a live drive from a replayed one.
It ports v1's lessons (`telemetry.py` at v1.0.0), not its code:

- **Our own heartbeat.** An `A` goes to the PS5's UDP port 33739 at start and
  then every `TWR_ECU_HEARTBEAT_SECS`, from a fresh socket each time. A failed
  send is logged once per outage, and when it gets out again, and never stops
  the heartbeat: v1 lost the stream for good when gt-telem's 10 s heartbeat died
  on one error.
- **A listener that comes back.** ecu listens on UDP 33740. When it cannot bind
  (the port is busy) or its socket dies, it is logged and bound again with
  exponential backoff, from 1 s up to 30 s. It never stops ecu.
- **In order.** A packet whose id is not newer than the last one handed on is
  dropped and counted, unless it is 600 ids or more back, which is a new game
  session. When the race state falls behind (600 packets waiting, 10 s), the
  oldest waiting is dropped.
- **No discovery.** UDP broadcast does not work from a pod, so the PS5's
  address is required: without `TWR_ECU_PS5_IP`, or with a value that is not an
  IP address, ecu stops at start (exit 2), so a typo in a deployment fails
  loudly rather than leaving a healthy pod that hears nothing (the
  maintainer's decision, 2026-10-28).
- **Live only on `live`.** Live frames go out under the source `live` and no
  other (paddock#106, R8): `--src` or `TWR_SRC` set to anything else stops ecu
  at start.

At the end ecu logs the packets handed on, dropped late, undecodable and
overflowed, the heartbeats sent and failed, and the listener's restarts.

| Variable | Default | What |
| --- | --- | --- |
| `TWR_ECU_PS5_IP` | none: required | The PS5's IP address, where the heartbeat goes. |
| `TWR_ECU_HEARTBEAT_SECS` | `2` | Seconds between heartbeats, from 0.01 to 3,600. |
| `TWR_ECU_RECORDINGS_DIR` | unset: no recording | The directory each drive is recorded in ([Recording](#recording)). |

The ports: UDP 33740 in (the PS5 sends there, to the address the heartbeat
came from), and UDP out to the PS5's 33739. In a pod, 33740/UDP needs a
`hostPort`, as v1's Deployment has.

Not here yet, and still paddock#106's: telemetry lost and restored after the
timeout (`telemetry.lost`, R6, with paddock#87), and the listener's state in
`/healthz` and the `live` KV (R7, with paddock#88 and paddock#107), and the
metrics (R9).

## Recording

With `TWR_ECU_RECORDINGS_DIR` set, ecu records every packet it drives on
([`internal/record`](internal/record), paddock#266) in **v1's recording
format**: gzip-compressed JSON lines, one v1 `RaceState` per line, byte for byte
as v1's `RaceState.to_dict` and `RecordingWriter` write them. So a drive raced
on v2 replays through v1's `replay` and through `salsa`'s `OpenRecording`, and
v1 and v2 can be compared on it. A test writes v1's own recordings again and
compares them byte for byte, and another reads a live drive's recording back
through `salsa`.

- **One file per run**, made on the first packet and named for the UTC time
  then, as v1 names its outings: `20261031-200509.jsonl.gz` (`-2` and so on
  when one of that second exists). A run that hears nothing makes no file.
- **`t` is seconds since ecu started**, on the monotonic clock: only
  differences between times mean anything, as in v1's recordings.
- **Off the frame path.** A goroutine writes the file; up to 4,096 packets
  wait for it (about 68 s), and a packet beyond that is dropped from the
  recording and counted. The file is written in gzip members of 600 lines,
  so if ecu is killed mid-drive everything up to the last 10 s reads back.
- **A directory ecu cannot write to stops it at start**; one that fails later
  stops the recording, logged, and never the drive.
- No PS5 address or other setting is written into a recording.

Not here yet, and still paddock#266's: a recording per outing, archiving,
pruning and listing recordings.

## Replay

`ecu replay PATH` replays a v1 recording onto NATS (paddock#35): `.jsonl`,
`.jsonl.gz`, or `-` for standard input, read through `salsa`'s recording
Source. Its packets drive the same race state and the same publisher as a live
drive's, so from the packet on a replay runs exactly the code a live drive
does. At the end of the recording the open session ends as the input ended
(`END_REASON_INPUT_ENDED`, the race state's reason for the end of a
recording), ecu waits up to 30 s for the server to take what is queued, and
exits.

```sh
ecu replay --speed max --src parity real-race-2laps-2026-10-14.jsonl.gz
docker run --rm -v "$PWD:/recordings" ghcr.io/those-who-run/twr-ecu \
  replay --nats nats://<host>:4222 /recordings/real-race-2laps-2026-10-14.jsonl.gz
```

| Flag | Default | What |
| --- | --- | --- |
| `--speed` | `1` | `1` keeps the recording's own pacing, a pause included; any factor above 0 (`10` is ten times faster); `max` does not wait. |
| `--src` | `replay-<stem>-<suffix>` | The source token: `parity`, or `replay-<run>`. `live` is refused. |
| `--run` | none | Publish under `replay-<run>`, with no suffix. Not with `--src`. |
| `--clock` | `2026-01-01T00:00:00Z` | The first frame's wall-clock time: ISO 8601, UTC unless it says (`2026-10-31T20:05:09+01:00`), or epoch seconds. |
| `--nats` | `TWR_NATS_URL`, then `nats://127.0.0.1:4222` | The NATS server. |

- **The source.** By default `replay-<stem>-<suffix>`: the file name without
  `.gz` and its extension, lower-cased, anything outside `[a-z0-9-]` made a
  `-`, and 6 random characters, so `Real Race 2laps.jsonl.gz` replays under
  `replay-real-race-2laps-k3x7qa`, and two replays of one file don't share a
  source. Standard input's stem is `stdin`. `TWR_SRC` is not read, so a live
  deployment's `live` never leaks into a replay.
- **The clock.** v1's `t` is the recording machine's uptime, with no date. A
  frame's `ce-time`, and a session's `started_at`, are the first frame's time
  (`--clock`, or 2026-01-01T00:00:00Z) plus the frame's `t` less the first
  frame's, whatever the speed. So a replay without `--clock` dates the same
  way every time.
- **Pacing.** Each packet waits for its own time since the first, divided by
  the speed, so the waits don't add up over a long recording.
- **No frame dropped.** A replay publishes with `PublishWait`: a frame or an
  event that finds the publisher's hand-over full waits for room instead of
  being dropped, so `--speed max` goes as fast as the server takes frames and
  no faster. Live keeps `Publish`, which never waits.
- **No recording, no PS5.** A replay never writes to
  `TWR_ECU_RECORDINGS_DIR` and needs no `TWR_ECU_PS5_IP`. It serves no
  `/healthz`, and logs JSON to standard output as live does.
- **Exit codes.** `0` once everything is published. `1` for a recording that
  cannot be read: the message names the file and the line
  (`x.jsonl:4: ...`); what came before the bad line is published, and its
  session ended as ecu stopped. `2` for a NATS server not reached within 10 s,
  one that did not take everything (any frame or event dropped, which the log
  line `published` counts), or a bad flag. `130` when SIGINT or SIGTERM stops
  it, the open session ended as ecu stopped.

Not here yet, and still paddock#35's: `--seed` for the ULIDs (session and
event ids are random on each run until then), `--from` and `--to`, and the
one line per session printed at the end.

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
  equal to `ce-id`. The ids follow the `msg_id` schemes of `contracts`'
  `subjects.yaml` (v0.1.5, paddock#279): a frame's is `<run>:frame:<seq>`, and
  an event's is `<src>:<sid>:session.started:1`, `<src>:<sid>:lap.completed:<n>`
  or `<src>:<sid>:session.ended:1`, with the subject's `<src>`, so two replays
  under different sources are not deduplicated against each other. An event
  published with no session open would go on `evt._`, with `<run>` in
  `<sid>`'s place; the race state sends none yet. A test checks every id
  against the catalogue. `session.started`'s `source` is the `<src>`.
- **Nothing waits on the network.** The step loop encodes its outputs and
  hands them to one goroutine, which owns the connection, without waiting. A
  frame that finds the hand-over full (1,024 frames, about 17 s) is dropped and
  counted, as is every frame while the connection is down. Only that goroutine
  waits on a server that stops reading: up to 2 s a write, and the next write
  as long again, since the client keeps the connection until its pings go
  unanswered (nats.go's defaults, minutes). At shutdown ecu closes such a
  connection under the write, so the wait ends at once.
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
  space, stops ecu at start, and so does anything but `live` when it listens to
  the PS5 ([Listening to the PS5](#listening-to-the-ps5)); `ecu replay` refuses
  `live` instead ([Replay](#replay)). ecu logs the server (without credentials) and the
  source at start, and the frames, 20 Hz frames and events published and
  dropped at the end, with `"final": true`. When a write still
  holds the publisher once the 5 s are up, that line has `"final": false`,
  ecu closes the connection under the write so it gives up at once, and waits
  up to 3 s more for the line with the final totals.
- **Tests.** The tests run the `nats-server` release paddock pins, downloaded
  once into the user's cache and checked against its sha256 (or the binary
  `TWR_TEST_NATS_SERVER` names), with the `TELEMETRY` and `EVENTS` definitions
  of paddock's `deploy/nats/streams` in
  [`internal/bus/bustest/streams`](internal/bus/bustest/streams), in
  [`internal/bus/bustest`](internal/bus/bustest), which `cmd/ecu`'s replay
  tests use too. `go test
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
