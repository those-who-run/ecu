"""Write the goldens laptime_test.go compares with: each callout v1's lap_time
rule raised, as JSON lines (those-who-run/paddock#91).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with every rule on, as
config.DEFAULT_RULES sets them with the bundled script off (`script: false`, so
each phrase has its one built-in template), into a store of its own, and writes
one line per event, in order, on the recording's frame (counted from 1):

- session.started: a session opened, numbered from 1;
- callout: the lap_time rule raised a callout, with its session (0 with none
  open), the kind contracts v0.2.3 names for it (the first phrase key the rule
  picked, a situational pool counting as chosen, as paddock's
  parity/v1events.py's callout_kind does), its slots (lap, lap_time_ms, and
  delta_ms: the lap's time less the best before it, exact, but for delta_first
  and a pit lap), its priority, hold_ms, expiry_ms (the rule's max_age, else the
  queue's 8 s), pit_lane, and what v1's engine did with it in the pit lane:
  spoken, held (raised in the lane and kept to the exit, with the frame it was
  released on) or dropped.
"""

import copy
import json
import sys
import tempfile
from pathlib import Path

from those_who_run.callouts import CalloutQueue
from those_who_run.config import DEFAULT_RULES, validate_templates
from those_who_run.engine import Engine
from those_who_run.replay import read_recording
from those_who_run.rules import build_rules
from those_who_run.state import posted_lap
from those_who_run.store import SessionStore

QUEUE_MAX_AGE_SECS = DEFAULT_RULES["queue"]["max_age_seconds"]


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    cfg = copy.deepcopy(DEFAULT_RULES)
    cfg["script"] = False
    validate_templates(cfg)
    rules = build_rules(cfg["rules"])
    rule = next(r for r in rules if r.name == "lap_time")
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        now = {"frame": 0, "session": 0}
        # Every line, written at the end: a callout's pit-lane outcome and release
        # come after the frame it was raised on.
        lines: list[dict] = []
        # id(callout): the callout, kept so its id is not reused, and its line
        raised: dict[int, tuple[object, dict]] = {}
        held: dict[int, dict] = {}  # id(callout) held in the lane: its line

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            lines.append({"event": "session.started", "session": now["session"], "frame": now["frame"]})
            return start(state, **kw)

        store.start_session = start_session

        # The phrase keys the rule picked, in order, as parity/v1events.py takes them.
        keys: list[str] = []
        pick, first = rule.phrases.pick, rule.phrases.first
        inside = [False]

        def _pick(key, default):
            if not inside[0]:
                keys.append(key)
            return pick(key, default)

        def _first(options, default):
            if options:
                keys.append(options[0])
            inside[0] = True
            try:
                return first(options, default)
            finally:
                inside[0] = False

        rule.phrases.pick, rule.phrases.first = _pick, _first

        evaluate = rule.evaluate

        def _evaluate(prev, state):
            keys.clear()
            # The best the rule measures against, as LapTimeRule.evaluate takes it.
            prev_best = 0
            if prev is not None:
                prev_best = prev.best_lap_ms if prev.best_lap_ms > 0 else rule.own_best_ms
            out = evaluate(prev, state)
            for c in out:
                kind = keys[0]
                slots = {"lap": posted_lap(state), "lap_time_ms": state.last_lap_ms}
                if kind != "delta_first" and kind not in (
                    "template_in_lap",
                    "template_out_lap",
                    "template_pit_lap",
                ):
                    slots["delta_ms"] = state.last_lap_ms - prev_best
                line = {
                    "event": "callout",
                    "session": now["session"] if store.session is not None else 0,
                    "frame": now["frame"],
                    "kind": kind,
                    "slots": slots,
                    "priority": c.priority,
                    "hold_ms": round(c.hold * 1000),
                    "expiry_ms": round((c.max_age if c.max_age is not None else QUEUE_MAX_AGE_SECS) * 1000),
                    "pit_lane": rule.pit_lane,
                    "outcome": "spoken",
                    "text": c.text,
                }
                lines.append(line)
                raised[id(c)] = (c, line)
            return out

        rule.evaluate = _evaluate

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            spoken = {id(c) for c in out}
            for c in callouts:
                if id(c) not in raised or id(c) in spoken:
                    continue
                line = raised[id(c)][1]
                policy = engine._policy(c)
                line["outcome"] = {"hold": "held", "drop": "dropped"}[policy]
                if policy == "hold":
                    held[id(c)] = line
            return out

        engine._quiet = _quiet

        release = engine._release

        def _release(state):
            for c in engine.pit_held:
                line = held.pop(id(c), None) if id(c) in raised else None
                if line is not None:
                    line["released"] = now["frame"]
            return release(state)

        engine._release = _release

        process = engine.process

        def _process(state):
            out = process(state)
            while engine.queue.pop(state.t) is not None:
                pass
            return out

        engine.process = _process

        for state in states:
            engine.process(state)
        engine.close_session()
        store.close()
    for line in lines:
        print(json.dumps(line))


if __name__ == "__main__":
    main(Path(sys.argv[1]))
