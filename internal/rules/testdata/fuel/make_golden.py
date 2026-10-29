"""Write the goldens fuel_test.go compares with: every callout v1's FuelRule
raised, as JSON lines (those-who-run/paddock#92).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with every built-in rule on as
config.DEFAULT_RULES sets them and `script: false` (no rule's state depends on
the phrase it picks), into a store of its own, and writes one line per
decision, in order, on the recording's frame (counted from 1):

- session.started: a session opened, numbered from 1;
- callout: the fuel rule raised a callout, with the session it was raised in
  (0 for none), its kind (the phrase key the rule picked: contracts v0.2.3's
  Callout comment, as paddock's parity/v1events.py callout_kind makes it), its
  slots (laps, the whole laps v1 said), its priority, hold_secs and
  expiry_secs (the rule's max_age_seconds, else the queue's 8 s), and what
  v1's engine did with it in the pit lane: spoken, held or dropped.

--drive takes every start for a drive, as testdata/kind/make_golden.py in
internal/race says: on the recordings v1 opens no session on (a replay, a GT7
demo), so the rule sees the frames ecu will until paddock#74.
"""

import copy
import json
import sys
import tempfile
from pathlib import Path

import those_who_run.rules.fuel as fuel_module
from those_who_run.callouts import CalloutQueue
from those_who_run.config import DEFAULT_RULES
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.rules import build_rules
from those_who_run.store import SessionStore

QUEUE_MAX_AGE = DEFAULT_RULES["queue"]["max_age_seconds"]


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    cfg = copy.deepcopy(DEFAULT_RULES)
    rules = build_rules(cfg["rules"])
    (fuel,) = [r for r in rules if r.name == "fuel"]
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0}
        raised: list = []  # (callout, session, frame, kind, laps)
        outcome: dict[int, str] = {}

        keys: list[str] = []
        pick = fuel.phrases.pick

        def _pick(key, default):
            keys.append(key)
            return pick(key, default)

        fuel.phrases.pick = _pick

        said: list[int] = []
        laps_words = fuel_module.laps_words

        def _laps_words(n):
            said.append(n)
            return laps_words(n)

        fuel_module.laps_words = _laps_words

        make = fuel.callout

        def callout(text, state, priority=None):
            c = make(text, state, priority)
            kind = keys[0] if keys else ""
            laps = said[-1] if kind in ("template", "template_short") else None
            keys.clear()
            said.clear()
            session = now["session"] if store.session is not None else 0
            raised.append((c, session, now["frame"], kind, laps))
            return c

        fuel.callout = callout

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            spoken = {id(c) for c in out}
            for c in callouts:
                # Only the fuel rule's, which raised keeps alive: another rule's
                # callout is freed, and its id can come back on a later one.
                if c.rule == "fuel" and id(c) not in spoken:
                    outcome[id(c)] = {"hold": "held", "drop": "dropped"}[engine._policy(c)]
            return out

        engine._quiet = _quiet

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        start = store.start_session
        lines: list[tuple[int, int, dict]] = []  # (frame, order, line)

        def start_session(state, **kw):
            now["session"] += 1
            line = {"event": "session.started", "session": now["session"], "frame": now["frame"]}
            lines.append((now["frame"], len(lines), line))
            return start(state, **kw)

        store.start_session = start_session

        for state in states:
            engine.process(state)
            while engine.queue.pop(state.t) is not None:
                pass
        engine.close_session()
        store.close()

    for c, session, frame, kind, laps in raised:
        line = {
            "event": "callout",
            "session": session,
            "frame": frame,
            "kind": kind,
            "slots": {} if laps is None else {"laps": laps},
            "priority": c.priority,
            "hold_secs": c.hold,
            "expiry_secs": c.max_age if c.max_age is not None else QUEUE_MAX_AGE,
            "pit_lane": outcome.get(id(c), "spoken"),
            "text": c.text,
        }
        lines.append((frame, len(lines), line))
    for _, _, line in sorted(lines, key=lambda x: (x[0], x[1])):
        emit(**line)


if __name__ == "__main__":
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
