"""Write the goldens finish_test.go compares with: each finish v1's engine saw in
an open session, and what each session stored about it, as JSON lines
(those-who-run/paddock#85).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with no rules (they do not
change the finish), into a store of its own, and writes one line per event, in
order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- race.finished: Engine.finished went from false to true in a step with the
  session open, on the recording's frame: "flag" for the first FINISHED frame,
  "timed" for Engine._timed_finish, with the frame it is as of (the flag frame,
  or the timed finish's line crossing), the lap (the crossing's current_lap - 1,
  as v1 logs it), the position there and the session's kind;
- session.ended: the session ended, with the finished, total_time_ms and
  best_lap_ms its summary.json stored, and the best lap's number
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
from those_who_run.state import Phase
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
        engine = Engine([], CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0}
        seen: dict[int, tuple[object, int]] = {}  # id(state): the state, its frame

        step = engine._step
        pending: dict[str, object] = {}  # the step under way: was it finished, was a session open

        def check() -> None:
            """Write the finish once Engine.finished goes true in a step that began
            with the session open: before the session ends, if it ends in that step."""
            if not pending or pending["finished"] or not engine.finished or not pending["open"]:
                return
            state = pending["state"]
            pending.clear()
            if state.phase is Phase.FINISHED:
                kind, at = "flag", state
            else:
                kind, at = "timed", engine._crossing
            emit(
                event="race.finished",
                session=now["session"],
                frame=now["frame"],
                kind=kind,
                at=seen[id(at)][1],
                lap=at.current_lap - 1,
                position=at.position,
                session_kind=engine.kinds.kind.kind,
            )

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            seen[id(state)] = (state, now["frame"])
            pending.update(finished=engine.finished, open=store.session is not None, state=state)
            out = step(state)
            check()
            pending.clear()
            return out

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            emit(event="session.started", session=now["session"], frame=now["frame"])
            return start(state, **kw)

        store.start_session = start_session

        end = store.end_session

        def end_session(state, **kw):
            check()
            session = now["session"] if store.session is not None else None
            out = end(state, **kw)
            if session is not None and out is not None:
                summary = json.loads(Path(out).read_text())
                kind = (summary.get("session_kind") or {}).get("kind")
                best = best_lap(summary["laps"], kind == "lap_race", summary.get("gt7_best_lap_ms"))
                emit(
                    event="session.ended",
                    session=session,
                    finished=summary["finished"],
                    total_time_ms=summary["total_time_ms"],
                    best_lap_ms=summary["best_lap_ms"],
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
