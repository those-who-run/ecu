"""Cut the start check's fixture (those-who-run/paddock#74) from the live recording
of 2026-10-29, short-shift-2026-10-29-20261029-213510.jsonl.gz (sha256
a553fc269d3a28840670767874093cae78cbec7b04e01e12f1f58401cf506383, 175,561 frames;
its gzip tail is cut short, so it is read with `zcat ... 2>/dev/null` and the
partial last line dropped). Run it in v1's environment at tag v1.0.0
(those-who-run/those-who-run, `uv sync --extra engineer`):

    zcat RECORDING 2>/dev/null | head -n 175561 | sed -n 133753,148100p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-short-shift-replay-2026-10-29.jsonl.gz

The slice is frames 133753 to 148100 of the recording: the menu after session 4's
results, the race replay the driver watched (frames 134511 to 144497, GT7's
cars-on-track flag off throughout), its menus and loading screens, and the start
of session 5 (frame 147885). It is thinned with v1's own `thin` (every frame
where a tracked field changed, the frames either side of it, one every 0.5 s),
keeping every frame from 12 s to 16 s in, the replay's start and the 3 s v1's
DriverCheck held it for, and from 235 s to 239 s, the start of session 5. The
lines are copied unchanged. v1 at v1.0.0 holds the replay's start on the
fixture's frame 73, judges it a demo on frame 253 ("it came straight from the
menu with no loading screen"), opens its one session on frame 824 and briefs it
on frame 944.
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
    windows = [Window.parse("12:16"), Window.parse("235:239")]
    with gzip.open(dst, "wt") as fh:
        for s in thin(states, every=0.5, windows=windows):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
