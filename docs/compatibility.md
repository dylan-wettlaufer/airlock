# Cursor compatibility gate

## Status

**Partially validated on Cursor 3.23.12 by user report. Immediate deny/allow, non-zero exit, missing executable, malformed output, empty output, and timeout passed. Both conversations printed their markers in the two-chat test. No native prompt appeared for the tested printf command either with Airlock allowance or with the hook disabled. Delay, cancellation, and permission configuration details still need confirmation before the full gate is marked complete.**

Setup environment on October 8, 2026: macOS arm64. The `cursor` launcher exists, but `cursor --version` reported that no Cursor IDE installation was found. This prevents a live IDE smoke test in this environment. Synthetic fixtures and local process tests establish only Airlock's own input/output behavior.

This spike targets only `beforeShellExecution`. The [official Cursor hook reference](https://cursor.com/docs/hooks) documents native `permission` responses, timeout seconds, and `failClosed`. Recheck that reference against your installed version; documentation is not evidence that your configuration gates execution.

### Initial user-reported probe

The user reported Cursor 3.23.12 invoking Airlock in a disposable project and blocking the harmless command with `invalid Cursor hook payload`. A captured input excerpt contained `cwd: ""` and one absolute workspace root. The adapter originally rejected empty working directories; it now preserves an empty or omitted `cwd` as **unknown**, without substituting a workspace root. Future analysis must mark relative path resolution incomplete when `cwd` is unknown.

`testdata/cursor/before-shell-execution-empty-cwd.json` is derived from that user-provided excerpt, with conversation identity and workspace path sanitized. It is not a full native payload. Local regression tests cover both allow and deny with this shape. After the rebuilt binary was supplied, the user reported an explicit block with `Airlock integration spike (simulated decision)` followed by `airlock-test` output from allowance testing. The repeated output was not sufficient to establish per-invocation execution counts. Subsequent user-reported results are recorded in the matrix below.

## Disposable project

1. Run `make build` in Airlock.
2. Create a disposable project outside your working repository.
3. Copy `bin/airlock` into that project's `bin/` directory.
4. Copy `examples/cursor-spike/hooks.json` into that project's `.cursor/hooks.json`.
5. Open the disposable project in the Cursor IDE. Do not install another shell approval hook alongside this one.
6. Record the version, local IDE mode, shell, sandbox/native permission settings, OS, and exact config below.
7. Use only harmless commands, such as `printf 'airlock-test\n'`, or writes inside this disposable project. Record whether the command actually executed; the hook response alone does not prove it.

The supplied config defaults to **deny**, has a 30-second outer timeout, and enables `failClosed`. Change its command for each case, keeping all changes within the disposable project. A future daemon deadline must be shorter than the measured usable outer timeout; 30 seconds is a probe setting, not an established human decision window.

## Matrix

Immediate deny, immediate allow, non-zero exit, missing executable, malformed output, empty output, and outer timeout passed by user report. Delay worked, without specifying whether both delayed allowance and delayed denial were exercised or providing measured timing. Cancellation also worked as reported, without explicit confirmation of the process-list result. Both agents printed in the two-chat test; overlapping hook lifetimes and distinct native conversation IDs were not captured. The native-permission test printed its marker without a Cursor approval prompt. The user then disabled the hook and reported that the baseline printf also ran without an approval prompt. This comparison does not establish that Airlock bypasses native approval. Active permission settings and commands that independently require native approval remain unverified.

| Case | Hook options or change | Evidence to capture |
| --- | --- | --- |
| Immediate deny | `--spike --decision deny` | PASS (user report): explicit spike denial blocked the command |
| Immediate allow | `--spike --decision allow` | PASS (user report): `airlock-test` printed; additional native prompt behavior not recorded |
| Delayed deny | `--spike --decision deny --delay 5s` | Wait time and no execution |
| Delayed allow | `--spike --decision allow --delay 5s` | Wait time and execution |
| Non-zero exit | `--spike --failure nonzero` | PASS (user report): Cursor reported fail-closed blocking with `airlock spike: intentional exit 1` |
| Missing executable | Point command at a nonexistent binary | PASS (user report): marker did not print; exact hook failure message not captured |
| Malformed output | `--spike --failure malformed` | PASS (user report): Cursor reported fail-closed blocking because the hook returned invalid JSON |
| Empty output | `--spike --failure empty` | PASS (user report): command did not print its marker |
| Outer timeout | `--spike --decision allow --delay 45s` | PASS (user report): timeout test worked with no marker printed; exact timing not measured |
| Session cancellation | Cancel during `--delay 20s` | User reported test worked; explicit process-exit confirmation still pending |
| Two simultaneous sessions | Use delayed probes in two conversations | PASS for both proposals completing (user report): both agents printed; overlapping waits and distinct native IDs not captured |
| Native permissions | Repeat sandboxed and approval-requiring proposals | OBSERVED (user report): `printf` ran without a Cursor prompt both with Airlock allowance and with the hook disabled; approval-requiring coverage remains unverified |

The spike path has no socket. Milestone 1 now detects submitter disconnects over the socket; its local process tests establish Airlock behavior only. Native Cursor cancellation and real overlapping daemon-hook waits still require live evidence.

## Manual daemon smoke test

Milestone 1 development proceeds with partial compatibility evidence at the user's request. The daemon defaults to a 25-second effective wait under the example's 30-second outer timeout; this is a conservative development setting, not a measured usable human review window. The following test is pending; automated process tests do not establish live Cursor gating.

1. Build Airlock and copy `bin/airlock` and `examples/cursor-daemon/hooks.json` into the disposable project's `bin/` and `.cursor/hooks.json`, replacing the spike configuration. Record the exact version and permission/sandbox settings. Register only this shell hook.
2. Start `airlock daemon` in a terminal and keep another terminal ready for `airlock list` / `airlock decide`. Verify `airlock doctor` reports a responding daemon. Use the same absolute `--socket` path in the daemon, CLI, and hook config if their temporary-directory environments differ.
3. Ask two separate Cursor conversations to run distinct harmless printf markers. Before deciding, verify that `list` contains two distinct request IDs and native conversation IDs, and that neither marker printed.
4. Decide one full ID with `allow` and the other with `deny` within their displayed deadlines. Confirm only the allowed conversation prints its marker. Record overlapping wait times and the exact native responses/messages. Sanitize any captured payloads.
5. Submit another marker and leave it undecided: verify expiry denies it before the outer timeout. Cancel a waiting conversation and confirm its hook process exits and its queue entry disappears; a later decision must fail.
6. Repeat a waiting marker while stopping the daemon, and with no daemon running. Both must deny. Record process behavior and any lingering socket after a forced crash; do not delete an active daemon's socket.
7. Separately test a harmless command that independently triggers native approval with the hook disabled under the recorded settings, then repeat it with manual Airlock allowance. Record whether a native prompt still appears. The baseline printf runs did not cover approval-requiring commands.

Airlock allowance is permission for a proposal. It does not prove execution, eliminate native prompts, or establish exactly-once execution. Leave the compatibility gate pending until the remaining timing, cancellation, and native-permission evidence is recorded.

## Evidence record

```text
Cursor version:
OS/architecture:
Local IDE mode:
Configured shell:
Native permission settings:
Sandbox settings:
Exact hook configuration:
Test date:
Observed usable wait window:
Cancellation behavior:
Stable native conversation identifier:
Optional tool-call identifier (only if actually observed):
Per-case expected/actual execution and notes:
Gate result: PENDING / PASS / FAIL
```

Save sanitized payloads in `testdata/cursor/`, identifying which are captured versus synthetic. Remove emails, transcript paths, secrets, and personal workspace names. The existing fixture is **synthetic** and does not establish outcome correlation.

Pass only when denial consistently prevents execution, waiting is usable, and all failure behavior is understood for the tested configuration. If the gate fails, investigate another adapter or explicitly choose monitoring-only scope before expanding the daemon.
