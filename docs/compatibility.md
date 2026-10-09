# Cursor compatibility gate

## Status

**Unverified. No supported Cursor version/configuration is claimed.**

Setup environment on October 8, 2026: macOS arm64. The `cursor` launcher exists, but `cursor --version` reported that no Cursor IDE installation was found. This prevents a live IDE smoke test in this environment. Synthetic fixtures and local process tests establish only Airlock's own input/output behavior.

This spike targets only `beforeShellExecution`. The [official Cursor hook reference](https://cursor.com/docs/hooks) documents native `permission` responses, timeout seconds, and `failClosed`. Recheck that reference against your installed version; documentation is not evidence that your configuration gates execution.

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

All rows are pending **live Cursor validation**:

| Case | Hook options or change | Evidence to capture |
| --- | --- | --- |
| Immediate deny | `--spike --decision deny` | Command did not execute |
| Immediate allow | `--spike --decision allow` | Command executed; note any native prompt |
| Delayed deny | `--spike --decision deny --delay 5s` | Wait time and no execution |
| Delayed allow | `--spike --decision allow --delay 5s` | Wait time and execution |
| Non-zero exit | `--spike --failure nonzero` | Failure blocks with failClosed enabled |
| Missing executable | Point command at a nonexistent binary | Failure blocks |
| Malformed output | `--spike --failure malformed` | Invalid output blocks |
| Empty output | `--spike --failure empty` | Empty output blocks |
| Outer timeout | `--spike --decision allow --delay 45s` | Timeout blocks before delayed allowance |
| Session cancellation | Cancel during `--delay 20s` | Whether process exits; no subsequent execution |
| Two simultaneous sessions | Use delayed probes in two conversations | Independent waits and native conversation IDs |
| Native permissions | Repeat sandboxed and approval-requiring proposals | Any additional native permission prompt |

This pre-daemon spike has no socket. Socket disconnect detection belongs to the vertical slice; record native hook process behavior here.

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
