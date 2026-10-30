"""Cut the Fuji race's fixture (those-who-run/paddock#81) from the live recording
of 2026-10-29, short-shift-2026-10-29-20261029-213510.jsonl.gz (sha256
a553fc269d3a28840670767874093cae78cbec7b04e01e12f1f58401cf506383, 175,561 frames;
its gzip tail is cut short, so it is read with `zcat ... 2>/dev/null` and the
partial last line dropped). Run it in v1's environment at tag v1.0.0
(those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING 2>/dev/null | head -n 175561 | sed -n 97871,133753p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-short-shift-fuji-race-2026-10-29.jsonl.gz

The slice is frames 97871 to 133753 of the recording: the loading screen onto
the grid, session 4 (a 3-lap race at Fuji International Speedway, in race from
frame 98626, the chequered flag on frame 131384, a pit cut-scene on frame
106291), its results and the first menu frame after them. The lap counter goes
up on frames 110189, 121620 and 131384, 0 m from Fuji's finish line, heading
+x. It is thinned with v1's own `thin` (every frame where a tracked field
changed, the frames either side of it, one every 0.5 s), and the lines are
copied unchanged. The thinned path still reaches x 667.1 on lap 1, beyond
v1.0.0's box for Fuji (x 615.2, Fuji Short 615.7) and its 50 m tolerance: v1's
detector rules both out and says "no layout fits" on the fixture's frame 476,
the first lap boundary; with the table's Fuji rows widened to x 670 it says
"probable Fuji International Speedway" there.
"""

import gzip
import sys
from pathlib import Path

from those_who_run.replay import read_recording, thin


def main(src: Path, dst: Path) -> None:
    lines = src.read_text().splitlines()
    states = list(read_recording(src, strict=True))
    assert len(states) == len(lines)
    index = {id(s): i for i, s in enumerate(states)}
    with gzip.open(dst, "wt") as fh:
        for s in thin(states, every=0.5):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
