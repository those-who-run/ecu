# The rules

ecu's rule engine (paddock#89): v1's built-in rules in Go, run in v1's order on
every frame against one read-only frame context, and published as callouts on
`twr.v1.<src>.callout.p0`, `.p1` and `.p2` (contracts v0.2.3's `Callout`).
**ecu decides** which callouts are raised, their kind and slots, priority,
hold, expiry and pit-lane policy. radio only phrases and speaks them.

## Layout

- `internal/rules` is the engine: the `Rule` interface, `Callout`, the
  settings and the rules file (`config.go`), and `Engine` (`engine.go`).
- Each rule is **a package of its own**, `internal/rules/<name>` without the
  underscore: `laptime`, `fuel`, `finallap`, `position`, `startbrief`,
  `racesummary`, `temps`. Its types can't collide with another rule's.
- `internal/rules/all` imports every rule's package, so a program gets them all
  with one blank import.
- Everything here is under `internal/` for now (the maintainer's default). The
  pure race-state packages may move out of `internal/` later (paddock#89's
  Rosberg triage), all together. That move is cheap, so it doesn't block a rule.
- No I/O: no NATS, no network, no files, no clock. A rule reads the context and
  returns callouts. It never logs, publishes or calls `time.Now`.

## How to add a rule

1. Make `internal/rules/<name>/<name>.go`, with a package comment that names
   v1's file and the paddock issue. Register the rule from a package-level var,
   with its name from `rules.Order`, v1's key in the rules file:

   ```go
   var _ = rules.Register("final_lap", func(s *rules.Settings) rules.Rule { return &rule{} })
   ```

   The builder runs once at start-up. It reads the rule's own settings with
   `s.Float`, `s.Int`, `s.Bool`, `s.String`, `s.Floats` or `s.Strings` (key,
   v1's default), or `s.Decode(&cfg)` into a struct with `yaml` tags named for
   v1's keys. They read null as not set; `s.Null(key)` says the file set it to
   null, for a setting v1 read null as off (temps' thresholds). The defaults are v1's `cfg.get(..., default)`. A value of the
   wrong type stops ecu at start-up, naming the rule and the key. The
   settings every rule takes (`enabled`, `priority`, `max_age_seconds`,
   `hold_secs`, `pit_lane`) are the engine's: `config.go` holds their v1
   defaults, so a rule never reads them.
2. Add its line to `internal/rules/all/all.go`.
3. Implement `rules.Rule`:
   - `Reset()` is v1's `reset`: forget the race. It's called on every start,
     before the start's frame is evaluated.
   - `Evaluate(ctx *race.Context) []rules.Callout` is called **on every frame**,
     in a race or not. Put v1's `evaluate` body under `if ctx.Racing`. Handle
     v1's hooks from the context fields below on any frame, because a pit cut
     can come on a loading frame.
4. Test it against v1 (see [Tests](#tests)). Copy `finallap`, the worked example.

Don't touch `internal/race/machine.go` or the engine. If the context lacks
something your rule reads, say so on paddock#89 and don't work around it.

## The context: v1's hooks and state

`race.Context` (`internal/race/context.go`) is filled by the machine on each
`Step`, at v1's place for the rules in `Engine._step`: after the pits, before
the timed finish, the track, the kind and the lap store. What v1 told the
rules later in the step (the lap closed, the finish, the end) is filled in as
of the end of the step, so a rule sees it on the same frame v1's did.

| v1 | `race.Context` |
| --- | --- |
| `state` (RaceState) | `ctx.Cur` (`race.View`): seq, `T`, phase, lap, total laps, GT7's last and best lap, fuel, the raw position (`Position`, -1 outside a race; `RacePosition()` 0 for none), total cars, speed, cars-on-track flag, throttle and brake, `Time`, and `Frame` (every field GT7 sent: temperatures, etc.) |
| `prev` passed to `evaluate` (`seen`) | `ctx.Prev`: the last driving frame across a pause or loading screen; nil before the first frame |
| `on_bridge(phase)` | `ctx.Bridged`: `PHASE_LOADING` or `PHASE_PAUSED` when `Prev` bridged such frames |
| evaluate is called (`in_race or racing_frame(seen, state)`) | `ctx.Racing` |
| `lap_completed`, `lap_time_posted`, `racing_frame` | `race.LapCompleted(ctx.Prev, &ctx.Cur)` and the others |
| `posted_lap` with the time, deduped by the rule, and race_summary's `_start_lap` | `ctx.Posted` (`Lap`, `TimeMs`, `StartLap`): every frame a time posts, closed lap or not (SUGO replay frame 864). The rule keeps the first per lap, as v1's did |
| `self.joined` | `ctx.Joined` |
| `self.session` / `on_session` | `ctx.Kind` (`Type`, `Field`, `CarsOnTrack`): the kind **before** this frame |
| `has_field(state)` | `ctx.HasField()`, or `ctx.HasFieldOn(v)` for another frame (the timed finish's crossing) |
| `on_track(match)` | `ctx.Track`, the session's verdict at the rules' place, and `ctx.TrackChanges`, each verdict applied since the frame before, oldest first, as v1 called `on_track` for each |
| `self.pits` / `on_pit` | `ctx.Pits` (`pit.Status` as last applied to the open session: `InPitLane`, `Stops`, `Stint()`, `IsPitLap`, `LapRole`) and `ctx.PitChange` (entry, exit, entry withdrawn on this frame). Both are zero with no session open |
| the lap the store closed | `ctx.Lap` (`race.ClosedLap`: lap, time, pit role, counts for the best, position), the complete lap closed on this frame |
| lap_time's `prev_best` / `own_best_ms` | `ctx.PrevBestMs()`: GT7's raw best on `Prev` (it can be an earlier session's), else `ctx.QuickestCountingMs()`, the quickest counting lap closed before this frame. **Not** `lap.best`. `ctx.TimedLaps()` lists those laps |
| FinalLapRule's decision | `ctx.FinalLap` (`race.final_lap` on this frame) |
| PositionRule's 3 s settle | `ctx.Settled` (`race.position` on this frame). The position callout's grace and interval run on the raw `ctx.Cur.Position`, because 2 of v1's 109 calls name a place that never settles (paddock#84, AC4) |
| `Engine.finished` | `ctx.Finished`: the race has finished, sticky until the next start |
| first `FINISHED` frame, and `on_finish(crossing)` | `ctx.Finish` (the frame it finished as of: the flag frame, or the timed finish's line crossing) and `ctx.FinishKind`, on the frame the finish is found, where `race.finished` is |
| `on_end(last, state)` | `ctx.EndedEarly`: the last driving frame, when the session ended on this frame without a finish (quit, menu). It isn't set on a restart, a new start or the end of the input, as in v1 |

Not in the context yet: the telemetry watchdog's loss and return (#87, for
#98), and the continued session after an ecu restart (#76, which will set
`ctx.Joined`).

## What the engine stamps

A rule returns `rules.Callout{Kind, Slots, Priority, AsOf}`. `Kind` is the
contract's script key ("The kinds ecu sends, by rule" in `callout.proto`), and
`Slots` are typed values, never words. The engine makes the contract's
`Callout`:

- `rule` is the registered name, and `priority` is `Callout.Priority` or the
  rule's configured one. The subject level comes from the priority: p0 under
  10, p1 10 to 19, p2 20 and over.
- `at` is the frame raised on, and `run_id` and `session_id` come from the
  context. `id` is `<src>:<run>:callout:<seq>:<rule>:<n>`, where n counts the
  rule's callouts on the frame from 1, so replays are reproducible.
- `created_at` is the frame's receive time, or `AsOf.Time` when set (the
  race summary at a timed finish is created as of the crossing, as in v1).
- `expires_at` is always set: `created_at` plus the rule's `max_age_seconds`,
  or the queue's (8 s).
- `hold_until` is `created_at` plus `hold_secs`, when there is one.
- A callout with `Callout.Alarm` (temps, paddock#97) has its `car.alarm`
  event (contracts' `CarAlarmRaised`) just before it, on the frame raised on:
  the engine sets its `session_id`, `at` and `run_id`, and its id
  `<sid>:car.alarm:<check>:<run>:<seq>`. It is published at once, whatever the
  pit lane does with the callout, and not with no session open.
- `pit_lane` is the rule's. **In the pit lane** (from the entry's frame to the
  exit's) the engine applies it, as v1's `_quiet` did: `speak` is published,
  `drop` is never published, and `hold` is kept until the exit, the entry
  withdrawn, or the finish in the lane. It's then published as raised there,
  with a new `created_at` and `expires_at` and no `hold_until`, keeping its
  `at` and `id`. The policy applies on the flag frame and after it too, and a
  callout held then is released on the same frame. A session that ends in the
  lane without a finish never publishes what was held. Callouts queued before the entry are radio's to handle
  (paddock#59).

Order within a frame: the released callouts, then each rule's in
`rules.Order` (v1's registry order: lap_time, fuel, final_lap, position,
start_brief, race_summary, temps), each in the order raised. v1 ran
`on_finish` and `on_end` after every rule's `evaluate`; here each rule raises
everything in one `Evaluate`. Rules that read the closed lap see it on the
frame it closes, against `race.final_lap` and `race.position`. The parity diff
matches callouts by rule and frame, so order within a frame doesn't matter
there.

A rule that panics is recovered, logged with its name and counted
(`Engine.Stats`), and the other rules and the frame carry on. A rule with a
value to show between callouts (fuel's laps remaining) implements
`rules.Gauge`.

## Tests

- `testdata/callouts/<recording>.jsonl` holds **every callout v1.0.0 raised**
  on each of the 25 recordings, every rule, with v1's defaults. It's written by
  `testdata/callouts/make_golden.py` in a v1.0.0 env. Each line has the
  session, the frame, the rule, the kind (contracts' table, as paddock's
  `parity/v1events.py` names it), priority, pit lane and outcome. The outcome
  is `spoken`, `held` (with the frame it was released on), `dropped` (queued at
  an entry, which radio drops) or `unpublished` (raised in the lane and
  dropped, or held and never released).
- `TestEveryCalloutIsV1sOnEveryRecording` (`golden_test.go`) replays each
  recording through the machine and the engine, and compares the callouts of
  **every registered rule** with the golden's lines for that rule. A new rule
  is checked as soon as it registers, and nothing needs regenerating.
- Slots aren't in the shared golden: v1 phrased words, not numbers. A rule
  checks its slots in its own package's tests, with goldens of its own
  (`internal/rules/<name>/testdata/`) where it needs them.
- Known differences from v1 are listed in the golden test's `accepted` map, each
  with the paddock issue that decided it. Callouts ecu raises that v1 did not,
  until an open issue fixes them, are in its `pending` map with that issue
  (the frozen pause, paddock#77), and the test fails once one is no longer
  raised, so the fix deletes its line.
