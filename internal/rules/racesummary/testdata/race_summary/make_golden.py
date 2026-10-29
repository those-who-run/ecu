"""Write the goldens the racesummary package's test compares with: each callout v1's
race_summary rule raised, as JSON lines (those-who-run/paddock#96).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each of v1's recordings:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --drive RECORDING > NAME.jsonl

It replays the recording as v1's `replay` does, with every rule on and the
bundled script off (`script: false`: DEFAULT_RULES as they are), into a store of
its own, and writes one line per event, in order:

- session.started: a session opened, numbered from 1, on the recording's frame
  (counted from 1);
- callout: the race_summary rule raised a callout, on the recording's frame the
  engine was stepping (at), as of the frame the rule gave it (as_of: the timed
  finish's line crossing for on_finish, else the frame itself), with the kind
  contracts v0.2.3 names for it (paddock's parity/v1events.py, callout_kind), its
  slots (position, best_lap_ms, laps; a slot v1 did not say is left out), its
  priority, max_age and hold in seconds, and what v1's pit lane did with it:
  "spoken", "held" or "dropped" (Engine._quiet), with in_pit_lane, the lane as
  the engine stood when it was raised;
- session.ended: the session ended.

Nothing in v1 is changed: the engine's, the store's and the rule's methods are
wrapped on their instances.

--drive takes every start for a drive, as internal/race/testdata/kind/make_golden.py
says. It is used on the recordings v1 opens no session on.
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
from those_who_run.store import SessionStore


def emit(**kw: object) -> None:
    print(json.dumps(kw))


def callout_kind(keys: list[str]) -> str:
    """race_summary's kind, as paddock's parity/v1events.py callout_kind gives it:
    the key that fills {position}; else template_quit; else template."""
    position = [k for k in keys if k.startswith("template_position")]
    if position:
        return position[0]
    return "template_quit" if "template_quit" in keys else "template"


def main(path: Path, drive: bool) -> None:
    states = list(read_recording(path, strict=True))
    frame_of: dict[tuple[int, float], list[int]] = {}
    for n, s in enumerate(states, 1):
        frame_of.setdefault((s.packet_id, s.t), []).append(n)
    with tempfile.TemporaryDirectory() as tmp:
        store = SessionStore(Path(tmp))
        cfg = copy.deepcopy(DEFAULT_RULES)
        validate_templates(cfg)
        rules = build_rules(cfg["rules"])
        rule = next(r for r in rules if r.name == "race_summary")
        engine = Engine(rules, CalloutQueue(max_age=None), store)
        if drive:
            engine.driver.observe = lambda state: "drive"
        now = {"frame": 0, "session": 0}
        seen: dict[int, int] = {}  # id(state) of a stepped frame: its frame
        raised: list[tuple[object, dict]] = []  # each callout and its line
        outcome: dict[int, str] = {}  # id(callout): held or dropped in the pit lane

        step = engine._step

        def _step(state):
            now["frame"] = frame_of[(state.packet_id, state.t)].pop(0)
            seen[id(state)] = now["frame"]
            return step(state)

        engine._step = _step

        quiet = engine._quiet

        def _quiet(callouts):
            out = quiet(callouts)
            spoken = {id(c) for c in out}
            for c in callouts:
                if id(c) not in spoken:
                    outcome[id(c)] = {"hold": "held", "drop": "dropped"}[engine._policy(c)]
            return out

        engine._quiet = _quiet

        start = store.start_session

        def start_session(state, **kw):
            flush()
            now["session"] += 1
            emit(event="session.started", session=now["session"], frame=now["frame"])
            return start(state, **kw)

        store.start_session = start_session

        end = store.end_session

        def end_session(state, **kw):
            session = now["session"] if store.session is not None else None
            out = end(state, **kw)
            if session is not None and out is not None:
                flush()
                emit(event="session.ended", session=session, frame=now["frame"])
            return out

        store.end_session = end_session

        # The keys the rule's phrases picked, and the slots it said, for the callout
        # it is making.
        keys: list[str] = []
        slots: dict[str, int] = {}
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

        on_finish = rule.on_finish

        def _on_finish(state):
            if not rule.announced and rule.has_field(state) and state.position is not None:
                slots["position"] = state.position
            return on_finish(state)

        rule.on_finish = _on_finish

        on_end = rule.on_end

        def _on_end(last, state):
            slots["laps"] = max(rule.laps_done, last.current_lap - 1)
            return on_end(last, state)

        rule.on_end = _on_end

        best_words = rule._best_words

        def _best_words(state):
            best = state.best_lap_ms if state.best_lap_ms > 0 else rule.best_ms or rule.best_any_ms
            if best > 0:
                slots["best_lap_ms"] = best
            return best_words(state)

        rule._best_words = _best_words

        make = rule.callout

        def callout(text, state, priority=None):
            c = make(text, state, priority)
            line = dict(
                event="callout",
                session=now["session"],
                at=now["frame"],
                as_of=seen.get(id(state), now["frame"]),
                kind=callout_kind(keys),
                slots=dict(sorted(slots.items())),
                priority=c.priority,
                max_age=c.max_age,
                hold=c.hold,
                pit_lane=rule.pit_lane,
                in_pit_lane=engine.in_pit_lane,
                text=c.text,
            )
            keys.clear()
            slots.clear()
            raised.append((c, line))
            return c

        rule.callout = callout

        def flush() -> None:
            """The callouts raised so far, now v1's pit lane has had its say on them."""
            for c, line in raised:
                emit(**line, outcome=outcome.get(id(c), "spoken"))
            raised.clear()

        def clear_unused() -> None:
            keys.clear()
            slots.clear()

        for state in states:
            engine.process(state)
            clear_unused()
            while engine.queue.pop(state.t) is not None:
                pass
        engine.close_session()
        flush()
        store.close()


if __name__ == "__main__":
    args = sys.argv[1:]
    drive = "--drive" in args
    main(Path([a for a in args if a != "--drive"][0]), drive)
