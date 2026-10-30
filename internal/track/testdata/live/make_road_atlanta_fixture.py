"""Cut the Road Atlanta race's fixture (those-who-run/paddock#81) from the live
recording of 2026-10-30, short-shift-2026-10-30-20261030-123308.jsonl.gz (sha256
81dcc6613750a61528d6337d5176add0ecb505ccb0eb0f252ceaa52760d1446e, 555,744 frames;
its gzip tail is cut short, so it is read with `zcat ... 2>/dev/null` and the
partial last line dropped). Run it in v1's environment at tag v1.0.0
(those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING 2>/dev/null | head -n 555744 | sed -n 485150,512900p > slice.jsonl
    python3 make_road_atlanta_fixture.py slice.jsonl real-nuc-short-shift-road-atlanta-race-2026-10-30.jsonl.gz

The slice is frames 485150 to 512900 of the recording: the results of the race
before, a GT7 demo race at another circuit (in race from frame 485183, GT7's
cars-on-track flag off, judged a demo on frame 485278, "it came straight from the
menu with no loading screen"), the menus and loading screens after it, the
formation lap at Michelin Raceway Road Atlanta (loading onto the grid on frame
508485), the start of the 7-lap race (lap 1 on frame 508860, on Road Atlanta's
finish line) and lap 1 up to and past frame 512836, where the path cuts
Sardegna - Road Track - A's line heading its way. It is thinned with v1's own
`thin` (every frame where a tracked field changed, the frames either side of it,
one every 0.5 s), keeping every frame from 0 s to 3 s in (the demo's start and
its hold), from 396 s to 404 s (the grid to the start) and from 469 s to 470 s
(the crossing of Sardegna's line). The lines are copied unchanged.

v1 at v1.0.0, which feeds its track detector only the frames it steps, never
shows it the frames after the demo: on the fixture it opens its one session on
the race's start, ambiguous ("... layouts near the start still fit"), and says
"probable Sardegna - Road Track - A" at the crossing, as ecu 0.1.9 did live.
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
    windows = [Window.parse("0:3"), Window.parse("396:404"), Window.parse("469:470")]
    with gzip.open(dst, "wt") as fh:
        for s in thin(states, every=0.5, windows=windows):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
