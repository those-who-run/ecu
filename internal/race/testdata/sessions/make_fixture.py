"""Cut the live sessions' fixtures (those-who-run/paddock#145) from ecu's live
recordings, in v1's environment at tag v1.0.0 (those-who-run/those-who-run,
`uv sync --extra engineer`):

    python3 make_fixture.py SLICE FIXTURE WINDOW

It thins SLICE with v1's own `thin` (every frame where a tracked field changed,
the frames either side of it, one every 0.5 s), keeping every frame of WINDOW
(seconds from the slice's first frame, START:END), and copies the lines it keeps
unchanged. make_golden.py writes what v1 reads on a fixture
(../kind/make_golden.py for the kind).

The practice (paddock#146) is cut from the live recording of 2026-10-31,
20261031-092952-full.jsonl.gz (sha256
2cbd5378c82b0dcd6663325b71f7dfd7cb29a8778ffa333e0168b4c4b1712c13; its gzip tail
is cut short, so it is read with `zcat ... 2>/dev/null` and the partial last line
dropped, leaving 52,164 frames):

    zcat RECORDING 2>/dev/null | head -n 52164 | sed -n 14900,52164p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-suzuka-practice-2026-10-31.jsonl.gz 84:88

The recording is a SUGO race's last lap and results, then a practice at Suzuka
in a custom race, with the AI field out on track: the slice is frames 14900 to
52164, from the menu before the practice to the menu after it. The car leaves the
pit lane at its limiter (GT7 reports the field, 16 cars, and no lap total),
starts its first lap on frame 19976 of the recording (5077 of the slice) at
105 km/h, runs four laps ranked by lap time (P1 on the first racing frame, then
P2 to P4 as the AI post their times), pauses three times and quits to the
menu. The window, 84 s to 88 s in, is the start and the 18 packets the start
type is read from.

The grid start (paddock#148) is cut from ecu 0.1.13's live recording of
2026-10-31, 20261031-163516.jsonl.gz (sha256
bf5cf55b84aeefa8607176581adcdf44bad6b1461b36fcb2c6686ec467f3f60b; its gzip tail
is cut short too, leaving 32,254 frames):

    zcat RECORDING 2>/dev/null | head -n 32254 | sed -n 6683,29501p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-suzuka-east-grid-start-2026-10-31.jsonl.gz 15:21

The recording is a custom race at Suzuka East, 5 laps against 19 AI cars with
GT7's "Grid Start", one pit stop, finished P9: the slice is frames 6683 to 29501,
from the menu before the race to the menu after its results. The car is put on
the grid in slot 10 (frame 7260 of the recording, 578 of the slice) and stands
there at 0 km/h for 6.1 s, then launches from 0 km/h (frame 7632, 950 of the
slice); the grid is behind the line, so lap 1 begins at speed, 97 km/h, 4.7 s
after the launch (frame 7909, 1227 of the slice). The window, 15 s to 21 s in, is
the launch, the start and the 18 packets after it.

The evening practice and qualifying (the ranked kind, paddock#146) are cut from
ecu 0.1.14's live recording of 2026-10-31, 20261031-173249.jsonl.gz (sha256
e7fef25cae42afd3cd993dec90c3f8f9bff8871cb8eba268a7c032ef02af70c9; its gzip tail
is cut short, leaving 177,822 frames), a custom race at Suzuka in the Alfa Romeo
Giulia Sprint GT Veloce '67 against 15 AI cars, fuel use off:

    zcat RECORDING 2>/dev/null | sed -n 104721,152195p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-suzuka-practice-evening-2026-10-31.jsonl.gz 33:37
    zcat RECORDING 2>/dev/null | sed -n 152195,176470p > slice.jsonl
    python3 make_fixture.py slice.jsonl real-nuc-suzuka-qualifying-2026-10-31.jsonl.gz 33:37

The practice's slice runs from the loading screen before it to the one before the
qualifying: the car leaves the pits and starts lap 1 on frame 106523 (1803 of
the slice) in slot 16, P2 on the next packet, four laps, three pauses, a quit.
The qualifying's runs from that loading screen to the track screen after it:
lap 1 on frame 154270 (2076 of the slice) in slot 5, P1 on the next packet, two
laps and back to the pits. Each window, 33 s to 37 s in, is the start and the 18
packets after it.
"""

import gzip
import sys
from pathlib import Path

from those_who_run.replay import Window, read_recording, thin


def main(src: Path, dst: Path, window: str) -> None:
    lines = src.read_text().splitlines()
    states = list(read_recording(src, strict=True))
    assert len(states) == len(lines)
    index = {id(s): i for i, s in enumerate(states)}
    with gzip.open(dst, "wt") as fh:
        for s in thin(states, every=0.5, windows=[Window.parse(window)]):
            fh.write(lines[index[id(s)]] + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3])
