"""Write the goldens startbrief_test.go compares with: each callout v1's
StartBriefRule raised, as JSON lines (those-who-run/paddock#95).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with every rule on as v1's
DEFAULT_RULES configures them (no bundled script: `script: false`, so each
callout has one template) and a queue with no age limit, popped on every frame,
into a store of its own. It writes one line per event, in order, on the
recording's frame (counted from 1):

- session.started: a session opened, numbered from 1, and whether it was joined
  part way;
- callout: StartBriefRule raised a callout, with the kind contracts v0.2.3 names
  for it (paddock's parity/v1events.py callout_kind: template for the brief,
  template_track for the track named after it, template_joined for the brief of a
  joined session), the slots that kind carries (Callout's comment), its
  priority, hold_secs, expiry (the rule's max_age_seconds, else the queue's 8 s),
  pit_lane, what v1's pit lane did with it (spoken, or held to the exit) and v1's
  text for reference;
- released: a callout held in the pit lane was let out, as raised on this frame.

The slots are the values v1 put in the phrases it picked: the name it spoke for
{track}, as the track verdict gives it (radio says it with v1's track_words), the
frame's total_laps for {laps}, laps_left for template_laps_to_go, the frame's
position and total_cars for {p} and {total}, and current_lap for
template_joined's {lap}, with total_laps beside it when the race has a lap total.
"""

import copy
import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.config import DEFAULT_RULES
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.rules import build_rules
from those_who_run.rules.start_brief import StartBriefRule
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def slots(keys: list[str], tracks: list[str], state) -> dict[str, object]:
    """The slots of the phrases picked, in contracts v0.2.3's names."""
    out: dict[str, object] = {}
    for key in keys:
        if key == "template_track":
            out["track"] = tracks.pop(0)
        elif key == "template_laps":
            out["laps"] = state.total_laps
        elif key == "template_laps_to_go":
            out["laps_left"] = state.laps_left
        elif key == "template_timed":
            out["timed"] = True
        elif key in ("template_position", "template_joined_position"):
            out["position"], out["total_cars"] = state.position, state.total_cars
        elif key == "template_joined":
            out["lap"] = state.current_lap
            if state.has_lap_count:
                out["laps"] = state.total_laps
        else:
            raise ValueError(f"unknown phrase key {key}")
    return out


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    cfg = copy.deepcopy(DEFAULT_RULES)
    rules = build_rules(cfg["rules"])
    brief = next(r for r in rules if isinstance(r, StartBriefRule))
    queue_max_age = float(cfg["queue"]["max_age_seconds"])
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        now = {"frame": 0, "session": 0}
        raised: list[tuple[object, dict[str, object]]] = []  # each callout, its line
        outcome: dict[int, str] = {}  # id(callout): held in the pit lane

        keys: list[str] = []
        tracks: list[str] = []
        template, track_phrase = brief.template, brief._track_phrase

        def _template(key, default):
            keys.append(key)
            return template(key, default)

        def _track_phrase(name):
            tracks.append(name)
            return track_phrase(name)

        brief.template, brief._track_phrase = _template, _track_phrase

        briefed = [False]
        evaluate = brief.evaluate

        def _evaluate(prev, state):
            briefed[0] = brief.briefed
            return evaluate(prev, state)

        brief.evaluate = _evaluate

        make = brief.callout

        def callout(text, state, priority=None):
            c = make(text, state, priority)
            picked = list(keys)
            keys.clear()
            if "template_joined" in picked:
                kind = "template_joined"
            else:
                kind = "template_track" if briefed[0] else "template"
            line = {
                "event": "callout",
                "session": now["session"],
                "frame": now["frame"],
                "kind": kind,
                "slots": slots(picked, tracks, state),
                "priority": c.priority,
                "hold_secs": c.hold,
                "expiry_secs": c.max_age if c.max_age is not None else queue_max_age,
                "pit_lane": brief.pit_lane,
                "text": c.text,
            }
            raised.append((c, line))
            return c

        brief.callout = callout

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            spoken = {id(c) for c in out}
            for c in callouts:
                if id(c) not in spoken:
                    outcome[id(c)] = {"hold": "held", "drop": "dropped"}[engine._policy(c)]
            return out

        engine._quiet = _quiet

        release = engine._release

        def _release(state):
            held = [c for c in engine.pit_held if c.rule == brief.name]
            out = release(state)
            for c in held:
                emit(event="released", session=now["session"], frame=now["frame"], text=c.text)
            return out

        engine._release = _release

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            raised.clear()
            out = step(state)
            for c, line in raised:
                emit(**line, outcome=outcome.get(id(c), "spoken"))
            return out

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            emit(
                event="session.started",
                session=now["session"],
                frame=now["frame"],
                joined=bool(kw.get("joined")),
            )
            return start(state, **kw)

        store.start_session = start_session

        for state in states:
            engine.process(state)
            while engine.queue.pop(state.t) is not None:
                pass
        engine.close_session()
        store.close()


if __name__ == "__main__":
    main(Path(sys.argv[1]))
