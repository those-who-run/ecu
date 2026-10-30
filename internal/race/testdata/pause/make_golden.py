"""Write the goldens pause_test.go compares with: each pause v1's PauseClock saw
in an open session, and what each session stored about its pauses, as JSON lines
(those-who-run/paddock#77).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with no rules (they do not change
the pauses), into a store of its own, and writes one line per event, in order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- session.paused: PauseClock.since went from none to a time while a session was
  open, on the recording's frame: "flagged" when GT7 flagged the frame paused,
  "frozen" when PauseClock read it as the frozen stream, with since_frame, the
  frame the pause is dated from (a frozen stream's first unchanged frame), and
  paused_ms, the session's time paused before it;
- session.resumed: PauseClock.since went back to none, for a pause that
  session.paused was written for, on the frame after the pause, with the
  pause's length (pause_ms, on the frames' clock) and the session's time paused
  so far (paused_ms);
- session.ended: the session ended, with the paused_ms and ended_paused its
  summary.json stored.

The milliseconds are v1's round(seconds * 1000).

--drive takes every start for a drive, as testdata/kind/make_golden.py says;
every golden here is made with it.
"""

import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.state import Phase
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def ms(secs: float) -> int:
    return round(secs * 1000)


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine([], CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0, "run_frame": 0}
        frame_of: dict[int, int] = {}  # id(state) as the clock gave it: its frame
        pause: dict[str, object] = {}  # the pause session.paused was written for

        clock = engine.pauses
        observe = clock.observe

        def observe_pause(state):
            key, since, before = clock._key, clock.since, clock.paused_secs
            out = observe(state)
            frame_of[id(out)] = now["frame"]
            if clock._key != key:
                now["run_frame"] = now["frame"]
            if since is None and clock.since is not None and store.session is not None:
                frozen = state.phase is Phase.IN_RACE
                pause.update(session=now["session"])
                emit(
                    event="session.paused",
                    session=now["session"],
                    frame=now["frame"],
                    kind="frozen" if frozen else "flagged",
                    since_frame=now["run_frame"] if frozen else now["frame"],
                    paused_ms=ms(before),
                )
            elif since is not None and clock.since is None and pause:
                emit(
                    event="session.resumed",
                    session=pause["session"],
                    frame=now["frame"],
                    pause_ms=ms(state.t - since),
                    paused_ms=ms(clock.paused_secs),
                )
                pause.clear()
            return out

        clock.observe = observe_pause

        start = store.start_session

        def start_session(state, **kw):
            # A start held for DriverCheck opens on a later frame than its own.
            now["session"] += 1
            emit(event="session.started", session=now["session"], frame=frame_of[id(state)])
            return start(state, **kw)

        store.start_session = start_session

        end = store.end_session

        def end_session(state, **kw):
            session = now["session"] if store.session is not None else None
            out = end(state, **kw)
            if session is not None and out is not None:
                summary = json.loads(Path(out).read_text())
                emit(
                    event="session.ended",
                    session=session,
                    paused_ms=summary["paused_ms"],
                    ended_paused=summary["ended_paused"],
                )
                pause.clear()
            return out

        store.end_session = end_session

        for n, state in enumerate(states, 1):
            now["frame"] = n
            engine.process(state)
        engine.close_session()
        store.close()


if __name__ == "__main__":
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
