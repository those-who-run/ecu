"""Write the goldens lap_test.go compares with: each lap v1 stored, with its pit
role and whether v1's lap_time rule counted it for the best, and each session's
best lap as it stored it, as JSON lines (those-who-run/paddock#83).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with v1's lap_time rule as the
only rule, into a store of its own, and writes one line per event, in order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- lap.completed: SessionStore._flush_lap stored a lap, with its lap, lap_time_ms,
  complete and pit as summary.json stores them, and counts_for_best: the lap_time
  rule's `counts` for the lap (not a start lap GT7 leaves out of its best, and no
  pit role), worked out as the rule does on the frame that posted the lap's time,
  or null when the rule never took a time for that lap;
- session.ended: the session ended, with the best_lap_ms, fastest_lap_ms and
  gt7_best_lap_ms its summary.json stored, and the best lap's number
  (results.best_lap, as the card and the spoken summary give it).

--drive takes every start for a drive, as testdata/kind/make_golden.py says.
"""

import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.results import best_lap
from those_who_run.rules.lap_time import LapTimeRule
from those_who_run.state import posted_lap
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        rule = LapTimeRule()
        engine = Engine([rule], CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0}
        counts: dict[int, bool] = {}  # the lap_time rule's verdict, by lap, this session

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        evaluate = rule.evaluate

        def rule_evaluate(prev, state):
            before = rule.last_announced_lap
            out = evaluate(prev, state)
            if rule.last_announced_lap != before:
                # LapTimeRule.evaluate's own `counts`, from the same frame and status.
                lap_no = posted_lap(state)
                start_lap = lap_no == 1 and (state.has_lap_count or state.best_lap_ms <= 0)
                counts[lap_no] = not start_lap and rule.pits.lap_role(lap_no) is None
            return out

        rule.evaluate = rule_evaluate

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            counts.clear()
            emit(event="session.started", session=now["session"], frame=now["frame"])
            return start(state, **kw)

        store.start_session = start_session

        flush = store._flush_lap

        def flush_lap(lap_no, state, complete):
            s = store.session
            n = len(s.laps) if s is not None else 0
            flush(lap_no, state, complete)
            if s is not None and len(s.laps) > n:
                lap = s.laps[-1]
                emit(
                    event="lap.completed",
                    session=now["session"],
                    frame=now["frame"],
                    lap=lap["lap"],
                    lap_time_ms=lap["lap_time_ms"],
                    complete=lap["complete"],
                    pit=lap["pit"],
                    counts_for_best=counts.get(lap["lap"]),
                )

        store._flush_lap = flush_lap

        end = store.end_session

        def end_session(state, **kw):
            session = now["session"] if store.session is not None else None
            out = end(state, **kw)
            if session is not None and out is not None:
                summary = json.loads(Path(out).read_text())
                kind = (summary.get("session_kind") or {}).get("kind")
                best = best_lap(summary["laps"], kind == "lap_race", summary.get("gt7_best_lap_ms"))
                emit(
                    event="session.ended",
                    session=session,
                    best_lap_ms=summary["best_lap_ms"],
                    fastest_lap_ms=summary["fastest_lap_ms"],
                    gt7_best_lap_ms=summary["gt7_best_lap_ms"],
                    best_lap=best[0] if best else None,
                )
            return out

        store.end_session = end_session

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
