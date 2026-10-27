"""Write the goldens race_test.go compares with: v1's stored sessions and laps for
one recording, as JSON lines (those-who-run/paddock#33).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), for each recording in
salsa's testdata/v1/, with v1's own replay into a store of its own:

    those-who-run replay tests/fixtures/NAME.jsonl.gz --store STORE \
        --clock 2026-01-01T00:00:00
    python3 make_golden.py STORE > NAME.jsonl

The replay needs v1's `engineer` extra. This script needs only the standard library.

One line per event, in the order v1 stored them: session.started, each
lap.completed, then session.ended. The laps and the session totals come from the
store's index.sqlite; finished and fuel_at_start, which the index doesn't keep, from
the session's summary.json. Floats are written as Python's repr, so they read back
as the same float64. Session ids are left out: v1's are UTC stamps, v2's are ULIDs.
"""

import json
import sqlite3
import sys
from pathlib import Path


def main(store: Path) -> None:
    db = sqlite3.connect(store / "index.sqlite")
    sessions = db.execute(
        "SELECT id, type, car_code, total_laps, laps_completed, fuel_used, final_position,"
        " joined_on_lap FROM sessions ORDER BY started_at, id"
    ).fetchall()
    for n, (sid, kind, car, total, completed, used, final, joined) in enumerate(sessions, 1):
        summary = json.loads((store / "sessions" / sid / "summary.json").read_text())
        emit(
            event="session.started",
            session=n,
            type=kind,
            car_code=car,
            total_laps=total,
            fuel_at_start=summary["fuel_at_start"],
            joined_on_lap=joined,
        )
        laps = db.execute(
            "SELECT lap, lap_time_ms, complete, fuel_start, fuel_end, fuel_used, position,"
            " frames FROM laps WHERE session_id = ? ORDER BY lap",
            (sid,),
        )
        for lap, time_ms, complete, start, end, lap_used, position, frames in laps:
            emit(
                event="lap.completed",
                session=n,
                lap=lap,
                lap_time_ms=time_ms,
                complete=bool(complete),
                fuel_start=start,
                fuel_end=end,
                fuel_used=lap_used,
                position=position,
                frames=frames,
            )
        emit(
            event="session.ended",
            session=n,
            laps_completed=completed,
            fuel_used=used,
            final_position=final,
            finished=summary["finished"],
        )


def emit(**fields: object) -> None:
    print(json.dumps(fields))


if __name__ == "__main__":
    main(Path(sys.argv[1]))
