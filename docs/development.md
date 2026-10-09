# Development milestones

Source of scope and design: [implementation plan](../airlock-implementation-plan.md).

## Completed setup

- Go module, planned package boundaries, local toolchain bootstrap, Make targets, and CI checks.
- Bounded Cursor payload decoder and immutable proposal translation with fresh invocation IDs.
- Native JSON hook probe with allow/deny, delay, intentional failure modes, and cancellation denial.
- Normal hook denial until a real authorization coordinator is implemented.
- Synthetic fixture, no-account demo, process tests, and a live compatibility checklist.

## Remaining gates

0. **Integration spike:** run and document the live Cursor matrix. Setup alone does not complete this milestone.
1. **Vertical slice:** foreground daemon; versioned bounded NDJSON over a private Unix socket; submit/list/decide CLI; two concurrent waiting hooks with correctly routed results.
2. **Reliable coordinator:** conditional durable decisions, SQLite migrations, deadlines, cancellation, conflicting/duplicate IDs, interrupted requests after restart, permissions, daemon lock, backpressure, retention and redaction.
3. **TUI:** pin compatible Bubble Tea packages; queue/details/countdowns, stable selection, decisions, snapshot/subscription sequencing and reconnect.
4. **Analysis:** pin the shell syntax parser; walk full ASTs, implement warning families, label candidate paths and unresolved effects, bound resources, never evaluate input.
5. **Release:** real compatibility smoke tests, installation/doctor, fixture queue demo and history, CI, measured stress experiment, and tagged binaries.

Do not add placeholders that silently allow commands or imply execution success. Maintain explicit implementation status in the README. Add SQLite and UI dependencies when their first working flows are built; avoid a plugin framework in V1.

## Checks

`make check` runs unit/process tests, `go test -race ./...`, `go vet ./...`, and formatting verification. `make build` produces the single CLI executable. `make demo` runs a labeled simulation without a live agent account. GitHub Actions repeats checks, build, and demo on macOS.

The first socket milestone must add tests for concurrent routing, dropped hooks, malformed/oversized frames, deadline races, and competing decisions. The compatibility gate must be backed by real-agent evidence before support is claimed.
