# v1 recordings

Outing recordings copied unchanged from v1,
[those-who-run/those-who-run](https://github.com/those-who-run/those-who-run) at
tag `v1.0.0`, `tests/fixtures/`, for the track detector's tests
(paddock#80). They are GPL-3.0, like `ecu`. Each is a v1 fixture, a real drive
cut down with v1's `those-who-run thin`; its entry in v1's
[`FIXTURES.yaml`](https://github.com/those-who-run/those-who-run/blob/v1.0.0/tests/fixtures/FIXTURES.yaml)
says what was driven and what it pins.

[`FIXTURES.yaml`](FIXTURES.yaml) here lists every file with its v1 path, its
sha256, its frame count and a link to that entry. `TestV1FixtureSums`
(`fixtures_test.go`) fails when a file and its entry disagree.

These are the 18 of v1's 25 recordings that `salsa` doesn't carry. The other
seven are salsa's `testdata/v1/`, which the tests read from the salsa module the
build already uses, as `race`'s do, so ecu keeps no second copy of them.
Together they are every recording v1's `FIXTURES.yaml` lists, and
[`../golden/`](../golden) has v1's verdicts on each.
