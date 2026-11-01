"""Write the goldens pit_test.go compares with: each pit entry, exit and
withdrawn entry v1's PitDetector finds, and the stops each session stored, as
JSON lines (those-who-run/paddock#78).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It runs the recording twice. First through a PitDetector of its own, fed every
frame as v1's tests/test_pitstop.py does, which writes a line for each change
of its status, in order:

- pit.entered, pit.exited or pit.teleport (an entry withdrawn), on the
  recording's frame (counted from 1), with the stint and whether the car is in
  the pit lane after it, and the stop as PitStop.record() writes it (the
  teleport for a withdrawn entry);
- session: the session v1's engine had open when it applied that change
  (Engine._apply_pits, numbered from 1), or null when it applied none there.

Then through v1's engine, with no rules, as v1's `replay` does, into a store of
its own: it checks that every change the engine applied is one of the
detector's, and writes a session.ended line for each session with the stints
and pit_stops its summary.json stored. A recording v1 opens no session on (a
replay or a GT7 demo, which v1 drops) has no session lines.
"""

import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.pitstop import PitDetector, PitStatus
from those_who_run.replay import read_recording
from those_who_run.store import SessionStore


def change(before: PitStatus, after: PitStatus) -> dict:
    if len(after.teleports) > len(before.teleports):
        jump = after.teleports[-1]
        return {
            "event": "pit.teleport",
            "teleport": {"lap": jump.lap, "t": jump.t, "entry_t": jump.entry_t, "evidence": jump.evidence},
        }
    stop = after.stops[-1]
    return {"event": "pit.entered" if after.in_pit_lane else "pit.exited", "stop": stop.record()}


def detect(states) -> list[dict]:
    det = PitDetector()
    lines = []
    for n, s in enumerate(states, 1):
        before = det.status
        status = det.observe(s)
        if status is not None:
            lines.append(
                {"frame": n, "stint": status.stint, "in_pit_lane": status.in_pit_lane, **change(before, status)}
            )
    return lines


def engine_run(path: Path, states) -> tuple[dict[int, tuple[int, str]], list[dict]]:
    """The changes v1's engine applied, by frame, and each session's stored stops."""
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    applied: dict[int, tuple[int, str]] = {}
    ended: list[dict] = []
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine([], CalloutQueue(max_age=None), store)
        now = {"frame": 0, "session": 0}

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            return start(state, **kw)

        store.start_session = start_session

        end = store.end_session

        def end_session(state, **kw):
            session = now["session"] if store.session is not None else None
            out = end(state, **kw)
            if session is not None and out is not None:
                summary = json.loads(Path(out).read_text())
                ended.append(
                    {
                        "event": "session.ended",
                        "session": session,
                        "stints": summary["stints"],
                        "pit_stops": summary["pit_stops"],
                    }
                )
            return out

        store.end_session = end_session

        apply = engine._apply_pits

        def _apply_pits(status, state):
            applied[now["frame"]] = (now["session"], status.in_pit_lane, len(status.teleports))
            return apply(status, state)

        engine._apply_pits = _apply_pits

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()
    return applied, ended


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    lines = detect(states)
    applied, ended = engine_run(path, states)
    by_frame = {line["frame"]: line for line in lines}
    for frame, (session, in_lane, teleports) in applied.items():
        line = by_frame.get(frame)
        assert line is not None, f"the engine applied a change on frame {frame} the detector did not make"
        assert line["in_pit_lane"] == in_lane, f"frame {frame}: the engine's change is not the detector's"
        line["session"] = session
    for line in lines:
        line.setdefault("session", None)
        print(json.dumps(line))
    for line in ended:
        print(json.dumps(line))


if __name__ == "__main__":
    main(Path(sys.argv[1]))
