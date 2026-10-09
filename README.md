# Airlock

A local terminal approval inbox for coding agents, with explainable command warnings and an audit trail.

**Current status: manual decisions with persistent SQLite history.** Hooks wait for individual decisions through `list` and `decide`. Local process tests prove two concurrent hooks receive their own results. Live concurrent review, expiry, offline fallback, and a native Cursor prompt after Airlock allowance are now user-reported. Cursor can require its own approval after Airlock approval. SQLite history is implemented; the analyzer and terminal UI remain planned. The full compatibility gate remains partial pending configuration details and remaining failure observations. See the [compatibility record](docs/compatibility.md) and [implementation plan](airlock-implementation-plan.md).

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

The module is named `airlock` until a remote repository is chosen. The SQLite store uses the pinned, pure Go `modernc.org/sqlite` driver with dependencies recorded in `go.mod` and `go.sum`. Bubble Tea and `mvdan.cc/sh/v3` remain planned. Socket tests need permission to bind local Unix sockets; a restrictive execution sandbox may require running `make check` outside it.

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

The queue holds at most 128 pending requests. Detected submitter disconnects cancel requests; graceful shutdown returns denial where possible. Requests are recorded in SQLite before review. Normal terminal transitions commit their state and event atomically before returning a decision acknowledgment or hook result. On restart, unfinished records become interrupted denials; they are never restored to the approval queue. There is no result replay. A stale decision fails, and every invocation needs an ID that has never appeared in that database. A crashed daemon can leave a socket behind. Startup acquires a private exclusive socket lock, verifies that the old endpoint is a user-owned `0600` socket with no live listener, completes database migration/recovery, then removes that verified stale socket and binds. Active listeners, unsafe files or permissions, and endpoints changed during startup are refused. Database setup failures leave the old socket untouched.

When no daemon is running at the configured socket, the Cursor hook immediately steps aside with a hook-level `permission: "allow"`; Cursor's native permission settings determine whether to prompt or execute. This applies to a missing socket or a private, correctly permissioned stale socket with no listener. It is not a recorded Airlock approval. A connected request still denies on timeout, cancellation, daemon shutdown/crash, or invalid responses. Unsafe socket paths and invalid hook input/options also deny. The standalone `submit`, `list`, and `decide` commands still report an error if the daemon is absent. Keep `failClosed: true` for hook execution failures. Native approval preservation must be tested on your installed Cursor configuration with a command that prompts when the hook is disabled.

`list --json` returns the pending snapshot as an array. Text output escapes control characters and labels an empty working directory as unknown. `decide` and `submit` return JSON terminal results; a denied or expired submission is a successfully delivered result with exit status 0, while transport/protocol failures exit nonzero. Put flags before positional decision arguments. See the [wire protocol](docs/daemon-protocol.md).

For a disposable Cursor project, copy the built binary and [daemon hook example](examples/cursor-daemon/hooks.json) into that project, then follow the [live smoke test](docs/compatibility.md#manual-daemon-smoke-test). This repository does not install hook configuration automatically.

## Persistent history

```sh
./bin/airlock history --limit 20
./bin/airlock history --state denied --json
./bin/airlock history --agent cursor --conversation CHAT_ID
./bin/airlock history --request REQUEST_ID --json
```

The default database is `$XDG_STATE_HOME/airlock/history.sqlite3`, or `$HOME/.local/state/airlock/history.sqlite3` when `XDG_STATE_HOME` is unset. It lives separately from the temporary socket directory and remains after socket cleanup. To override it, pass the same absolute `--database PATH` to `daemon` and `history`; hook clients need only the socket path. `doctor` reports the default history path.

History works while the daemon is running or stopped. It opens an existing database read-only, without creating files or recovering pending records. Startup applies checked-in, transactional migrations using SQLite's `user_version`; a newer schema is rejected. An exclusive `SOCKET.lock` is held before database recovery and through shutdown. A separate database lock permits one daemon per database, including when sockets differ. Lock files remain on disk; their presence does not mean a daemon is running, and they must not be deleted while a daemon holds them. Process exit, including a crash, releases the locks. State directories must be owned by you with mode `0700`, and database, lock, and SQLite sidecar files use `0600`. Unsafe existing modes and symlinks in the immediate directory or database files are rejected.

Records include agent/conversation sessions, native request metadata, workspace roots and working directory, receipt/deadline/terminal timestamps, manual decisions with source and reason, and request lifecycle events. Execution remains `unobserved`: an authorization record does not prove that the hook received it or the command ran. The first storage failure immediately denies every pending waiter, empties the review queue, and rejects further submissions and decisions with `history_unavailable` until the daemon is restarted after the storage issue is resolved. These safety denials may lack a committed terminal record; any history still pending is interrupted on restart. A failed or ambiguous write never produces a hook allowance.

Exact commands stay in memory for pending review. History retains only the executable basename followed by `[arguments redacted]`; assignment-leading commands and complex first tokens become `[command redacted]`. This conservative display removes all arguments and subsequent shell syntax. Redaction is best effort: executable names, native IDs, and path metadata can still be sensitive. No terminal output or transcripts are collected. The pending `list` command continues to show exact commands for review.

Only one terminal transition wins, including competing decisions, expiry, cancellation, and shutdown. Repeated decisions return `not_pending` for either the same or opposite choice, including after restart; they never alter history or replay approval. Identical and conflicting resubmissions both return `duplicate_request` without changing the original proposal.

Queries return newest requests first, with a request-ID tie break. The default limit is 50, the maximum is 200, and offsets are bounded to 0–10000. Filters match exact request ID, agent, conversation, or state. JSON includes decision and event details; text escapes terminal control characters. Query limits bound each response; automatic retention is still planned before release.

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
cmd/airlock/                 daemon, hook, submit/list/decide/history, doctor, demo
internal/protocol/          source-fact requests and bounded NDJSON envelopes
internal/adapters/cursor/   bounded native JSON input and translation
internal/coordinator/      decisions, durable transitions, deadlines, cancellation
internal/transport/        private Unix-socket server and clients
internal/analysis/         reserved for shell syntax warnings
internal/store/            SQLite history, embedded migrations, redacted display
internal/tui/              reserved for terminal approval UI
testdata/cursor/            synthetic, sanitized native payload
testdata/protocol/          synthetic manual submission
testdata/commands/          reserved for analyzer cases
examples/cursor-spike/     disposable-project configuration template
examples/cursor-daemon/    disposable-project manual decision template
docs/                      compatibility gate and milestone tracking
```

The current flow is hook → private Unix socket → daemon ↔ CLI review. The terminal UI comes later. Authorization is separate from observed execution; no execution observation, rollback, or filesystem preview is provided.

Next: complete the remaining compatibility evidence and Milestone 2 retention work. See [development milestones](docs/development.md).
