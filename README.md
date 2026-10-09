# Airlock

A local terminal approval inbox for coding agents, with explainable command warnings and an audit trail.

**Current status: Milestone 1 in-memory daemon and manual decisions.** Hooks wait for individual decisions through `list` and `decide`. Local process tests prove two concurrent hooks receive their own results. SQLite history, the analyzer, and terminal UI are not implemented. Cursor compatibility is partially validated by user report; approval-requiring commands and the live daemon flow remain unverified. See the [compatibility record](docs/compatibility.md) and [implementation plan](airlock-implementation-plan.md).

## Development setup

On macOS, with `make`, `curl`, and Xcode Command Line Tools available:

```sh
make setup       # installs checksum-verified Go 1.27.2 into .tools/go
make check       # unit/process tests, race detector, vet, formatting
make build       # bin/airlock
make demo        # simulated allow/deny; never executes fixture commands
./bin/airlock doctor
```

If you already have Go 1.27 or later, skip `make setup`; `scripts/go` uses the local toolchain when present, otherwise the Go executable on PATH. Build and module caches stay in the ignored `.cache/` directory. No system-wide Go installation or agent configuration is changed.

The module is named `airlock` until a remote repository is chosen. The daemon uses only the standard library, so there is no `go.sum`. Add and pin SQLite, Bubble Tea, and `mvdan.cc/sh/v3` when their milestones need them. Socket tests need permission to bind local Unix sockets; a restrictive execution sandbox may require running `make check` outside it.

## Manual decision flow

Use three terminals, starting each in this repository:

```sh
# Terminal 1: keep the foreground daemon running
./bin/airlock daemon

# Terminal 2: submits a fixture and waits; no fixture command executes
./bin/airlock submit < testdata/protocol/manual-submit.json

# Terminal 3: inspect the pending proposal, then decide within 25 seconds
./bin/airlock list
./bin/airlock decide simulated-manual deny
```

`submit` reads a protocol request. To exercise native Cursor translation instead, run `./bin/airlock hook --agent cursor < testdata/cursor/before-shell-execution.json` in terminal 2 and use its full generated ID from `list`. Choosing `allow` returns permission to the hook; Airlock never executes the command.

The default socket is `airlock-<uid>/airlock.sock` under the OS temporary directory, displayed by `doctor` and daemon startup. The directory must be owned by you with mode `0700`, and the socket has mode `0600`. Use the same absolute `--socket PATH` on every process when overriding it, or when their temporary-directory environments differ. The daemon creates the immediate parent directory when absent; its ancestors must already exist. Keep custom paths short enough for Unix sockets.

The daemon and submit/hook `--wait` flags default to 25 seconds. The effective deadline is the shorter of the daemon ceiling and the submitter's requested wait. Requests expire to denial without a decision. Clients allow up to two further seconds for transport; the daemon example keeps Cursor's outer timeout at 30 seconds. Longer waits require changing both Airlock settings and Cursor's timeout and validating that configuration live.

The queue holds at most 128 pending requests and disappears on daemon exit. Detected submitter disconnects cancel requests; graceful shutdown returns denial where possible. There is no persistence or result replay. A stale decision fails, and every new invocation needs a fresh ID. A crashed daemon can leave a socket behind; startup refuses to replace any existing path. Confirm the old process has stopped before removing its stale socket manually.

`list --json` returns the pending snapshot as an array. Text output escapes control characters and labels an empty working directory as unknown. `decide` and `submit` return JSON terminal results; a denied or expired submission is a successfully delivered result with exit status 0, while transport/protocol failures exit nonzero. Put flags before positional decision arguments. See the [wire protocol](docs/daemon-protocol.md).

For a disposable Cursor project, copy the built binary and [daemon hook example](examples/cursor-daemon/hooks.json) into that project, then follow the [live smoke test](docs/compatibility.md#manual-daemon-smoke-test). This repository does not install hook configuration automatically.

## Hook probe

```sh
./bin/airlock hook --agent cursor --spike --decision allow \
  < testdata/cursor/before-shell-execution.json

./bin/airlock hook --agent cursor --spike --decision deny --delay 2s \
  < testdata/cursor/before-shell-execution.json
```

`--spike` bypasses manual review and returns a configured probe response. Use it only in a disposable project for the [compatibility matrix](docs/compatibility.md). `--decision`, `--delay`, and `--failure` require `--spike`, including when explicitly set to their default values. Normal hook stdout contains exactly one native JSON response; diagnostic messages go to stderr. Explicit failure probes intentionally violate this contract to test Cursor behavior.

## Project layout

```text
cmd/airlock/                 daemon, hook, submit/list/decide, doctor, demo
internal/protocol/          source-fact requests and bounded NDJSON envelopes
internal/adapters/cursor/   bounded native JSON input and translation
internal/coordinator/      in-memory decisions, deadlines, cancellation
internal/transport/        private Unix-socket server and clients
internal/analysis/         reserved for shell syntax warnings
internal/store/            reserved for SQLite persistence
internal/tui/              reserved for terminal approval UI
testdata/cursor/            synthetic, sanitized native payload
testdata/protocol/          synthetic manual submission
testdata/commands/          reserved for analyzer cases
examples/cursor-spike/     disposable-project configuration template
examples/cursor-daemon/    disposable-project manual decision template
docs/                      compatibility gate and milestone tracking
```

The current flow is hook → private Unix socket → daemon ↔ CLI review. The terminal UI comes later. Authorization is separate from observed execution; no execution observation, rollback, or filesystem preview is provided.

Next: validate the manual flow with two real Cursor conversations and complete the remaining compatibility evidence, then build durable coordination in Milestone 2. See [development milestones](docs/development.md).
