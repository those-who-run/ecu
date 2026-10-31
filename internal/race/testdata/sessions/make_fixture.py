"""Cut the practice fixture (those-who-run/paddock#146, #145) from the live
recording of 2026-10-31, 20261031-092952-full.jsonl.gz (sha256
2cbd5378c82b0dcd6663325b71f7dfd7cb29a8778ffa333e0168b4c4b1712c13; its gzip tail
is cut short, so it is read with `zcat ... 2>/dev/null` and the partial last line
dropped, leaving 52,164 frames). Run it in v1's environment at tag v1.0.0
(those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING 2>/dev/null | head -n 52164 | sed -n 14900,52164p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-suzuka-practice-2026-10-31.jsonl.gz

The recording is a SUGO race's last lap and results, then a practice at Suzuka
in a custom race, with the AI field out on track: the slice is frames 14900 to
52164, from the menu before the practice to the menu after it. The car leaves the
pit lane at its limiter (GT7 reports the field, 16 cars, and no lap total),
starts its first lap on frame 19976 of the recording (5077 of the slice) at
105 km/h, runs four laps ranked by lap time (P1 on the first racing frame, then
P2 to P4 as the AI post their times), pauses three times and quits to the menu.

It is thinned with v1's own `thin` (every frame where a tracked field changed,
the frames either side of it, one every 0.5 s), keeping every frame from 84 s to
88 s in, the start and the 18 packets the start type is read from. The lines are
copied unchanged. make_golden.py writes what v1 reads on the fixture
(../kind/make_golden.py for the kind).
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
        for s in thin(states, every=0.5, windows=[Window.parse("84:88")]):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
