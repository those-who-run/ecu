"""Write the goldens golden_test.go compares with: every callout v1's TempsRule
raised, as JSON lines (those-who-run/paddock#97).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does: every rule on, as
config.DEFAULT_RULES has them with `script: false` (the bundled script only
changes the words), a queue with no age limit, drained after every frame at the
frame's time, into a store of its own. Nothing in v1 is changed: methods are
wrapped on their instances. It writes one line per event, in order, on the
recording's frame (counted from 1):

- session.started: a session opened, numbered from 1;
- callout: TempsRule raised a callout, with the session open on that frame (0
  for none), and:
  - kind: the phrase key the rule picked (template_tyre_hot,
    template_tyres_cold, template_water or template_oil), as contracts
    v0.2.3's Callout says;
  - corners: for template_tyre_hot, the tyres whose alarm fired on the frame
    (fl, fr, rl, rr, in GT7's order): the one that fired and those that joined
    it, v1's {corners};
  - tyres_c, water_c and oil_c: the frame's temperatures, which car.alarm
    carries (tyres in GT7's order);
  - priority, hold_secs, max_age_secs (null: the queue's, 8 s live) and
    pit_lane, as the rule stamped them;
  - outcome: spoken (popped from the queue once due), held (kept in the pit
    lane for the exit: pit_lane hold), dropped (still queued at a pit entry,
    which radio drops), unpublished (raised in the pit lane and dropped, never
    published) or cleared (still queued when a start or a paused end cleared
    the queue).
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

CORNERS = ("fl", "fr", "rl", "rr")


def main(path: Path) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    cfg = copy.deepcopy(DEFAULT_RULES)
    cfg["script"] = False
    rules = build_rules(cfg["rules"])
    (rule,) = [r for r in rules if r.name == "temps"]
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        now = {"frame": 0, "session": 0}
        lines: list[tuple[object, dict]] = []  # each line, with its callout or None
        outcome: dict[int, str] = {}  # id(callout): how it left the queue
        at_entry = [False]

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            return step(state)

        engine._step = _step

        start = store.start_session

        def start_session(state, **kw):
            now["session"] += 1
            lines.append((None, {"event": "session.started", "session": now["session"], "frame": now["frame"]}))
            return start(state, **kw)

        store.start_session = start_session

        keys: list[str] = []
        template = rule.template

        def _template(key, default):
            keys.append(key)
            return template(key, default)

        rule.template = _template

        evaluate = rule.evaluate

        def _evaluate(prev, state):
            # reset() makes new alarms on every start, so read them on the call.
            before = [a.fired_at for a in getattr(rule, "tyres", [])]
            keys.clear()
            out = evaluate(prev, state)
            fired = [CORNERS[i] for i, a in enumerate(rule.tyres) if a.fired_at != before[i]]
            for key, c in zip(keys, out, strict=True):
                lines.append((c, {
                    "event": "callout",
                    "session": now["session"] if store.session is not None else 0,
                    "frame": now["frame"],
                    "kind": key,
                    "corners": fired if key == "template_tyre_hot" else [],
                    "tyres_c": list(state.tire_temp),
                    "water_c": state.water_temp,
                    "oil_c": state.oil_temp,
                    "priority": c.priority,
                    "hold_secs": c.hold,
                    "max_age_secs": c.max_age,
                    "pit_lane": rule.pit_lane,
                }))
            return out

        rule.evaluate = _evaluate

        apply_pits = engine._apply_pits

        def _apply_pits(status, state):
            at_entry[0] = True  # what _quiet sorts now was queued before the entry
            try:
                return apply_pits(status, state)
            finally:
                at_entry[0] = False

        engine._apply_pits = _apply_pits

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            kept = {id(c) for c in out}
            for c in callouts:
                # Only the rule's own: lines keeps them alive, so their ids are
                # never reused, which another rule's released callout's can be.
                if c.rule == rule.name and id(c) not in kept:
                    o = {"hold": "held", "drop": "dropped"}[engine._policy(c)]
                    if o == "dropped" and not at_entry[0]:
                        o = "unpublished"
                    outcome[id(c)] = o
            return out

        engine._quiet = _quiet

        for state in states:
            engine.process(state)
            while (c := engine.queue.pop(state.t)) is not None:
                if c.rule == rule.name:
                    outcome.setdefault(id(c), "spoken")
        engine.close_session()
        store.close()

    # After the run, when the pit lane has had its say on each callout.
    for c, line in lines:
        if c is not None:
            line["outcome"] = outcome.get(id(c), "cleared")
        print(json.dumps(line))


if __name__ == "__main__":
    main(Path(sys.argv[1]))
