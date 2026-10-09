# Reproducible reliability experiment

Run from the repository root:

```sh
make stress
# Equivalent, without test caching:
./scripts/go test -v ./internal/reliability -run '^TestStress$' -count=1
# Race instrumentation (latency is not comparable with the ordinary run):
./scripts/go test -race -v ./internal/reliability -run '^TestStress$' -count=1
```

Local Unix socket binding must be permitted. The harness uses a fresh private
`/tmp/al-stress-*` directory, isolated socket and SQLite database, and removes
its artifacts and child processes on completion. It never uses the user's daemon,
agent account, hook settings, or history. All proposals are sanitized fixture
strings; **no proposal command is executed**. The only spawned executable is the
Go test binary running its daemon helper, using the production socket, coordinator,
lock, recovery, and store implementations. CLI wiring and hourly maintenance are
covered separately by `make check`.

## Workload and assertions

Seed **42** assigns 100 requests to 30 allow, 30 deny, 20 conflicting allow/deny
pairs, 10 disconnects, and 10 expiry cases. Requests connect/write concurrently;
an explicit barrier requires all 100 to be pending before decisions/disconnects.
Each request has an eight-second effective deadline. OS scheduling determines the
winner of a conflict, so the workload is reproducible while the allowed/denied
split and measured timings can vary.

The harness checks result IDs and permissions, agreement between successful
manual acknowledgments, submitter results and retained audit records, exactly
one pending and one terminal event per record, manual-decision metadata,
redacted command display, approval timestamps strictly before deadlines, and an
empty queue after completion. Disconnected submitters have no received-result
latency; their cancelled audit state and queue removal are checked. Each conflict
must yield exactly one success and one `not_pending`; every expired request must
reject a subsequent allowance.

A separate 101st request is left pending before an actual child-process SIGKILL.
The waiting socket must fail without an authorization result. Restart uses the
same socket/database and must recover that request to interrupted while preserving
all 100 completed audit records. Another restart applies a count limit of 20;
retained details must match the original audit. All 101 IDs must reject changed
proposals, including IDs whose audit details were pruned. With the child stopped,
explicit maintenance at current time plus 31 days exercises age pruning without
waiting a month. After another restart, detailed history must be empty and all
101 IDs must still reject reuse. This is an injected maintenance time, not a
measurement of hourly scheduling. Retention unit/process tests also cover pending
protection, age boundaries, migrations, dependent cleanup and rollback failures.

## Measured run — October 9, 2026

Environment: macOS **26.6**, build **25G72**; Darwin **25.6.0**; **arm64**;
**Go 1.27.2** (`darwin/arm64`). Ordinary `make stress`, without race instrumentation.
Retention reviewed at commit `b25564a`; harness and documentation accompany this
report. The test reported **8.14 seconds**, package invocation **8.522 seconds**.

| Measurement | Observed |
| --- | ---: |
| Concurrent admitted requests | 100 |
| Allowed | 39 |
| Denied | 41 |
| Cancelled by disconnect | 10 |
| Expired, permission deny | 10 |
| Additional pending request at crash | 1, recovered interrupted |
| Successful manual decisions | 80 |
| Expected conflicting-decision rejections | 20 |
| Expected late-allowance rejections | 10 |
| Expected duplicate-ID rejections across two pruning/restart phases | 202 |
| Unexpected errors / assertion failures | 0 |
| Retained records before crash / after recovery / count prune / age prune | 100 / 101 / 20 / 0 |

Latency is measured in the parent from immediately before connection to reading
the terminal submit result. It includes admission, the 100-request barrier,
decision scheduling, SQLite commits and transport. It is not isolated database
latency or a throughput benchmark. Percentiles use the sorted sample, p50 lower
median and p95 nearest rank. Disconnected and crashed requests are excluded.

| Latency, milliseconds | Min | Mean | p50 | p95 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| 80 manually decided requests | 50.604 | 68.516 | 69.120 | 80.924 | 83.212 |
| 90 connected results, including 10 intentional eight-second expiries | 50.604 | 952.779 | 70.266 | 8027.445 | 8044.091 |

The printed JSON includes unrounded measurements and environment on every rerun.
A prior successful ordinary run produced 38 allowed / 42 denied; both runs met the
same assertions. Two initial sandboxed attempts could not bind a socket and
produced no performance result. These numbers come from the subsequent successful
run with local socket permissions.

This validates Airlock's fixture behavior under the specified workload. Native
Cursor cancellation, timeout margins and crash behavior require the separate
[human-observed checklist](compatibility.md#remaining-human-observed-checklist).
