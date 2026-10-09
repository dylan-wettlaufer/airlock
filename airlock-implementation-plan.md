# Airlock: implementation plan and portfolio review

Prepared October 8, 2026. This is a proposed design, informed by the supplied outline and current primary documentation. The integrations have not been exercised against the installed agents.

## Recommendation

Build Airlock. It is a strong junior SWE portfolio idea, especially for backend, infrastructure, platform, and developer tooling roles. Its value comes from coordinating real concurrent requests, handling failure correctly, and explaining design tradeoffs. A finished, reliable approval dashboard will demonstrate more than an unfinished dashboard plus rollback system.

Keep the name Airlock for now. Branding can wait until release.

Use this product description:

> A local terminal approval inbox for coding agents, with explainable command warnings and an audit trail.

Remove “safely preview” from the initial promise: a predicted list of paths is analysis, while a preview implies observing what execution would actually do. Treat agent independence as a design direction until two real adapters work.

## What to change in the original outline

| Original choice | Recommendation | Reason |
| --- | --- | --- |
| Snapshots and undo required for V1 | Move to a separate experimental release | Capturing files is straightforward compared with attributing and restoring concurrent changes |
| Five weekends for the whole project | Plan milestones in hours and reserve integration time | Hook behavior and recovery work are uncertain |
| Large shell risk catalog | Start with 6–8 explainable rule families | A small, tested analyzer is easier to trust and discuss |
| `Approved → Executed` in one lifecycle | Separate authorization from observed execution | Allowing a hook does not prove that the agent ran the command |
| Estimated `files` in the incoming request | Put path estimates in daemon-generated analysis | Integration metadata and analysis should have separate ownership |
| Agent-agnostic architecture as the main differentiator | Lead with a useful multi-session workflow | Extensibility only becomes user value after another integration ships |
| Custom policy engine early | Manual decisions first; narrow opt-in rules later | Correct request coordination is the first engineering problem |

## Integration feasibility: the first gate

