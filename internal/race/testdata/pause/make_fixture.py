"""Cut the pause fixture (those-who-run/paddock#77, AC2) from the live recording
of 2026-10-29, short-shift-2026-10-29-20261029-213510.jsonl.gz (sha256
a553fc269d3a28840670767874093cae78cbec7b04e01e12f1f58401cf506383, 175,561 frames;
its gzip tail is cut short, so it is read with `zcat ... 2>/dev/null` and the
partial last line dropped). Run it in v1's environment at tag v1.0.0
(those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING 2>/dev/null | head -n 175561 | sed -n 80537,81620p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-short-shift-pauses-2026-10-29.jsonl.gz

The slice is frames 80537 to 81620 of the recording: lap 6 of session 2, a timed
race, from its line crossing; the pause on it (frame 81224, 11.5 s in), which GT7
flags for 1.3 s and then sends as a frozen stream for about 3 s; the quit to the
menu from the pause menu (frame 81478); and the track screen and loading frames
after it. The fixture's session is joined part way, on lap 6. It is thinned with
v1's own `thin` (every frame where a tracked field changed, the frames either
side of it, one every 0.5 s), keeping every frame from 10 s to 17 s in, so the
pause and its frozen stream are whole. The lines are copied unchanged.

On the whole recording v1 at v1.0.0 reads two pauses there, in its session 2:
flagged on frame 81224, resumed on 81303, then frozen from 81303, read on 81364,
and resumed on 81478 (2,913 ms), 4,238 ms paused in all. pause/make_golden.py
writes what v1 reads on the fixture.
"""

import gzip
import sys
from pathlib import Path

from those_who_run.replay import Window, read_recording, thin


def main(src: Path, dst: Path) -> None:
    lines = src.read_text().splitlines()
    states = list(read_recording(src, strict=True))
    assert len(states) == len(lines)
    index = {id(s): i for i, s in enumerate(states)}
    with gzip.open(dst, "wt") as fh:
        for s in thin(states, every=0.5, windows=[Window.parse("10:17")]):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
