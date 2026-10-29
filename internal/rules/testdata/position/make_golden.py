"""Write the goldens the position rule's tests compare with: every callout v1's
PositionRule raised, as JSON lines (those-who-run/paddock#94).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does: every rule on, as
config.DEFAULT_RULES has them with `script: false` (the bundled script only
changes the words), a queue with no age limit, drained after every frame at the
frame's time, into a store of its own. It writes one line per event, in order,
on the recording's frame (counted from 1):

- session.started: a session opened, numbered from 1;
- callout: PositionRule raised a callout, with the session open on that frame (0
  for none), and:
  - kind: the first phrase key the rule picked, the situational one where it
    chose one, configured or not, as contracts v0.2.3's Callout says (and
    paddock's parity/v1events.py callout_kind);
  - position and previous_position: the slots, the place called and the last
    place called (or the grid slot) before it;
  - priority, hold_secs, max_age_secs (null: the queue's, 8 s live) and
    pit_lane, as the rule stamped them;
  - outcome: spoken (popped from the queue once due), dropped (still held in
    the queue at a pit entry, which took it out: pit_lane drop, those-who-run#244;
    radio's to do in v2), held (kept for the exit: pit_lane hold), unpublished
    (raised in the pit lane and dropped there, which ecu never publishes) or
    cleared (still queued when a start or a paused end cleared the queue).

--drive takes every start for a drive: v1's DriverCheck, which holds a start
GT7's cars-on-track flag is off for and drops a replay or a GT7 demo, always says
"drive", as internal/race/testdata/kind/make_golden.py says. Use it on the
recordings v1 opens no session on (the SUGO replay and the Laguna Seca demo);
on the others it changes nothing.
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


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    cfg = copy.deepcopy(DEFAULT_RULES)
    cfg["script"] = False
    rules = build_rules(cfg["rules"])
    (rule,) = [r for r in rules if r.name == "position"]
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
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
        first = rule.first_template

        def first_template(options, default):
            keys.append(options[0])
            return first(options, default)

        rule.first_template = first_template

        evaluate = rule.evaluate

        def _evaluate(prev, state):
            before = rule.announced
            keys.clear()
            out = evaluate(prev, state)
            for c in out:
                lines.append((c, {
                    "event": "callout",
                    "session": now["session"] if store.session is not None else 0,
                    "frame": now["frame"],
                    "kind": keys[0],
                    "position": state.position,
                    "previous_position": before,
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
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
