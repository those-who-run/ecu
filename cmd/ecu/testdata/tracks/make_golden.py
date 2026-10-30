"""Write the goldens tracks_test.go compares with: what v1's `tracks detect` and
`tracks learn` print over a recording (those-who-run/paddock#300).

Made once from v1 at tag v1.0.0 (those-who-run/those-who-run), in v1's own
environment (`uv sync --extra engineer`), with ecu's internal/track/tracks.csv
copied over v1's src/those_who_run/data/tracks.csv (v1's but for the rows
README.md's "Track detection" lists), for each recording internal/track's
goldens cover:

    python3 make_golden.py detect RECORDING > NAME.detect.txt
    python3 make_golden.py learn RECORDING > NAME.learn.txt

Each file is the command's standard output, unchanged, then one line `exit N`
with its exit code. learn is made for the recordings the tests name.
"""

import subprocess
import sys

cmd, path = sys.argv[1], sys.argv[2]
p = subprocess.run(["those-who-run", "tracks", cmd, path], capture_output=True, text=True, check=False)
sys.stdout.write(p.stdout)
print(f"exit {p.returncode}")
