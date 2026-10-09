# Daemon wire protocol

The private Unix socket accepts one operation per connection. Each frame is one JSON value followed by a newline, at most 2 MiB including that newline. Unknown wire fields, invalid versions, unsupported operations, and invalid proposals are rejected. Request JSON is limited to 1 MiB when encoded. Protocol version is `1`.

Client operations:

```json
{"protocol_version":1,"type":"submit","request":{"protocol_version":1,"request_id":"unique-id","agent":"fixture","agent_version":"synthetic","conversation_id":"chat","source_tool_call_id":null,"event":"before_shell_execution","command":"printf test","cwd":"","workspace_roots":[]},"wait_ms":25000}
{"protocol_version":1,"type":"list"}
{"protocol_version":1,"type":"decide","request_id":"unique-id","permission":"allow"}
{"protocol_version":1,"type":"health"}
```

`wait_ms` is optional for submit; absent or zero uses the daemon's ceiling. A positive value shortens that ceiling. Waits are bounded to 24 hours; the CLI requires at least 1 ms. The effective deadline starts when the coordinator accepts the proposal. Request IDs use 1–128 letters, digits, hyphens, or underscores. Agent, conversation, and command are required; the event is `before_shell_execution`. Working directories and roots must be absolute when supplied. Empty `cwd` stays unknown. Use a fresh request ID per invocation; this protocol has no result replay. IDs are unique across the persistent history, including terminal and interrupted requests.

A submit response arrives only after a terminal transition:

```json
{"protocol_version":1,"type":"submit","result":{"request_id":"unique-id","state":"allowed","permission":"allow","reason":"manual decision"}}
```

Only `allowed` has permission `allow`. `denied`, `expired`, `cancelled`, and `interrupted` have permission `deny`. A decision acknowledgment uses type `decide` with the same result shape. Acknowledgment establishes a committed authorization transition in SQLite, not successful delivery to the hook or observed execution. Detected disconnects remove pending requests; no result is replayed to a new connection. After a transition, the request is removed from the queue and subsequent decisions return `not_pending`. Repeating the same permission and sending the opposite permission both return `not_pending`; neither replays an acknowledgment, changes the winner, or appends another event. This also applies after daemon restart. Unknown IDs and completed lifecycle transitions (expiry, cancellation, shutdown, or recovery) have the same behavior.

Every submitted ID must be fresh. Both an identical resubmission and a changed payload with an existing ID return `duplicate_request`, whether the original is pending or terminal, including after restart. A duplicate never replaces the original proposal or updates its session metadata. No comparison of redacted command displays is used to establish identity.

List takes one consistent pending snapshot and sends a `pending` response for each item, followed by a `list` response with no payload. Each item contains the immutable `request`, `received_at`, and `deadline` (RFC3339 timestamps). This stream keeps individual frames bounded even when the combined queue exceeds 2 MiB. Snapshots are ordered by receipt time, breaking ties by ID; they can become stale as decisions happen. The CLI emits an array only after the complete stream is received. Health returns `{"protocol_version":1,"type":"health"}`.

Errors use a structured envelope:

```json
{"protocol_version":1,"type":"error","error":{"code":"not_pending","message":"request is unknown or no longer pending"}}
```

Other codes include `invalid_frame`, `frame_too_large`, `unsupported_version`, `invalid_message`, `invalid_request`, `duplicate_request`, `queue_full`, `history_unavailable`, and `unavailable`. Clients validate response type, version, ID, and state/permission combinations. Hook diagnostics use locally defined messages rather than echoing remote diagnostic text.

An absent daemon is a local connection error, not a wire result. The Cursor hook alone translates a missing socket or connection refusal before any request is sent into hook-level `allow`, deferring to Cursor's native permission rules. Standalone CLI clients still fail. A wire `unavailable` error from a connected daemon, malformed responses, disconnects after connection, and unsafe socket permissions do not trigger this fallback.

Initial frame reads and each response write have two-second deadlines. Clients bound list/decide/health exchanges to two seconds and submit exchanges to their requested wait plus two seconds. The server caps active connections at 256 and pending proposals at 128. Excess connections are closed; excess proposals receive `queue_full`. No connection is trusted to execute commands.

History is a local read-only database query (`airlock history`), not a socket operation. The daemon commits a request and its admission event before exposing it in `list`. For a normal terminal transition, a conditional update from `pending`, the manual decision (when applicable), and the lifecycle event commit in one transaction before either a decision acknowledgment or a submit result is published. One competing transition wins; every later decision returns `not_pending`.

Any admission or terminal audit failure puts the coordinator into a failed state. The triggering operation returns `history_unavailable`, and all accepted waiters immediately receive an interrupted denial. The pending review queue becomes empty, and later submissions and decisions return `history_unavailable` until restart. This includes failures while saving expiry, cancellation, or shutdown. No further history writes are attempted after failure, so failed storage cannot delay denial of other waiters. Safety denials are the sole exception to commit-before-response: a storage failure cannot reliably persist them, and any records left pending are interrupted during startup recovery. A commit error can also be ambiguous; even if an allow record reached disk, the daemon sends no allowance or successful decision acknowledgment for that failed operation. Pending records left by a crash become interrupted at next daemon startup; permissions are never replayed.
