# Terminal approval inbox

Build with `make build`, then launch the daemon and UI in separate terminals:

```sh
# Terminal 1
./bin/airlock daemon

# Terminal 2
./bin/airlock tui

# Terminal 3: submit a sanitized fixture; no proposal command executes
./bin/airlock hook --agent cursor < testdata/cursor/before-shell-execution.json
```

The hook fixture creates a fresh invocation ID each time. The UI uses the same
private socket as the other CLI commands; pass `--socket PATH` to every process
when overriding it. It can start before the daemon and will keep reconnecting.
Quitting the UI leaves the daemon and requests from other connections running.

The UI displays its configured socket above the queue. If it reports no daemon
reachable there while CLI requests work, compare that path with daemon startup
and the hook's `--socket` option. For example, a daemon started with
`--socket /tmp/airlock-cursor-test/airlock.sock` requires:

```sh
./bin/airlock tui --socket /tmp/airlock-cursor-test/airlock.sock
```

Refreshing reconnects to the same configured socket; it does not discover daemons
on other paths.

The queue is ordered by receipt time and full request ID. Selection stays attached
to its ID as other requests arrive or disappear. If that request leaves the queue,
the nearest remaining row becomes selected and any confirmation is cleared.
The details pane shows the proposed command first, then native metadata, working
directory, workspace roots and deadline. An empty working directory is labeled
unknown. Wide terminals show panes side by side; narrower ones stack them.

| Key | Action |
| --- | --- |
| Up/Down or k/j | Select a request |
| Home/End or g/G | Select first/last request |
| PageUp/PageDown | Scroll the selected proposal's details |
| a / d | Open an allow / deny confirmation for the selected full ID |
| Enter | Confirm that specific request and permission |
| Esc | Cancel confirmation |
| r | Discard the connection and fetch a fresh snapshot |
| q / Ctrl+C | Close the UI |

Only one decision is sent at a time. Navigation clears confirmation when selection
changes; new arrivals do not silently retarget it. Countdown displays update every
250 milliseconds. At the deadline, confirmation and decision controls are disabled,
even if the daemon's expiry event has not arrived yet. The daemon remains the
final authority for deadlines and winning transitions. A terminal smaller than
52 columns by 16 rows shows a resize prompt with decisions disabled.

On disconnect, stream gaps or buffer overflow, the screen marks its cached queue
as disconnected and disables decisions. It retries with delays from 500 ms up to
5 seconds. A complete new snapshot replaces the cache; selection survives if the
same ID is still pending. A restarted daemon has a new stream epoch and never
restores interrupted approvals. A queued keyboard decision rechecks the connection
lease before sending, and disconnection cancels in-flight client I/O. A late error
from an older connection does not invalidate a newer synchronized view.

Decision acknowledgments report authorization, not execution. If an acknowledgment
is lost, the UI reports the uncertainty and never automatically retries it. Check
retained history for the recorded outcome. An external CLI/UI decision can win
first; a losing decision refreshes the queue. Closing or refreshing this UI's
subscription does not cancel hook submissions on their separate connections.

Command and metadata control characters, including ANSI and bidi controls, are
escaped before rendering. Proposal strings are displayed, never executed. Individual
fields are visibly limited to 64 KiB and workspace displays to 1,024 roots; limits
are labeled. Shell analysis remains unimplemented, so the screen provides no
command risk classification. Read the proposal before granting permission.

## Local validation

`make check` covers state transitions and keyboard routing, selection stability,
countdown/confirmation expiry, repeated keys, stale queued commands, UI handoff
overflow, reconnects/new epochs, uncertain acknowledgments, late old-connection
errors, escaping, scrolling and small-terminal layouts. A headless Bubble Tea
program drives real socket decisions and checks submitter results against SQLite
records. Those tests complement the coordinator/subscription reliability tests.

To exercise the rendered program against an isolated daemon in a pseudo-terminal:

```sh
make build
python3 scripts/tui-smoke.py
```

This uses only Python's standard library and a private temporary socket/database.
It sends actual keyboard bytes, crashes and restarts the daemon on the same endpoint,
resizes the terminal, checks routed results and history, and checks alternate-screen
and user terminal-mode restoration. It removes its processes and temporary files.
No proposal commands execute. On Darwin, the pending-input bookkeeping bit is
ignored when comparing restored termios state; canonical input, echo, signals and
the other user modes must match.

The October 9, 2026 run on macOS 26.6 arm64 / Go 1.27.2 passed: fixture allowances
and denials routed correctly, a crash-pending request recovered as interrupted,
the UI reconnected, a fresh request received a keyboard denial after resize to
80×24, and the terminal was restored on quit.

This establishes Airlock's local behavior. It does not establish Cursor's native
cancellation, timeout margin or crash behavior. Those remain in the separate
[human-observed Cursor checklist](compatibility.md#remaining-human-observed-checklist).
