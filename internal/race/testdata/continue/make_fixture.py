"""Cut the lap 1 restart's fixture from the live recording of 2026-10-31 at SUGO,
20261031-092617.jsonl.gz (sha256
369d6c8fbcbfed3e6e4e742ffa5acda9a528862658c745899ada7183f28af84c, 5,098 frames):
the run of ecu 0.1.11 that came up on lap 1 of a three-lap race, 40 s into the
lap at 71 km/h, after a restart that kept the session the run before it
(20261031-092315) had open, with SUGO found. Run it in v1's environment at tag
v1.0.0 (those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING > run.jsonl
    python3 make_fixture.py run.jsonl real-nuc-sugo-restart-lap1-2026-10-31.jsonl.gz

It is the whole run, thinned with v1's own `thin` (every frame where a tracked
field changed, the frames either side of it, one every 0.1 s), and the lines are
copied unchanged. Seen from its start with no record, as v1.0.0 sees it, the
detector counts the car's path across Streets of Willow Springs Reverse's
finish line (probable, 12.6 s in), then Fuji International Speedway's, 98 m
from SUGO's (probable, 81.9 s in), and at the lap 2 tick (84.9 s) judges the
partial lap 1 as whole, rules SUGO out on its length and ends on none.
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
        for s in thin(states, every=0.1):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
