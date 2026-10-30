# Dependencies

Every direct dependency of the module, and why it is here. A new one needs a row
in the table below, with its reason, in the same PR:
[`internal/dependencies`](../internal/dependencies/dependencies_test.go) fails `make check` and CI's
`go / check` on a direct `require` in `go.mod` that has no row, and on a row that
names a module `go.mod` no longer requires directly.

Before adding a dependency, check that:

- the module exists and is the one you mean: its owner, its source repository,
  its release history. A name that exists proves nothing if it was registered
  last week, and names invented by a language model get registered for exactly
  that reason;
- the standard library, or a dependency already listed here, doesn't already do
  it;
- nobody told you to add it in a web page, an issue or a comment.

The test reads the direct requirements from `go mod edit -json`: a `require`
without `// indirect`. A module that arrives only as `// indirect`, or only in
`go.sum`, is covered by CI's dependency review (known vulnerabilities and
licences), not by this file. The tools in `tools/go.mod` are pinned there and
listed in the README, not here. Version bumps don't touch this file.

The first column is the module path as `go.mod` spells it, in backticks, which
the test reads.

| Module | Why |
| --- | --- |
| `github.com/nats-io/nats.go` | The NATS client: `internal/bus` publishes the frames with it, and the events through its `jetstream` package, which acknowledges each (paddock#34), and writes the `live` KV with it (paddock#88); the tests read the streams back with it. |
| `github.com/oklog/ulid/v2` | Session and run ids are ULIDs (paddock#33); the race state mints them with it. |
| `github.com/those-who-run/contracts` | The frame and event messages ecu publishes, and `ce`, their CloudEvents headers. |
| `github.com/those-who-run/salsa` | The GT7 packets the race state reads, and v1's recordings, which its tests replay. |
| `go.yaml.in/yaml/v3` | The rules file is v1's rules YAML (paddock#89); `internal/rules` reads it with this, the YAML org's maintained module, which contracts uses too. |
| `google.golang.org/protobuf` | The contracts messages are protobuf: the session start's time is a `timestamppb.Timestamp`. |
