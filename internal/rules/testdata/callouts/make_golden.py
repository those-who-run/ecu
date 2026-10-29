"""Write the goldens golden_test.go compares with: every callout v1 raised on a
recording, every rule, as JSON lines (those-who-run/paddock#89).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with every rule on and v1's
defaults (config.DEFAULT_RULES), and v1's start check taking every start for a
drive, as ecu does until paddock#74 (testdata/kind/make_golden.py in
internal/race says why; on a recording v1 opens a session on, it changes
nothing). Nothing in v1 is changed: methods are wrapped on their instances, as
paddock's parity/v1events.py wraps them, and the kind is named as it names it.

One line per callout, in the order raised:

- session: the open session, numbered from 1; 0 with none;
- frame: the frame it was raised on (counted from 1), ecu's `at`;
- rule, kind, priority, pit_lane: as ecu publishes them (contracts v0.2.3);
- outcome: spoken; held in the pit lane and released on frame `released`;
  dropped, queued at a pit entry, which radio drops and ecu publishes; or
  unpublished, raised in the lane and dropped, or held and never released (the
  session ended in the lane), which ecu never publishes.
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
from those_who_run.store import SessionStore


def callout_kind(rule, keys, briefed):
    """parity/v1events.py's callout_kind, as paddock#308 has it."""
    if rule == "start_brief":
        if "template_joined" in keys:
            return "template_joined"
        return "template_track" if briefed else "template"
    if rule == "race_summary":
        position = [k for k in keys if k.startswith("template_position")]
        if position:
            return position[0]
        return "template_quit" if "template_quit" in keys else "template"
    return keys[0] if keys else ""


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    now = {"frame": 0, "session": 0}
    raised = []  # [callout, line]
    outcome: dict[int, dict] = {}
    at_entry = [False]
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        cfg = copy.deepcopy(DEFAULT_RULES)
        rules = build_rules(cfg["rules"])
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        engine.driver.observe = lambda state: "drive"

        def wrap_rule(rule):
            keys = []
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
            briefed = [False]
            evaluate = rule.evaluate

            def _evaluate(prev, state):
                briefed[0] = bool(getattr(rule, "briefed", False))
                return evaluate(prev, state)

            rule.evaluate = _evaluate
            make = rule.callout

            def callout(text, state, priority=None):
                c = make(text, state, priority)
                picked = list(keys)
                keys.clear()
                line = {
                    "session": now["session"] if store.session is not None else 0,
                    "frame": now["frame"],
                    "rule": c.rule,
                    "kind": callout_kind(rule.name, picked, briefed[0]),
                    "priority": c.priority,
                    "pit_lane": rule.pit_lane,
                }
                raised.append((c, line))
                return c

            rule.callout = callout

        for rule in rules:
            wrap_rule(rule)

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

        apply_pits = engine._apply_pits

        def _apply_pits(status, state):
            at_entry[0] = True
            try:
                return apply_pits(status, state)
            finally:
                at_entry[0] = False

        engine._apply_pits = _apply_pits

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            spoken = {id(c) for c in out}
            for c in callouts:
                if id(c) not in spoken:
                    what = {"hold": "held", "drop": "dropped"}[engine._policy(c)]
                    if what == "dropped" and not at_entry[0]:
                        what = "unpublished"
                    outcome[id(c)] = {"outcome": what}
            return out

        engine._quiet = _quiet

        release = engine._release

        def _release(state):
            for c in engine.pit_held:
                outcome[id(c)]["released"] = now["frame"]
            return release(state)

        engine._release = _release

        for state in states:
            engine.process(state)
            while engine.queue.pop(state.t) is not None:
                pass
        engine.close_session()
        store.close()

    for c, line in raised:
        line.update(outcome.get(id(c), {"outcome": "spoken"}))
        if line["outcome"] == "held" and "released" not in line:
            line["outcome"] = "unpublished"  # held, and the session ended in the lane
        print(json.dumps(line))


if __name__ == "__main__":
    main(Path(sys.argv[1]))
