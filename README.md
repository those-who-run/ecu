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
events it gives rise to, `session.started`, `pit.entered`, `pit.exited`,
`pit.teleport`, `race.final_lap`, `race.position`, `session.track`,
`session.kind`, `session.paused`, `session.resumed`, `lap.completed`, `lap.best`, `race.finished`,
`session.ended` and `session.discarded`, as the `contracts` messages. It opens a session, closes a lap
and ends a session exactly where v1 did, with v1's rules ported from its
`state.py`, `engine.py` and `store.py` at v1.0.0 (paddock#33).

- **Session type.** The session's kind ([The session's kind](#the-sessions-kind)):
  a lap race, solo laps, a timed session with a field, or solo timed.
  `session.started` carries the verdict on the session's first frame, and
  `session.ended`'s summary the final one, which can differ (paddock#34). Outside a field, `position` and
  `final_position` are left out, where v1 stored GT7's 1; `finishing_position`
  is set only for a lap race that ended at the flag.
- **Joined part way.** A session whose first frame is a race frame past lap 1,
  with nothing before it (ecu came up mid-session), is joined part way:
  `session.started` carries `joined_part_way` and `joined_on_lap`, its start
  is unknown, no track is named before a line, and the rules are told
  (`Context.Joined`), as v1's `joined_part_way` (paddock#75).
- **End reasons.** At the flag, quit (a loading screen with the lap counter
  reset), back to a menu or the track screen (`END_REASON_MENU`, contracts
  v0.2.0), restarted, the next session started, the input ended (`EndOfInput`,
  the end of a recording) and ecu stopped (`Stop`). A timed race ended after
  its timed finish is `END_REASON_TIMED_FINISH` ([The finish](#the-finish)),
  and a timed session left for a menu before it, as a time trial always is,
  `END_REASON_MENU`. A session can also end in a pause ([Pauses](#pauses)),
  the feed lost in one, live, among the ways
  ([Telemetry lost and restored](#telemetry-lost-and-restored)).

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
- **The start check.** A start GT7's cars-on-track flag is off for is held
  until ecu knows whether it is a drive, a replay or a demo
  ([The start check](#the-start-check)).
- **Tests.** The tests replay v1's recordings from `salsa`'s `testdata/v1/` and
  compare the sessions, laps and final session kind with what v1 1.0.0 stored
  for each, in
  [`internal/race/testdata/golden/`](internal/race/testdata/golden), which
  `make_golden.py` there says how to make.
- **Benchmark.** `go test -run '^$' -bench Replay ./internal/race` replays
  every recording and reports the p50, p99 and max time of a step, per frame
  and per session start, lap close and session end.

## The start check

GT7 sends driving frames for its own race replays and for the demo race it
plays when the PS5 sits idle on the menu. [`internal/race/start.go`](internal/race/start.go)
is v1's `DriverCheck` (`sessionkind.py` at v1.0.0) and the hold around it in
`Engine.process` (paddock#74), with v1's thresholds as named constants.

- **A drive at once.** A start whose first frame has the cars-on-track flag
  set is a drive: `session.started` on that frame, with no hold. Every drive
  since 2026-10-16 has it.
- **The hold.** A start with the flag off is held. The machine still gives
  each frame, but no event, and the rules (`Machine.Rules`) don't see it. It is
  a drive as soon as the flag comes on, a replay as soon as the fuel drops more
  than 0.05 below the highest level of the hold (with a tank), and otherwise,
  after 3 s or 180 packets, or a pause or an end first, a demo if no loading
  screen came since the last menu, or if no menu was seen and GT7 reports two
  or more cars; a drive otherwise.
- **A drive.** The held frames go through the race state and the rules in
  turn, as if never held, so `session.started` carries the first held frame's
  seq and time, and its id is the one minted for the hold.
- **A replay or a demo.** `session.discarded`, with the reason, v1's evidence
  sentence and the held frames (`first_seq` to `at`), and nothing else: a
  session still open is ended first, on the frame before the start, and the
  frames after are ignored, by the rules and the detectors too, until the next
  start. The publisher logs one line for it.
- **The feed lost meanwhile.** A loss during a hold or while a replay or a
  demo is ignored is still `telemetry.lost`, but the rules are not told of it,
  as of none of those frames; a hold that becomes a drive opens its session as
  ever, the return told on its own frame.
- **Tests.** The SUGO replay and the five demos of v1's recordings each give
  one `session.discarded` with v1's verdict and evidence, and their goldens
  have no session; the two-lap race's held drive goes through as if never
  held. `testdata/start/` holds a cut of the 2026-10-29 race, where the replay
  the driver watched gives no session and no callout, and
  `make_fixture.py` there says how it was cut.

## Track detection

GT7's telemetry carries no track id. [`internal/track`](internal/track) is the
track table and v1's track detector, ported rule for rule from its
`tracks.py` and `trackdetect.py` at v1.0.0 (paddock#80): a pure library the
race state feeds every frame ([The session's track](#the-sessions-track)).

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

## The session's track

The detector's answer moves frame by frame; the session keeps one, stable
enough to speak and store, and publishes `session.track` each time it changes
(paddock#81), with v1's rules from `Engine._apply_track` in `engine.py` at
v1.0.0, in [`internal/race/track.go`](internal/race/track.go).

- **Fed by the race state.** Every frame goes to the detector, a session open
  or not, but those of a start held by the start check and of a replay or a
  demo after it (as v1's engine). A restart (the car teleported back to the grid) resets it; a menu or
  a loading screen at lap 0 resets it by itself, so a rolling start keeps what
  it saw before the line. A session joined part way tells it so (`Join`).
- **Never downgraded on a blip.** The session's verdict changes only for one
  that ranks higher (confidence, then naming a layout), or when the layout it
  holds has been ruled out by the detector. A change of candidates or wording
  alone publishes nothing.
- **At the session's open.** A verdict the detector already has when a
  session opens (a rolling start crosses the line before lap 1) is applied at
  once, on the session's first frame.
- **The verdict.** `SessionTrackChanged`: the confidence, the layout's id and
  name, the circuit (`family`) when only that is known, the evidence, and the
  best 8 candidates with their fit (rounded to 3 decimals) and whether their
  line was crossed. The detector keeps v1's whole list; the cap of 8 is
  applied as it is published. `at` is the frame's seq, and a frame publishes
  one verdict at most, its last: when the session opens on a verdict and the
  line is crossed on that same frame, v1 logged two changes, and two events
  would share an id, which `EVENTS` drops as a duplicate.
- **At the end.** `session.ended`'s summary carries the session's track:
  when the race ends (the flag, a quit, back to the menu), the detector's
  final candidates and fit replace it when they are the same decision and the
  same circuit, as v1's "final candidates and fit".
- **Not yet carried.** `carried` is always false: a session continued after
  an ecu restart, which starts with the track it had, is paddock#76's.
- **Tests.** On every one of v1's 25 recordings, the `session.track` events
  and each summary's track are v1's: where v1's engine applied a verdict, the
  same verdict (the last of a frame's), its first 8 candidates and its
  evidence, as
  `make_golden.py` in [`internal/race/testdata/track/`](internal/race/testdata/track)
  wrote them from v1.0.0. The six v1 drops as a replay or a GT7 demo
  ([The start check](#the-start-check)) have none.

## The session's kind

GT7 sends no mode. [`internal/race/kind.go`](internal/race/kind.go) is v1's
`SessionDetector` (`sessionkind.py` at v1.0.0), ported as it is, and publishes
`session.kind` each time its decision changes (paddock#82), as v1's
`Engine._apply_kind` applied it.

- **The kind.** A lap total and two or more cars is a lap race, a lap total
  and one car solo laps; no lap total and three or more cars a timed field
  (qualifying or a timed race), one car solo timed (a time trial or
  practice). Two cars make a timed field only once the driver has been
  anything but first, so a ghost doesn't. `total_cars` of -1 (around the
  results, and a whole time trial) says nothing. `field` is true for a lap
  race and a timed field.
- **Fuel use.** On once the level drops 0.05 below the highest seen while
  racing; off after 20 s of driving at 20 km/h or more without a drop, or at
  once with no tank.
- **The start.** The fastest the car goes in the first 18 packets of racing:
  under 30 km/h is a grid start, else rolling in a lap race and flying
  otherwise. A session joined part way leaves it unknown.
- **What counts as a change.** The kind, fuel use, GT7's cars-on-track flag or
  the start. The evidence sentence rides along, but a change of its wording
  alone publishes nothing. Each start resets the detector, so the session's
  first frame always publishes one. The detector is fed every racing frame,
  so a lap total GT7 posts a moment after the start still counts.
- **The event.** `SessionKindChanged`: the verdict as `SessionKind`, the
  frame's seq as `at` and the run's id as `run_id`. The detector decides at
  most once a frame, so a frame publishes one at most. `session.ended`'s
  summary carries the last one as `kind`.
- **Tests.** On every one of v1's 25 recordings, the `session.kind` events
  and each summary's kind are v1's, evidence word for word, as `make_golden.py`
  in [`internal/race/testdata/kind/`](internal/race/testdata/kind) wrote them
  from v1.0.0. The six v1 drops as a replay or a GT7 demo have none.

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
| `TWR_ECU_PAUSE_LIMIT_SECS` | unset: no limit | Seconds a pause may last before it ends the session paused ([Pauses](#pauses)); 0 or more, 0 is no limit. |
| `TWR_ECU_RULES_FILE` | unset: v1's defaults | The rules file, v1's rules YAML ([The callouts](#the-callouts)); `ecu replay` reads it too. |
| `TWR_ECU_TELEMETRY_TIMEOUT_SECS` | `3` | Seconds with no packet that count as the feed lost ([Telemetry lost and restored](#telemetry-lost-and-restored)), above 0 and at most 3,600. |

The ports: UDP 33740 in (the PS5 sends there, to the address the heartbeat
came from), and UDP out to the PS5's 33739. In a pod, 33740/UDP needs a
`hostPort`, as v1's Deployment has.

Not here yet, and still paddock#106's: the listener's state in `/healthz` and
the `live` KV (R7, with paddock#88 and paddock#107), and the metrics (R9).

### Telemetry lost and restored

Live only, a watchdog (paddock#87, v1's `App._watchdog` at v1.0.0) notices when
the packets stop and when they come back, so a quiet headset is never
ambiguous. The race state keeps no clock: it says when the feed counts as lost
if nothing comes (the last packet's receive time plus
`TWR_ECU_TELEMETRY_TIMEOUT_SECS`), the drive waits for a packet no later than
that, and when none came it tells the race state the time
(`race.Machine.Tick`).

- **Lost.** `telemetry.lost`, once, the timeout after the last packet, whatever
  the phase, with the last frame's phase, its seq (`at`) and the silence. The
  telemetry rule decides what is said (paddock#98): "Lost telemetry." when
  the last frame was a race or a pause (`phases`), since silence in a menu is
  normal, and "Telemetry back." on the restore after such a loss. Both are
  priority 5, on `.p0`, and spoken in the pit lane, as v1's watchdog did. The
  first packet after ecu starts is no restore.
- **In a pause.** Lost in a pause with a session open, the session ends
  paused on its last frame: `session.ended` with `END_REASON_TELEMETRY_LOST`
  and `ended_paused` (the PS5 switched off or put to rest,
  those-who-run/those-who-run#229), and the frames are ignored until the next
  start, as v1's `Engine.feed_lost`. Lost while driving, the session stays
  open: the feed may come back.
- **Restored.** `telemetry.restored` on the first packet after the loss, with
  the gap between the two packets' receive times. v1 noticed the return on
  its next once-a-second check; ecu at the packet, as the contract's `at`
  says.
- **Not in a replay.** `ecu replay` never watches the feed, as v1's replay had
  none: the end of a recording is not a loss, and a gap in one is not either.

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
  one that did not take everything by the end of the 30 s (any frame or event
  dropped, which the final `published` log line counts, or a final flush that
  failed), or a bad flag. ecu waits for the connection to close before it
  exits, so nothing is logged after its last line. `130`
  when SIGINT or SIGTERM stops it, the open session ended as ecu stopped; a
  signal while it waits for the server at the end cuts that wait to 1 s.

Not here yet, and still paddock#35's: `--seed` for the ULIDs (session and
event ids are random on each run until then), `--from` and `--to`, and the
one line per session printed at the end.

## Pit stops

GT7 sends no pit flag. [`internal/pit`](internal/pit) finds pit stops from its
camera cuts: v1's `PitDetector` (`pitstop.py` at v1.0.0), ported as it is, a
pure library like the race state (paddock#78). The race state feeds it every
frame and publishes the session's entries, exits and withdrawn entries, as v1's
`Engine._apply_pits`, in [`internal/race/pit.go`](internal/race/pit.go).

- **Entry.** A one-frame loading screen on lap 1 or later after a racing
  frame, or a teleport (over 10 m at over 600 km/h between two frames, timed
  from the packet counter as well as the clock). `pit.entered` carries the
  in-lap (the lap on the frame before the cut), the fuel and position before,
  `entered_t` and `entered_at`, and the evidence.
- **The stop.** The time at rest (under 1 km/h, steps capped at 1 s, pauses
  not counted), the fuel after, new tyres (all four temperatures dropping over
  5 °C in a frame) and the position after.
- **Exit.** A cut or teleport once the car has been at rest and moves at
  5 km/h or more, or any frame over 100 km/h. `pit.exited` carries the stop as
  v1's `PitStop.record()` writes it (fuel to 3 decimals, times to 10 ms, the
  limiter's speed to 0.1 km/h), the stint that starts, and the entry's and
  exit's times.
- **Teleports.** An entry the car leaves without ever coming to rest, with
  under 0.5 litres added and no tyres changed, was no stop: being put back on
  track after an off moves the car too (paddock#79). The detector withdraws
  it, so its stops, stints and lap roles are as they were before the entry,
  and the race state publishes `pit.teleport` instead of `pit.exited`: the
  entry it withdraws (its number and its `pit.entered`'s `at`), the in-lap,
  the evidence and `withdrawn_t`. The next entry takes the stop number again.
- **Before the laps.** The pits are worked out before the lap store reads the
  frame, so a line crossed on the frame after the cut is known to be in the
  pits. A start or a restart resets the detector; a loading cut keeps the
  track detector's verdict.
- **Ids.** `<sid>:pit.entered:<entry>` counts every entry in the session,
  withdrawn ones too, `<sid>:pit.teleport:<entry>` is the entry it
  withdraws, and `<sid>:pit.exited:<stop>` counts the stops (contracts
  v0.2.3). The counts are
  the session's, for paddock#76 to carry across an ecu restart. A frame
  changes the pits once at most, so it gives one pit event at most.
- **At the end.** `session.ended`'s summary carries the stints and the stops,
  a stop still open last, without an out-lap, a lane time or a position after
  (Spa, quit from the box).
- **Lap roles.** Each `lap.completed` carries its pit role (in-lap, out-lap,
  both or none) from the status as last applied to the session, as v1's
  store and rules read it ([The laps](#the-laps)).
- **Not yet.** The summary has no teleports: v1's `summary.json` stored none, and contracts v0.2.3's
  `SessionSummary` has no field for them.
- **Tests.** On every one of v1's 25 recordings, the detector's changes are
  v1's, field for field, and the `pit.entered`, `pit.exited` and
  `pit.teleport` events and each summary's stops are what v1's engine applied
  and stored, as `make_golden.py` in
  [`internal/race/testdata/pit/`](internal/race/testdata/pit) wrote them from
  v1.0.0. None of the recordings has a teleport, in v1 or here; a synthetic
  jump on lap 3 checks one. The Gilles Villeneuve stop is also `contracts`'
  `testdata/v1/gilles_villeneuve_pit_stop.json`.

## The finish

[`internal/race/finish.go`](internal/race/finish.go) publishes
`race.finished` once a session at most, where v1's engine set `finished`
(paddock#85).

- **By the flag.** A race with a lap total shows the flag in the lap counter
  (lap total + 1): the first finished frame is the finish (`FINISH_KIND_FLAG`),
  solo laps included, as v1 counted them finished.
- **Timed.** A timed race has no flag. v1's `Engine._timed_finish`, ported as it
  is: in a timed session with a field and GT7's cars-on-track flag, after a
  lap time is posted, throttle and brake both 0 at over 20 km/h for 3 s is GT7
  driving the car after the line (`FINISH_KIND_TIMED`), as of that line
  crossing. Inputs within 0.5 s of the line are still the driver's; after that
  the driver is on another lap. A paused car is not coasting.
- **The event.** `RaceFinished`: the lap completed at the line, the position
  there (left out outside a field, as for a lap), the total of the complete
  laps' times, the session's best lap as the last `lap.best` gave it
  ([The laps](#the-laps)), and the seq of the
  flag frame or of the crossing as `at`. Its `ce-time` is the frame it was
  decided on: 3 s after the crossing for a timed finish.
- **Where in a step.** v1's `Engine._step` decides the finish after the pits
  and the rules, before the track, the kind and the lap store see the frame,
  reading the kind from before the frame. ecu takes the kind from before the
  frame too, and publishes `race.finished` after the lap store, so the final
  lap's `lap.completed`, which closes on the flag frame, comes first and is
  counted in the total, then `session.ended`.
- **The end.** The session's summary is `finished`, with `total_time_ms`, and
  the end that follows is `END_REASON_FINISHED` or `END_REASON_TIMED_FINISH`.
  The frames GT7 replays after the results start no session and finish
  nothing. A session that ends any other way after its finish (the input
  ending, ecu stopping or a new start during GT7's coast to the results) is
  still `finished`, with its total time: a deliberate departure from v1.0.0,
  which stored it as not finished. ecu has published `race.finished` for it,
  and the contract says its summary is then finished.
- **Tests.** On every one of v1's 25 recordings, `race.finished` and each
  summary's `finished` and total time are v1's, as `make_golden.py` in
  [`internal/race/testdata/finish/`](internal/race/testdata/finish) wrote them
  from v1.0.0, the six v1 opens no session on with every start taken for a
  drive (SUGO's replay then finishes at the flag, as v1's engine would).

## Pauses

[`internal/race/pause.go`](internal/race/pause.go) is v1's `PauseClock` and
the pause half of `Engine.process` (paddock#77). A pause is not driving time,
and never ends a session on its own.

- **Two shapes.** GT7 flags most pauses: its frames are paused for as long as
  the pause menu is up. From the pause menu it can send a frozen stream
  instead: frames marked in the race that do not change at all (position,
  velocity, speed, revs, pedals, fuel, lap, rotation and spin), the car
  stopped in mid-air at racing speed. Identical for 1 s with the car over
  1 km/h, a frame is read as paused, and the pause is dated from the first
  identical frame. Below 1 km/h identical frames are a car standing still.
- **First in the step.** The frame is read before anything else sees it: it
  goes out as `PHASE_PAUSED`, and the start check, the detectors, the rules
  and the lap store read it so: a frozen stream inside a start's hold settles
  it as a flagged pause does ([The start check](#the-start-check)). No rule evaluates on a paused frame, so
  nothing is said in a pause, and the frame after it is compared with the
  last driving frame (`Bridged` `PHASE_PAUSED`), as v1's rules had it.
- **The events.** `session.paused` on the frame a pause is read on, in an open
  session, with how it began (`PAUSE_KIND_FLAGGED`, or `PAUSE_KIND_FROZEN`
  for a frozen stream, read 1 s after its first frame), its start on the
  frames' clock and in UTC, and the session's time paused before it;
  `session.resumed` on the first frame after it, with its length and the
  session's time paused so far. A pause is numbered from 1 in the session.
  A pause that GT7 flags and then freezes, as SUGO 2026-10-18's 45 s before
  a restart does, is one flagged pause, as v1 read it. A pause the session
  ends in has no `session.resumed`; one that ends on a frame that also ends
  the session (a quit or a restart from the pause menu) has, before the end.
- **The summary.** `paused_ms` is the session's time paused, on the frames'
  clock, so a replay gives the same totals. `driving_ms` and `ended_at` are
  paddock#86's.
- **Ending paused.** The session ends paused (`ended_paused`), on its last
  frame, when the feed is lost during the pause (the watchdog's
  `Machine.Tick`, live only, paddock#87: `END_REASON_TELEMETRY_LOST`), a
  frozen pause as a flagged one, when the
  frames stop in it (`END_REASON_INPUT_ENDED` at the end of a recording,
  `END_REASON_ENGINEER_STOPPED` when ecu stops), or once a pause has lasted
  `TWR_ECU_PAUSE_LIMIT_SECS` (`END_REASON_PAUSE_LIMIT`, on the frame the
  limit passes; off by default). The frames after it are ignored until the
  next start: only the frame goes out, and no rule sees it.
- **Tests.** On every one of v1's 25 recordings, the pauses read in an open
  session, their frames and times, and each summary's `paused_ms` and
  `ended_paused` are v1's, as `make_golden.py` in
  [`internal/race/testdata/pause/`](internal/race/testdata/pause) wrote them
  from v1.0.0. No callout is raised on a
  paused frame of any of them. A slice of the live race of 2026-10-29,
  which `make_fixture.py` there cut, holds a pause GT7 flags and then
  freezes before a quit: two pauses, flagged then frozen, as v1 read them.

## The laps

[`internal/race/laps.go`](internal/race/laps.go) is the lap model: what each
lap was, decided once for every rule and subscriber (paddock#83).

- **Where a lap closes.** Where v1's store closed it: on the lap counter's
  bump, seen against the last driving frame across a pause or a loading
  screen, so a lap posted in a pit cut-scene closes on the frame after it.
  GT7 posts the lap's time on the bump frame, which is also where v1's
  `lap_time` rule took it: on all 25 recordings every complete lap v1 stored
  was posted on its bump, with the stored time. The one post without a bump,
  SUGO's replay jumping to the results, is for a lap v1 never stored, and
  closes nothing here either.
- **`lap.completed`.** The lap, its time, complete, the pit role
  ([Pit stops](#pit-stops)), `counts_for_best`, the fuel at its start and end
  and used, the position, and its first and last seq. A lap counts for the
  best as v1's `lap_time` rule decided: a timed lap with no pit role, but not
  a start lap GT7 leaves out of its best (lap 1 of a session with a lap total,
  and lap 1 of a timed one posted with GT7's best still blank).
- **`lap.best`.** Published when a lap's end changes the session's best, once
  per lap at most, with the best before it and the gap. The best is GT7's own
  when it has one and a lap of the session has that time (GT7 keeps its best
  across sessions at a track), else the quickest lap that counts.
  `race.finished` and the summary's `best_lap` and `best_lap_time_ms` are the
  last `lap.best`'s, so none of them can disagree.
- **Not v1's in one way.** Without GT7's best on a lap, v1's stored best
  (`results.best_lap`) was the quickest complete lap, counted or not, lap 1
  of a lap race left out only when another lap was timed. ecu never takes a
  lap that does not count, as contracts v0.2.3 says; on the 25 recordings
  that leaves two sessions whose only timed lap is lap 1 with no best, where
  v1 stored lap 1.
- **The summary.** Also `fastest_lap_ms` (the quickest complete lap of any
  kind) and `game_best_lap_ms` (GT7's own best, as last reported), as v1
  stored them.
- **Where in a step.** v1's `Engine._step` ran the `lap_time` rule after the
  pits, and its store after the track and the kind. ecu decides a lap's role,
  whether it counts and the best at the store's place, after the pits, the
  track and the kind, and before the finish; the rule read nothing those two
  change in between.
- **Tests.** On every one of v1's 25 recordings, each `lap.completed`'s frame,
  lap, time, complete, pit role and `counts_for_best`, and each summary's
  best, fastest and GT7's best laps, are v1's but for the difference above,
  as `make_golden.py` in [`internal/race/testdata/lap/`](internal/race/testdata/lap)
  wrote them from v1.0.0 with v1's `lap_time` rule, and the last `lap.best`
  of each session is the summary's best.

## The final lap and the position

[`internal/race/position.go`](internal/race/position.go) publishes the
decisions of v1's `FinalLapRule` and `PositionRule` (`rules/final_lap.py` and
`rules/position.py` at v1.0.0) as facts (paddock#84). What is said about them
is the callouts' (paddock#93, #94).

- **The final lap.** `race.final_lap` once a race at most, on the frame the
  lap counter reaches the lap total. A timed race has no lap total, so none.
- **The position.** `race.position` when the position has held a new value for
  3 s, with the one before it (the last published, or the grid slot) and the
  frame it settled on as `at`. It is v1's rule with only its 3 s settle: the
  15 s start grace and the 10 s between calls are the callout's. The race's
  first 3 s follow the grid slot quietly (GT7 reports the slot a frame or two
  late), a frame with no position (-1 or 0) is passed over, and there is none
  without a field: once the kind is known, a field GT7 flags with cars on
  track (a time trial's ghost is flagged but no field); on a start's first
  frame, before it, the flag and more than one car (v1's `has_field`).
- **In the pit lane.** The position is followed but not published, and settles
  again from the exit, so the first change after it is the net change against
  the last place published.
- **Where in a step.** v1's `Engine._step` runs its rules after the pits and
  before the timed finish, the track, the kind and the lap store, against the
  same frame the lap store compares with, the kind from before the frame and
  the pit lane after it. ecu does the same, so both come after a pit entry or
  exit and before a change of track or kind and the lap the same crossing
  closes: `race.final_lap` comes before the `lap.completed` of the lap before.
- **Every settled change, not every call.** A v1 call can name a place held
  under 3 s: while it waits out its grace or interval, its settle timer runs on
  through every place that differs from its last call. Each v1 call has a
  `race.position` at or before it, though not always of the place called: of
  the six such calls on v1's recordings, four are published a few frames
  later, and two never are, the place changing again before it held (SUGO
  restarts: P18 called on frame 234, `race.position` P16 to P19 on 238; SUGO
  two stops: P13 on 823, P12 to P14 on 827).
- **Not yet.** `live`'s current position is paddock#88's.
- **Tests.** On every one of v1's 25 recordings, `race.final_lap` and
  `race.position` are v1's rules' decisions, frame for frame, as
  `make_golden.py` in
  [`internal/race/testdata/final-lap-position/`](internal/race/testdata/final-lap-position)
  wrote them from v1.0.0 (the position with v1's settings for the event, and
  v1's calls with `rules.example.yaml`'s), the six v1 opens no session on with
  every start taken for a drive.

## The callouts

[`internal/rules`](internal/rules) is the rule engine (paddock#89): v1's
built-in rules run in v1's order on every frame, against one read-only
`race.Context` the machine fills on each step, and what they raise is published
as contracts' `Callout` on `twr.v1.<src>.callout.p0`, `.p1` or `.p2`, by
priority (p0 under 10, p1 10 to 19, p2 20 and over). ecu decides each
callout's kind and slots, priority, hold, expiry and pit-lane policy; radio
only phrases and speaks it. Its [README](internal/rules/README.md) says how
the context stands for v1's hooks, what the engine stamps, and how to add a
rule.

- **Rules.** `lap_time` (paddock#91), `fuel` ([Fuel](#fuel), paddock#92),
  `final_lap` (paddock#93), `position` (paddock#94), `start_brief`
  (paddock#95), `race_summary` (paddock#96), `temps` (paddock#97) and
  `telemetry` ([Telemetry lost and restored](#telemetry-lost-and-restored),
  paddock#98) so far, each a package under `internal/rules`.
- **Car alarms.** Each firing of `temps` (a tyre, the water or the oil hot, or
  cold tyres at the start) is also published as `car.alarm` (contracts'
  `CarAlarmRaised`) on `twr.v1.<src>.evt.<sid>.car.alarm`, from the same
  decision as the callout, just before it, whatever the pit lane does with the
  callout. Its id is `<src>:<sid>:car.alarm:<check>:<run>:<seq>`.
- **Where in the step.** The context holds each value as v1's rules saw it in
  `Engine._step` (after the pits, before the timed finish, the track, the kind
  and the lap store), plus the lap closed, the finish and the end of the
  session from later in the step, so the engine runs once `Step` returns.
- **The rules file.** `TWR_ECU_RULES_FILE`, v1's rules YAML: v1's
  `rules.example.yaml` loads unchanged. ecu reads `queue.max_age_seconds` and
  each rule's `enabled`, `priority`, `max_age_seconds`, `hold_secs`,
  `pit_lane` and own thresholds, and ignores the templates, the script and the
  voice, which are radio's. A `pit_lane` other than `speak`, `hold` or `drop`,
  or a value of the wrong type, stops ecu at start.
- **Times.** `created_at` is when ecu received the frame (the replay's clock
  in a replay); `expires_at` is always set, `created_at` plus the rule's
  `max_age_seconds` or the queue's 8 s; `hold_until` is `created_at` plus
  `hold_secs`. The id is `<src>:<run>:callout:<seq>:<rule>:<n>`, so a replay
  with the same run id repeats its ids.
- **The pit lane.** From the entry's frame to the exit's, a callout of a
  `speak` rule is published, a `drop` one never is, and a `hold` one is
  published at the exit, an entry withdrawn, or the finish in the lane, as
  raised then (contracts' `PitLane`).
- **A rule that panics** is recovered, logged with its name and counted, and
  the other rules carry on.
- **Tests.** `internal/rules/testdata/callouts` holds every callout v1.0.0
  raised on each of the 25 recordings, written by its `make_golden.py`; the
  callouts of every registered rule are compared with it. A callout ecu raises
  that v1 did not, until an open issue fixes it, is listed with the issue
  (`pending` in its `golden_test.go`), and must still be raised.

## Fuel

[`internal/fuel`](internal/fuel) is the fuel model, and
[`internal/rules/fuel`](internal/rules/fuel) the `fuel` rule that reads it
(paddock#92): v1's `FuelRule` and `FuelStints` (`rules/fuel.py` and
`stints.py` at v1.0.0), with the arithmetic in one place.

- **The model.** Use per lap is the drop in level between two line crossings,
  averaged over the last `rolling_laps` (3). Left out: the in-lap and out-lap
  of a stop, a lap crossed inside a telemetry gap over `max_gap_secs` (1 s, not
  a pause) and the lap after it, the lap a session was joined on, and a lap the
  level rose over by more than `refuel_epsilon` (a stop the pit detector
  missed). A completed stop redoes laps remaining at once from the level after
  it. Fuel used per stint and in total adds up every drop, as v1's
  `fuel_burned` did.
- **The calls.** Laps remaining at the thresholds (10, 5, 3, 2, 1), as whole
  laps rounded up within `round_up_laps` (`template_short` when rounded),
  "Box next lap." and "Pit this lap." at priority 5 ahead of the number,
  "Fuel critical." under one lap, each once per stint; only while the estimate
  is short of a lap race's laps left, or a timed race. When it covers them,
  `template_tight` once per stint under half a lap of margin, and
  `template_last_lap_enough` as the final lap starts after a warning. Nothing
  on a crossing in the pit lane or seen through a loading screen, or past the
  flag. They are spoken in the pit lane (`pit_lane: speak`).
- **The gauge.** The rule shows the model's laps remaining as
  `gt7_fuel_laps_remaining` (`rules.Gauge`), for paddock#100 and the `live`
  KV (paddock#88).
- **Tests.** On every one of v1's 25 recordings the rule's callouts are
  v1.0.0's, frame for frame, with kind, laps, priority, hold, expiry and pit
  lane, as `make_golden.py` in
  [`internal/rules/fuel/testdata/`](internal/rules/fuel/testdata) wrote them,
  the six v1 opens no session on with every start taken for a drive.

## Publishing

[`internal/bus`](internal/bus) puts the race state's outputs on NATS
(paddock#34), and is the only package that imports nats.go (depguard in
[`.golangci.yml`](.golangci.yml)).

| Subject | What | Captured by |
| --- | --- | --- |
| `twr.v1.<src>.frame.gt7` | every frame | `TELEMETRY` |
| `twr.v1.<src>.frame.gt7.lo` | the same frames, at most one per 50 ms of frame time (20 Hz) | none |
| `twr.v1.<src>.evt.<sid>.session.started` | a session began | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.track` | the session's track verdict changed | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.kind` | the session's kind verdict changed | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.paused` | the session paused, flagged or frozen | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.resumed` | the session's pause ended | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.pit.entered` | the car came into the pit lane | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.pit.exited` | the car left the pit lane, with its stop | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.pit.teleport` | a pit entry was no stop, and is withdrawn | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.race.final_lap` | the final lap of a lap race began | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.race.position` | the car's position changed and settled for 3 s | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.lap.completed` | a lap ended | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.lap.best` | the session's best lap changed | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.race.finished` | the race reached its finish, by the flag or timed | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.ended` | a session ended | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.session.discarded` | a held start was a replay or a demo, not a drive | `EVENTS` |
| `twr.v1.<src>.evt.<sid>.telemetry.lost`, `.telemetry.restored`, or `evt._.` with no session open | the packets stopped for the timeout, or came back (live only) | `EVENTS` |
| `twr.v1.<src>.callout.p0`, `.p1`, `.p2` | a callout, on its priority's level ([The callouts](#the-callouts)) | `CALLOUTS` |

- **Messages.** Each is the `contracts` message in protobuf
  JSON, with every CloudEvents header from `contracts`' `ce` package:
  `ce-specversion`, `ce-id`, `ce-type`, `ce-source` (`/twr/ecu/gt7`), `ce-time`
  (when ecu received the packet) and `ce-datacontenttype`, and `Nats-Msg-Id`
  equal to `ce-id`. The ids follow the `msg_id` schemes of `contracts`'
  `subjects.yaml` (v0.2.3, paddock#279): a frame's is `<run>:frame:<seq>`, and
  an event's is `<src>:<sid>:session.started:1`,
  `<src>:<sid>:session.track:<run>:<seq>` and
  `<src>:<sid>:session.kind:<run>:<seq>` (the run and the frame it changed
  on; the run is the message's `run_id` too), `<src>:<sid>:pit.entered:<entry>`,
  `<src>:<sid>:pit.exited:<stop>`, `<src>:<sid>:pit.teleport:<entry>`,
  `<src>:<sid>:race.final_lap:1`, `<src>:<sid>:race.position:<run>:<seq>`,
  `<src>:<sid>:lap.completed:<n>`, `<src>:<sid>:lap.best:<n>` (the lap whose
  end changed the best), `<src>:<sid>:race.finished:1`,
  `<src>:<sid>:session.paused:<pause>`, `<src>:<sid>:session.resumed:<pause>`,
  `<src>:<sid>:session.ended:1` or `<src>:<sid>:session.discarded:1`, with
  the subject's `<src>`, so two replays
  under different sources are not deduplicated against each other.
  `telemetry.lost` and `telemetry.restored` are
  `<src>:<sid>:telemetry.lost:<run>:<seq>` with a session open, and go on
  `evt._` with none, as `<src>:<run>:telemetry.lost:<seq>`. A test checks every id
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
- **Callouts** go the same way as events, in the same queue, into
  `CALLOUTS`, with the id `<src>:<run>:callout:<seq>:<rule>:<n>` in the
  message, `ce-id` and `Nats-Msg-Id`. A callout that is not acknowledged is
  sent again only until its life is over (`expires_at` less `created_at`,
  counted from when the step handed it on, so a replay dated in the past still
  gets it): then it is dropped, counted and logged, since radio would drop it
  unspoken (paddock#73's decision). `ecu replay` exits 2 when a callout was
  dropped.
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
  source at start, and the frames, 20 Hz frames, events and callouts published
  and dropped at the end, with `"final": true`. When a write still
  holds the publisher once the 5 s are up, that line has `"final": false`,
  ecu closes the connection under the write so it gives up at once, and waits
  up to 3 s more for the line with the final totals.
- **Tests.** The tests run the `nats-server` release paddock pins, downloaded
  once into the user's cache and checked against its sha256 (or the binary
  `TWR_TEST_NATS_SERVER` names), with the `TELEMETRY`, `EVENTS` and `CALLOUTS` definitions
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
