"""Write the goldens final_lap_test.go and position_test.go compare with: v1's
final lap and position decisions in an open session, as JSON lines
(those-who-run/paddock#84).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with three rules and no others
(no rule changes what these decide), into a store of its own, and writes one
line per decision, in order, on the recording's frame (counted from 1):

- session.started: a session opened, numbered from 1;
- race.final_lap: v1's FinalLapRule announced the final lap, with its lap;
- race.position: v1's PositionRule with the event's settings
  (start_grace_secs 0 and min_interval_secs 0, so only its 3 s settle is left)
  took a new position, with the one before it: race.position is the fact
  without the callout's limits (contracts v0.2.3's RacePositionChanged);
- position.call: v1's PositionRule as rules.example.yaml configures it (15 s
  start grace, 10 s between calls) called a position, for AC4: every call has a
  race.position at or before it.

Only decisions with a session open are written, as ecu publishes only into one.
--drive takes every start for a drive, as testdata/kind/make_golden.py says.
"""

import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.rules.final_lap import FinalLapRule
from those_who_run.rules.position import PositionRule
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    final_lap = FinalLapRule()
    event = PositionRule({"start_grace_secs": 0, "min_interval_secs": 0})
    call = PositionRule({"settle_secs": 3, "min_interval_secs": 10, "start_grace_secs": 15})
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine([final_lap, event, call], CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0}

        def is_open() -> bool:
            return store.session is not None

        def wrap(rule, write):
            evaluate = rule.evaluate

            def _evaluate(prev, state):
                before = getattr(rule, "announced", None)
                out = evaluate(prev, state)
                if out and is_open():
                    write(state, before)
                return out

            rule.evaluate = _evaluate

        wrap(final_lap, lambda state, _: emit(
            event="race.final_lap", session=now["session"], frame=now["frame"], lap=state.current_lap))
        wrap(event, lambda state, before: emit(
            event="race.position", session=now["session"], frame=now["frame"],
            previous=before, position=state.position))
        wrap(call, lambda state, _: emit(
            event="position.call", session=now["session"], frame=now["frame"], position=state.position))

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            emit(event="session.started", session=now["session"], frame=now["frame"])
            return start(state, **kw)

        store.start_session = start_session

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
