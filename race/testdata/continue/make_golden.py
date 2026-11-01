"""Write the golden continue_test.go compares with: what v1 stored for Suzuka's
restart pair (those-who-run/paddock#76), as JSON lines.

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), with the pair's two recordings:

    python3 make_golden.py BEFORE JOINED > restart-pair.jsonl

BEFORE is real-nuc-suzuka-before-restart-2026-10-18.jsonl.gz, the old pod's
view; JOINED is real-nuc-suzuka-joined-mid-race-2026-10-18.jsonl.gz, the new
pod's. Each scenario replays BEFORE into a store of its own, as
testdata/summary/make_golden.py does (no rules, v1's RecordingClock from
2026-01-01T00:00:00Z), closes the engine as v1's shutdown does
(close_session(shutdown=True)), then opens a second store and engine on the
same directory, as a restarted engineer does, and replays JOINED from GAP
seconds after BEFORE's last frame. Once a second of the clock, and once more
before JOINED's first frame, it lets go of the cards held past the window
(release_held, as v1's watchdog does). One line per scenario: its name and each
summary v1 let go of (on_end), in order, with the fields summary_test.go's
golden has plus gaps.

The scenarios (paddock#76's ACs):
  continue     the pair 8.7 s apart, the window 600 s (AC1)
  another-car  the same with JOINED's car code changed (AC2)
  late         the same 11 min apart (AC3)
  window-0     the same with the window 0 s (AC4)
"""

import dataclasses
import json
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.store import SessionStore

EPOCH = datetime(2026, 1, 1, tzinfo=timezone.utc).timestamp()
GAP = 8.7
LATE = 11 * 60.0

FIELDS = (
    "started_at",
    "ended_at",
    "driving_ms",
    "paused_ms",
    "ended_paused",
    "car_code",
    "car_name",
    "total_laps",
    "laps_completed",
    "best_lap_ms",
    "fastest_lap_ms",
    "finished",
    "joined_on_lap",
    "total_time_ms",
    "final_position",
    "total_cars",
    "fuel_burned",
    "fuel_burned_by_stint",
    "fuel_stints",
    "gt7_best_lap_ms",
    "fuel_capacity",
    "fuel_at_start",
    "fuel_at_end",
    "fuel_used",
    "stints",
    "gaps",
    "engineer_shutdown",
)

LAP_FIELDS = ("lap", "lap_time_ms", "complete", "fuel_start", "fuel_end", "fuel_used", "position", "pit")


class Clock:
    def __init__(self) -> None:
        self.now = EPOCH

    def __call__(self) -> float:
        return self.now


def line(summary):
    out = {k: summary.get(k) for k in FIELDS}
    out["pit_stops"] = len(summary["pit_stops"])
    out["track"] = (summary.get("track") or {}).get("name")
    out["laps"] = [{k: lap[k] for k in LAP_FIELDS} for lap in summary["laps"]]
    return out


def scenario(name, before, joined, gap, window, car=None):
    clock = Clock()
    ended = []
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp), clock=clock, on_end=lambda s: ended.append(line(s)),
                             continue_window_secs=window)
        engine = Engine([], CalloutQueue(max_age=None), store)
        t0 = before[0].t
        for state in before:
            clock.now = EPOCH + (state.t - t0)
            engine.process(state)
        engine.close_session(shutdown=True)
        store.close()
        last = clock.now

        store = SessionStore(Path(tmp), clock=clock, on_end=lambda s: ended.append(line(s)),
                             continue_window_secs=window)
        engine = Engine([], CalloutQueue(max_age=None), store)
        t1 = joined[0].t
        tick = last
        for state in joined:
            now = last + gap + (state.t - t1)
            while tick + 1 <= now:
                tick += 1
                clock.now = tick
                store.release_held()
            clock.now = now
            store.release_held()
            if car is not None:
                state = dataclasses.replace(state, car_code=car)
            engine.process(state)
        engine.close_session()
        store.close()
    print(json.dumps({"scenario": name, "ended": ended}))


def main(before_path: Path, joined_path: Path) -> None:
    before = list(read_recording(before_path, strict=True))
    joined = list(read_recording(joined_path, strict=True))
    scenario("continue", before, joined, GAP, 600.0)
    scenario("another-car", before, joined, GAP, 600.0, car=joined[0].car_code + 1)
    scenario("late", before, joined, LATE, 600.0)
    scenario("window-0", before, joined, GAP, 0.0)


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
