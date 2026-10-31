"""Write the goldens kind_test.go compares with: each change of the session kind
v1's engine applied to a session, and the kind each session stored, as JSON lines
(those-who-run/paddock#82).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

and, the same way, for the live sessions cut in ../sessions/ (paddock#145), such
as real-nuc-suzuka-practice-2026-10-31.

It replays the recording as v1's `replay` does, with no rules (they do not change
the kind), into a store of its own, and writes one line per event, in order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- session.kind: SessionDetector.observe returned a new decision and the engine
  applied it to the open session (v1's "session ..." log line, Engine._apply_kind),
  on the frame it changed on;
- session.ended: the session ended, with the kind its summary.json stored.

A kind is v1's SessionKind.record(), as summary.json stores it.
"""

import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
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
            emit(event="session.started", session=now["session"], frame=now["frame"])
            return start(state, **kw)

        store.start_session = start_session

        end = store.end_session

        def end_session(state, **kw):
            s = store.session
            if s is not None:
                emit(event="session.ended", session=now["session"], frame=now["frame"], kind=s.kind)
            return end(state, **kw)

        store.end_session = end_session

        apply = engine._apply_kind

        def _apply_kind(kind):
            if store.session is not None:
                emit(event="session.kind", session=now["session"], frame=now["frame"], kind=kind.record())
            apply(kind)

        engine._apply_kind = _apply_kind

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    main(Path(sys.argv[1]))
