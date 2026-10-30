"""Write the goldens summary_test.go compares with: each session's summary.json as
v1 wrote it, as JSON lines (those-who-run/paddock#86).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay --clock 2026-01-01T00:00:00` does, with
no rules, into a store of its own, the store's clock starting on the first frame
and moving with the frames' own times (v1's RecordingClock), as the tests date
the frames. One line per session v1 stored, in order: the fields of its
summary.json that SessionSummary carries, with started_at and ended_at in epoch
seconds and each lap's file and recorded_at left out (ecu has no lap files).
Floats are written as Python's repr, so they read back as the same float64.
"""

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
)

LAP_FIELDS = ("lap", "lap_time_ms", "complete", "fuel_start", "fuel_end", "fuel_used", "position", "pit")


class Clock:
    def __init__(self) -> None:
        self.now = EPOCH

    def __call__(self) -> float:
        return self.now


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    clock = Clock()
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp), clock=clock)
        engine = Engine([], CalloutQueue(max_age=None), store)
        end = store.end_session

        def end_session(state, **kw):
            out = end(state, **kw)
            if out is not None:
                summary = json.loads(Path(out).read_text())
                line = {k: summary[k] for k in FIELDS}
                line["pit_stops"] = len(summary["pit_stops"])
                line["laps"] = [{k: lap[k] for k in LAP_FIELDS} for lap in summary["laps"]]
                print(json.dumps(line))
            return out

        store.end_session = end_session
        t0 = states[0].t if states else 0.0
        for state in states:
            clock.now = EPOCH + (state.t - t0)
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    main(Path(sys.argv[1]))
