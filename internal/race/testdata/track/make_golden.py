"""Write the goldens track_test.go compares with: each change of the track verdict
v1's engine applied to a session, and the track each session stored, as JSON lines
(those-who-run/paddock#81).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with no rules (they do not touch
the track), into a store of its own, and writes one line per event, in order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- session.track: Engine._apply_track changed the session's verdict (v1's
  "track ..." log line), on the frame it changed on;
- session.ended: the session ended, with the track its summary.json stored.

A track is its id, name, confidence and family, its candidates' ids best first and
the evidence, as v1's track_record stores it. A recording v1 opens no session on
(a replay or a GT7 demo, which v1 drops) gives an empty file.

real-nuc-laguna-seca-demo-2026-10-18.joined.jsonl is not this script's: it is
internal/track's make_golden.py --join on that recording, the detector told the
frames start part way, as ecu's session there is joined.
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


def track(m) -> dict:
    return {
        "track_id": m.track_id,
        "name": m.name,
        "confidence": m.confidence,
        "family": m.family,
        "candidates": [c.track.id for c in m.candidates],
        "evidence": m.evidence,
    }


def stored(rec: dict | None) -> dict | None:
    if rec is None:
        return None
    return {
        "track_id": rec["id"],
        "name": rec["name"],
        "confidence": rec["confidence"],
        "family": rec["family"],
        "candidates": [c["id"] for c in rec["candidates"]],
        "evidence": rec["evidence"],
    }


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
                emit(event="session.ended", session=now["session"], frame=now["frame"], track=stored(s.track))
            return end(state, **kw)

        store.end_session = end_session

        apply = engine._apply_track

        def _apply_track(match, store=True):
            before = engine.track
            apply(match, store=store)
            if engine.track is not before:
                emit(event="session.track", session=now["session"], frame=now["frame"], track=track(match))

        engine._apply_track = _apply_track

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    main(Path(sys.argv[1]))