Cursor documents `beforeShellExecution`, configurable timeout seconds, `failClosed`, and permission responses. Its generic `preToolUse` also supplies tool IDs, but its `ask` behavior differs. Specialized post-shell payloads do not establish reliable per-call correlation. Select one approval hook surface after testing; do not register both for shell approval. [Cursor hooks reference](https://cursor.com/docs/hooks)

Run a disposable-project spike before building the daemon:

1. Record the Cursor version, local IDE mode, configured shell, and permission settings.
2. Write a tiny Go hook that reads one JSON input, writes one JSON response, and logs diagnostics to stderr.
3. Use harmless commands such as `printf 'airlock-test\n'` and writes only inside the disposable project.
4. Check immediate allow, immediate deny, delayed allow, and delayed deny.
5. Check non-zero exit, missing executable, malformed output, empty output, and timeout with fail-closed enabled.
6. Cancel a session while the hook waits; observe whether the process and its socket are closed.
7. Run two sessions simultaneously. Capture sanitized payload fixtures and identify stable conversation and optional tool-call IDs.
8. Check whether an Airlock allow is followed by another native permission prompt. Repeat for sandboxed and approval-requiring commands.
9. Measure the usable human decision window. Set the eventual Airlock deadline below the outer hook timeout, leaving time to return a denial.

Pass condition: denial consistently prevents the tested commands, waiting is usable, and failure behavior is understood for that exact version and configuration. Save the evidence in `docs/compatibility.md`.

If centralized approval is impractical, stop expanding that adapter. Either test another agent or release a clearly labeled monitoring-only variant. Avoid a fragile permission-control claim.

Codex is a credible later adapter: current documentation describes `PreToolUse` interception and `PermissionRequest` allow/deny decisions. The latter runs only when native approval is needed; these are different coverage levels. Hook trust, tool coverage, timeout, and failure behavior need their own compatibility spike. [Official OpenAI hooks documentation](https://learn.chatgpt.com/docs/hooks)

An app-server client is another integration route, but choose it deliberately if Airlock should own a Codex client/session workflow; it should not be assumed to transparently intercept every existing UI session. [Official OpenAI app-server documentation](https://learn.chatgpt.com/docs/app-server)

## V1 scope

Ship one local macOS integration with:

- A foreground daemon and separately launched terminal UI.
- A queue for two or more concurrent agent conversations.
- Individual allow/deny decisions, visible deadlines, and explicit failure reasons.
- Explainable shell warnings and labeled candidate paths.
- SQLite history for requests, decisions, and supported observed outcomes.
- A fixture-driven demo that needs no live agent account.
- Installation instructions, compatibility notes, tests, and a tagged release.

Defer rollback, sandbox execution, cost tracking, arbitrary SQL analysis, cloud agents, session-wide approvals, background service installers, and additional platforms. Monitor direct file edits later; V1 shell approval does not cover every way an agent can change files.

## Proposed architecture

```text
Agent hook
    ↓ stdin JSON
airlock hook --agent cursor
    ↕ private Unix socket
airlock daemon
    ├─ request coordinator
    ├─ command analyzer
    ├─ SQLite store
    └─ subscriptions
           ↕ Unix socket
       airlock tui
```

Package one Go executable with `daemon`, `hook`, `tui`, `doctor`, and `demo` subcommands. This simplifies installation while preserving separate processes. Start the daemon manually during development.

Use a small layout:

```text
cmd/airlock/
internal/protocol/
internal/adapters/cursor/
internal/coordinator/
internal/analysis/
internal/store/
internal/tui/
testdata/cursor/
testdata/commands/
docs/
```

Keep transport and adapter translation outside the coordinator. Introduce interfaces where tests or another adapter need a boundary; avoid building a plugin framework before it has a second consumer.

Go and Unix sockets fit the workload. Keep SQLite and Bubble Tea from the original stack. Pin mutually compatible UI package versions and use one version's examples consistently; the current Bubble Tea README uses the v2 import path. [Bubble Tea repository](https://github.com/charmbracelet/bubbletea)

Use `mvdan.cc/sh/v3/syntax` for syntax analysis, never its interpreter to evaluate an untrusted command. Explicitly choose the applicable dialect. Current Zsh parsing support is documented as experimental and incomplete, so unsupported syntax must produce incomplete analysis. [Shell parser documentation](https://pkg.go.dev/mvdan.cc/sh/v3/syntax)

## Request and decision model

The hook sends source facts:

```json
{
  "protocol_version": 1,
  "request_id": "unique-per-hook-invocation",
  "agent": "cursor",
  "agent_version": "captured-version",
  "conversation_id": "native-conversation-id",
  "source_tool_call_id": null,
  "event": "before_shell_execution",
  "command": "rm -rf ./build",
  "cwd": "/project",
  "workspace_roots": ["/project"]
}
```

The daemon supplies receipt time, effective deadline, project identity, analysis, state, and decision metadata. Preserve optional source identifiers without pretending every adapter provides them. Namespace session identity by agent and conversation, with project association stored separately; a conversation can involve multiple roots.

Suggested analysis fields: `severity`, `analysis_complete`, `reasons`, `candidate_paths`, `unknown_effects`, and `external_effects`. Each path should include its role and whether it is literal or inferred. A display severity of “unknown” can coexist with a known high-risk finding.

Use newline-delimited JSON with a versioned envelope, explicit message types, and maximum frame size. Define commands for submit, decide, list current state, subscribe, and health. Reject invalid messages; keep stdout exclusively for the native hook response.

Authorization states:

```text
received → analyzing → pending → allowed
                              → denied
                              → expired
                              → cancelled
                              → interrupted
```

Track execution separately: `unobserved`, `started` when supported, `completed`, or `failed`. Keep outcome records unlinked when native events lack a reliable identifier. Matching identical command strings is insufficient when sessions execute the same command twice.

An approval is permission for this proposal. It is neither a success receipt nor a guarantee of exactly-once execution.

## Concurrency and failure design

Use a coordinator with a mutex-protected request map and a one-result channel per waiting hook. Start with straightforward synchronization. Do not hold a lock or database transaction while waiting for the user.

For a decision, verify the request ID, request version, live waiter, and deadline. Make the transition from pending conditional and persist it before replying. Serialize competing transitions so a timeout, cancellation, and two UI decisions cannot all win. Return an acknowledgment to the UI only after the durable transition succeeds.

The deadline is checked when deciding, even if a timer callback has been delayed. A duplicate decision should return the existing result or a stale-state error. Reusing a request ID with a different command or working directory is rejected. The same command proposed again under a fresh ID requires a fresh decision.

Keep the immutable proposal in memory for the lifetime of the request. Approve the selected request's stable ID, never “whatever is currently at row 1.” Render control characters safely so command text cannot alter terminal UI behavior.

| Failure | Expected behavior |
| --- | --- |
| Daemon unavailable | Hook returns native denial if possible; installed fail-closed configuration covers hook failure |
| UI absent or closes | Pending requests remain queued until the deadline, then deny |
| Hook connection disappears | Cancel pending authorization when detected; do not replay an old allow to a new invocation |
| Daemon restarts | Mark old pending requests interrupted; hooks must reconnect with fresh invocations where appropriate |
| Database cannot commit | Do not return an allow |
| Decision response is lost | Record delivery uncertainty; observed execution remains unknown unless a later event proves it |
| UI subscriber falls behind | Disconnect/resynchronize from current state rather than block the coordinator |
| Queue fills | Reject new approval requests with an explicit denial reason |

Use a private per-user socket directory and restrictive permissions for the socket and database. Add a daemon lock and conservative stale-socket cleanup. These are practical local reliability boundaries, not protection against a malicious process running as the same user.

For subscriptions, send a consistent initial snapshot plus subsequent sequenced updates. On reconnect or an event gap, fetch current state again. Avoid a message broker or elaborate event-sourcing system.

## Persistence and privacy

Use `sessions`, `actions`, and `action_events` tables, with migrations checked into the repository. Store the native IDs, sanitized display command, working directory, timestamps, analysis version, decision source/reason, and outcome linkage when available. Add a uniqueness constraint for request IDs.

Start with one serialized writer. Consider WAL for UI reads alongside writes; it improves reader/writer coexistence but still permits only one writer at a time. [SQLite WAL documentation](https://www.sqlite.org/wal.html)

Keep exact command text in memory for pending approvals. Persist a redacted display form by default and avoid collecting terminal output or transcripts. Document that redaction is best effort; commands themselves can contain secrets. Use sanitized fixtures in the repository. Add retention before the release, rather than collecting unlimited history and fixing it later.

## Risk analyzer

Make analysis deterministic and explanatory. Manual approval is the default. Unknown effects require review; they do not make the whole product unusable, and a user may explicitly approve them.

Start with these families:

| Example | Finding |
| --- | --- |
| `rm -rf ./build` | Recursive deletion; literal candidate target |
| `git reset --hard` | Potential loss of uncommitted tracked changes |
| `git clean -fd` | Potential deletion of untracked content |
| `git push --force` | External history mutation; local backup cannot reverse it |
| `printf x > file.txt` | Redirect can truncate a file |
| `chmod -R 777 .` | Broad permission change |
| `rm -rf "$TARGET"` | Destructive operation with unresolved target |
| `npm test` or `./script.sh` | Project-defined code; effects not established by the command name |
| `curl URL \| sh` | Remote content execution and unknown effects |

Walk complete syntax trees, including chained commands, substitutions, and redirects. Do not split on spaces or whitelist a first token. A command named `git` or `ls` is not proof of a harmless binary or configuration.

Bound recursion and input size. Never run substitutions or source scripts to “discover” effects. Treat wrappers, variables, glob expansion, shell functions, `cd` chains, symlinks, and executable lookup as sources of uncertainty. Lexically resolving a path is useful for display but does not prove filesystem containment.

Avoid V1 database-statement classification: understanding shell syntax does not establish the behavior of SQL passed through a client.

## Milestones and effort

These are planning estimates for someone learning parts of the stack, not guaranteed delivery dates. Budget roughly 80–145 focused hours for the proposed V1. At 8–12 hours a week, allow approximately 8–18 weeks, including ordinary rework. A basic queue demo should arrive much earlier.

| Milestone | Estimate | Deliverable and exit test |
| --- | --- | --- |
| 0. Integration spike | 6–10 h | Documented real allow/deny/wait/failure behavior; usable decision window |
| 1. Vertical slice | 10–16 h | Hook → daemon → simple CLI decision → hook; two simultaneous requests routed correctly |
| 2. Reliable coordinator | 14–24 h | SQLite, deadlines, cancellations, idempotency, restart handling; racing decisions yield one terminal state |
| 3. TUI | 14–24 h | Queue, details, deadline, keyboard decisions, reconnect; selection remains stable as requests arrive |
| 4. Analysis | 16–28 h | Tested warning families, candidate paths, explicit uncertainty; unsupported commands still enter manual review |
| 5. Release | 20–40 h | Real compatibility smoke tests, fixture demo, installation/doctor commands, docs, CI, tagged binary |

Complete each milestone through the full flow before expanding the next. Introduce only enough persistence to support coordinator semantics; advanced search and timeline polish can wait.

First three work sessions:

1. Initialize the Go module and implement the hook spike in a disposable project.
2. Run the integration matrix, save sanitized fixtures, and write the compatibility result.
3. If it passes, implement submit/list/decide over a Unix socket and prove two waiting requests with a simple CLI before starting Bubble Tea.

## Verification that matters

- Table-driven tests for command warnings, including quoting, redirects, substitutions, wrappers, malformed syntax, and mixed safe/destructive chains.
- Coordinator tests with an injectable clock: approve versus expiry, deny versus cancellation, repeated IDs, conflicting proposals, and concurrent UI decisions.
- Socket integration tests: malformed/oversized messages, partial reads, dropped clients, and slow subscribers.
- Storage tests: commit failure, migration, restart with pending actions, and durable audit records.
- Process-level hook tests using sanitized native payloads; assert stdout contains only a valid agent response.
- `go test -race ./...`, normal unit/integration tests, and `go vet ./...` in CI.
- A disposable real-agent smoke test for every claimed supported version/configuration. Fixtures alone cannot prove native execution gating.

Suggested stress experiment: 100 simulated pending requests with a fixed random seed, concurrent decisions and disconnects, and zero misrouted replies. Report measurements and environment after running it; do not put invented performance numbers in the README.

## Recovery after V1

Treat recovery as a separate subsystem and release gate. First add an explicit checkpoint/export feature that lets the user inspect or retrieve previous files. Defer automatic restoration until attribution is addressed.

For an experimental restoration feature:

1. Restrict support to documented regular files inside one disposable workspace; define exclusions, size limits, permissions, symlink handling, and sensitive-file policy.
2. Keep checkpoint storage outside the live project and separate from its Git history.
3. Define what is covered: shadow Git does not capture every filesystem property, ignored file, external mutation, or repository metadata operation.
4. Require exclusive workspace access for the supported execution/checkpoint window. An Airlock mutex only coordinates Airlock requests; it cannot stop edits by the IDE, user, or bypassing processes.
5. Save before and after manifests. Offer restoration only when current files match the expected post-operation state; otherwise export recovered content for review.
6. Back up current files before a confirmed restore and handle created/deleted files explicitly.

Matching a hash detects some conflicts; it does not prove which actor made a change or eliminate races during restore. Describe the feature as recovery of supported checkpointed files. Never promise command-level undo across arbitrary concurrent operations.

Sandbox dry-runs are also a separate project: environment differences and external side effects make their results conditional. They should not be required to finish the portfolio release.

## Portfolio presentation

For backend/platform applications, Airlock gives you a coherent story about Go concurrency, IPC, persistence, parsing, and fault handling. For frontend/full-stack applications, this project provides less evidence about browser UI and web systems; pair it with an existing web project if those are your target roles.

Make the repository easy to evaluate:

- A short demo: two sessions, two simultaneous proposals, one denial, one allowance, a timeout, and visible history.
- A no-account fixture demo, clearly labeled as simulated, alongside evidence from a real supported agent.
- A concise architecture diagram and design notes on deadlines, durable decisions, and unknown outcomes.
- A compatibility matrix, practical installation instructions, and a clear limitations section.
- A small benchmark or failure experiment with measured results.
- A tagged release that another developer can install and use.

Native permission prompts and hooks already address parts of this space. Your portfolio case does not depend on claiming novelty. The useful contribution is one understandable workflow across several sessions, with explanations and dependable failure handling.

Write a resume bullet only after shipping and substitute actual measurements:

> Built a Go terminal approval dashboard for concurrent coding-agent sessions using Unix sockets and SQLite; implemented deadline-aware request coordination and explainable shell analysis, validated with integration tests and the Go race detector.

Release V1 when the core queue is useful and reliable. A second real adapter is a strong next milestone because it tests the abstraction. Recovery should follow only if it remains interesting and you have time to validate its narrower guarantees.
