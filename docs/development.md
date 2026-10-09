# Development milestones

Source of scope and design: [implementation plan](../airlock-implementation-plan.md).

## Completed setup

- Go module, planned package boundaries, local toolchain bootstrap, Make targets, and CI checks.
- Bounded Cursor payload decoder and immutable proposal translation with fresh invocation IDs.
- Native JSON hook probe with allow/deny, delay, intentional failure modes, and cancellation denial.
- Normal hook submission to the daemon, deferral to Cursor's native permissions when no daemon is running, and denial for connected-request failures.
- Synthetic fixture, no-account demo, process tests, and a live compatibility checklist.
- Milestone 1 foreground daemon, private Unix socket, bounded versioned NDJSON, and submit/list/decide CLI.
- In-memory deadlines, disconnect cancellation, competing-decision protection, queue bounds, and health reporting.
- Two concurrent hook processes with independently routed allow/deny, plus process-level expiry, cancellation, shutdown, and crash denial tests.

- Persistent private SQLite history, versioned transactional migrations, agent/conversation sessions, decisions and lifecycle events, conservative command redaction, durable unique request IDs, and bounded `history` queries.
- Atomic terminal audit writes before acknowledgment or hook allowance, one winner across competing transitions, explicit duplicate-decision rejection, single-writer process locking, and interruption of pending records on restart.
- Immediate denial of every pending waiter after admission or terminal persistence failure; gated-commit tests and injected SQLite COMMIT failures validate visibility, rollback, and socket responses.
- Exclusive socket ownership before database recovery, safe stale-socket replacement after successful recovery, startup failure cleanup, and process tests for crash/restart on the same endpoint, preserved completed history, and fresh hook decisions.

- Automatic completed-history retention by age and count, version-2 migration backfilling a permanent ID registry, pruning of dependent metadata, hourly idle maintenance, and transaction rollback/duplicate protection tests.

- Fixed-seed, 100-request reliability harness with isolated sockets/SQLite, conflicting decisions, disconnects, expiry, real process crash/restart, audit agreement and permanent ID checks after count/age pruning. See [measured results and rerun command](reliability.md).
- October 9 user-reported manual Airlock checks: redaction, duplicate-decision rejection, same-socket crash recovery, completed-history preservation, count/age pruning and rejection of pruned IDs. These observations do not complete native Cursor validation.

- Atomic stamped queue snapshots and bounded subscriptions, with process-lifetime epochs, contiguous admission/terminal events, commit-before-event publication, disconnect cleanup and a validated transport client. Tests cover snapshot/decision races, mutable-field isolation, overflow without blocked decisions, subscriber limits, commit gates/failure, large snapshots, idle cancellation, invalid streams and fresh synchronization after reconnect/restart.

## Remaining gates

0. **Integration spike:** run and document the live Cursor matrix. Setup alone does not complete this milestone.
1. **Vertical slice: implemented and locally verified, with live user-reported coverage.** Foreground daemon; versioned bounded NDJSON over a private Unix socket; submit/list/decide CLI; two concurrent waiting hooks with correctly routed results. The October 9 report covers concurrent live review, expiry, offline fallback, denial of pending commands on shutdown, and a native approval prompt after Airlock allowance. The full compatibility gate still needs configuration details, session cancellation process-exit confirmation, and measured timing.
2. **Reliable coordinator: SQLite history implemented and locally tested.** Durable transitions, migrations, deadlines, cancellation, unique IDs across restarts, recovery, private files, single-writer locking, and redaction are implemented. Age/count retention with permanent ID reservation is implemented. The 100-request stress experiment and isolated process restart validation pass. Native Cursor crash/restart evidence remains separate.
3. **TUI:** snapshot/subscription support with sequencing is implemented. Next pin compatible Bubble Tea packages and build queue/details, stable selection, countdowns, keyboard decisions and reconnect behavior (invalidate cached state, disable decisions while disconnected, retry with backoff, replace state on a fresh snapshot).
4. **Analysis:** pin the shell syntax parser; walk full ASTs, implement warning families, label candidate paths and unresolved effects, bound resources, never evaluate input.
5. **Release:** real compatibility smoke tests, installation/doctor, fixture queue demo and history, CI, measured stress experiment, and tagged binaries.

Do not add placeholders that silently allow commands or imply execution success. Maintain explicit implementation status in the README. Add SQLite and UI dependencies when their first working flows are built; avoid a plugin framework in V1.

## Checks

`make check` runs unit/process tests, `go test -race ./...`, `go vet ./...`, and formatting verification. `make stress` reruns the isolated experiment without caching and prints measured JSON. `make build` produces the single CLI executable. `make demo` runs a labeled simulation without a live agent account. GitHub Actions repeats checks, build, and demo on macOS.

The socket milestone includes tests for concurrent routing, dropped hooks, malformed/oversized frames, deadline races, and competing decisions. `make check` verifies formatting without rewriting source files. The compatibility gate must be backed by real-agent evidence before support is claimed. Development proceeded to Milestone 1 with the user's explicit acceptance of partial compatibility evidence; native approval after Airlock allowance is now observed by user report for the tested proposal, with broader coverage and exact configuration still open.
