# Development milestones

Source of scope and design: [implementation plan](../airlock-implementation-plan.md).

## Completed setup

- Go module, planned package boundaries, local toolchain bootstrap, Make targets, and CI checks.
- Bounded Cursor payload decoder and immutable proposal translation with fresh invocation IDs.
- Native JSON hook probe with allow/deny, delay, intentional failure modes, and cancellation denial.
- Normal hook submission to the daemon, with native denial when authorization is unavailable.
- Synthetic fixture, no-account demo, process tests, and a live compatibility checklist.
- Milestone 1 foreground daemon, private Unix socket, bounded versioned NDJSON, and submit/list/decide CLI.
- In-memory deadlines, disconnect cancellation, competing-decision protection, queue bounds, and health reporting.
- Two concurrent hook processes with independently routed allow/deny, plus process-level expiry, cancellation, shutdown, and crash denial tests.

## Remaining gates

0. **Integration spike:** run and document the live Cursor matrix. Setup alone does not complete this milestone.
1. **Vertical slice: implemented and locally verified.** Foreground daemon; versioned bounded NDJSON over a private Unix socket; submit/list/decide CLI; two concurrent waiting hooks with correctly routed results. The live Cursor daemon smoke test remains pending.
2. **Reliable coordinator:** conditional durable decisions, SQLite migrations, deadlines, cancellation, conflicting/duplicate IDs, interrupted requests after restart, permissions, daemon lock, backpressure, retention and redaction.
3. **TUI:** pin compatible Bubble Tea packages; queue/details/countdowns, stable selection, decisions, snapshot/subscription sequencing and reconnect.
4. **Analysis:** pin the shell syntax parser; walk full ASTs, implement warning families, label candidate paths and unresolved effects, bound resources, never evaluate input.
5. **Release:** real compatibility smoke tests, installation/doctor, fixture queue demo and history, CI, measured stress experiment, and tagged binaries.

Do not add placeholders that silently allow commands or imply execution success. Maintain explicit implementation status in the README. Add SQLite and UI dependencies when their first working flows are built; avoid a plugin framework in V1.

## Checks

`make check` runs unit/process tests, `go test -race ./...`, `go vet ./...`, and formatting verification. `make build` produces the single CLI executable. `make demo` runs a labeled simulation without a live agent account. GitHub Actions repeats checks, build, and demo on macOS.

The socket milestone includes tests for concurrent routing, dropped hooks, malformed/oversized frames, deadline races, and competing decisions. `make check` verifies formatting without rewriting source files. The compatibility gate must be backed by real-agent evidence before support is claimed. Development proceeded to Milestone 1 with the user's explicit acceptance of partial compatibility evidence; approval-requiring commands remain an open check.
