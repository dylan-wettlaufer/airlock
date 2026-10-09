# Airlock

A local terminal approval inbox for coding agents, with explainable command warnings and an audit trail.

**Current status: project setup and Milestone 0 hook spike.** The approval daemon, queue, SQLite history, analyzer, and terminal UI are not implemented. Live Cursor execution gating has not been validated. See the [implementation plan](airlock-implementation-plan.md) for the intended V1.

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

The module is named `airlock` until a remote repository is chosen. This initial spike uses only the standard library, so there is no `go.sum`. Add and pin SQLite, Bubble Tea, and `mvdan.cc/sh/v3` when their milestones need them.

## Hook probe

```sh
./bin/airlock hook --agent cursor < testdata/cursor/before-shell-execution.json
# native denial: daemon not implemented

./bin/airlock hook --agent cursor --spike --decision allow \
  < testdata/cursor/before-shell-execution.json

./bin/airlock hook --agent cursor --spike --decision deny --delay 2s \
  < testdata/cursor/before-shell-execution.json
```

`--spike` bypasses future manual review and returns a configured probe response. Use it only in a disposable project for the [compatibility matrix](docs/compatibility.md). The example hook config is deliberately not installed automatically. Normal hook stdout contains exactly one native JSON response; diagnostic messages go to stderr. Explicit failure probes intentionally violate this contract to test Cursor behavior.

## Project layout

```text
cmd/airlock/                 CLI, doctor, fixture demo, hook probe
internal/protocol/          immutable source-fact request type
internal/adapters/cursor/   bounded native JSON input and translation
internal/coordinator/      reserved for pending request lifecycle
internal/analysis/         reserved for shell syntax warnings
internal/store/            reserved for SQLite persistence
internal/tui/              reserved for terminal approval UI
testdata/cursor/            synthetic, sanitized native payload
testdata/commands/          reserved for analyzer cases
examples/cursor-spike/     disposable-project configuration template
docs/                      compatibility gate and milestone tracking
```

The intended flow is hook → private Unix socket → daemon → terminal UI. Authorization and observed execution will be tracked separately. No command execution, rollback, or filesystem preview is provided by this setup.

Next: run the real Cursor integration matrix and record evidence. Build submit/list/decide with two simultaneous waiting hooks only after that gate passes. See [development milestones](docs/development.md).
