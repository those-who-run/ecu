"""Write the goldens detector_test.go compares with: every change of v1's track
detector over one recording, as JSON lines (those-who-run/paddock#80).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), for each recording the tests replay:

    python3 make_golden.py RECORDING > NAME.jsonl
    python3 make_golden.py --join RECORDING > NAME.joined.jsonl

The table is ecu's internal/track/tracks.csv, which is v1's but for the rows
README.md's "Track detection" lists (Fuji's box, paddock#81): copy it over v1's
src/those_who_run/data/tracks.csv first. The goldens of the eight recordings
whose verdicts that changes (their candidates' order beyond the first, and at
Willow Springs a pool of 9 layouts, not 7) were written again with it, on
2026-10-30; the rest are as v1.0.0's own table gave them.
The Fuji race's, from internal/track/testdata/live/, was written with it too.

Without --join it is what v1's `tracks detect` printed: tracktools.detect_recording,
a fresh TrackDetector fed every frame of the recording. With --join the detector is
told the frames start part way through a lap (TrackDetector.join) before the
first, as v1's engine does for a session it joined part way (#265).

One line per change of the detector's decision (track id and confidence), in
order, then one line for the match after the last frame. frame counts the
recording's frames from 1. candidates are the reported candidates' ids, best
first, and crossed those whose line was crossed.
"""

import json
import sys
from pathlib import Path

from those_who_run.replay import read_recording
from those_who_run.trackdetect import TrackDetector, TrackMatch


def line(frame: int, lap: int, m: TrackMatch, final: bool) -> str:
    return json.dumps(
        {
            "frame": frame,
            "lap": lap,
            "track_id": m.track_id,
            "name": m.name,
            "confidence": m.confidence,
            "family": m.family,
            "candidates": [c.track.id for c in m.candidates],
            "crossed": [c.track.id for c in m.candidates if c.line_crossed],
            "evidence": m.evidence,
            "final": final,
        }
    )


def main(path: Path, join: bool) -> None:
    det = TrackDetector()
    if join:
        det.join()
    n, lap = 0, 0
    for n, state in enumerate(read_recording(path, strict=True), 1):
        lap = state.current_lap
        m = det.observe(state)
        if m is not None:
            print(line(n, lap, m, False))
    print(line(n, lap, det.match, True))


if __name__ == "__main__":
    args = sys.argv[1:]
    join = args[:1] == ["--join"]
    main(Path(args[-1]), join)
