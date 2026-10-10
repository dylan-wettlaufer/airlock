#!/usr/bin/env python3
"""Isolated PTY smoke test. Run make build first; no proposals execute."""

import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import time


BINARY = str(Path(__file__).resolve().parents[1] / "bin" / "airlock")
DIRECTORY = tempfile.mkdtemp(prefix="al-tui-smoke-", dir="/tmp")
SOCKET = DIRECTORY + "/s.sock"
DATABASE = DIRECTORY + "/history.sqlite3"
connections = []
capture = bytearray()
daemon = ui = master = slave = None


def until(condition, timeout=5):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            if condition():
                return
        except (OSError, ValueError):
            pass
        time.sleep(0.01)
    raise AssertionError("condition timed out")


def wire(kind, **fields):
    connection = socket.socket(socket.AF_UNIX)
    try:
        connection.settimeout(5)
        connection.connect(SOCKET)
        message = dict(protocol_version=1, type=kind, **fields)
        connection.sendall((json.dumps(message) + "\n").encode())
        return connection
    except BaseException:
        connection.close()
        raise


def operation(kind):
    with wire(kind) as connection, connection.makefile("rb") as reader:
        rows = []
        while True:
            row = json.loads(reader.readline())
            rows.append(row)
            if kind != "list" or row["type"] == "list":
                return rows


def start_daemon():
    process = subprocess.Popen(
        [BINARY, "daemon", "--socket", SOCKET, "--database", DATABASE, "--wait", "15s"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        until(lambda: operation("health")[0]["type"] == "health")
        return process
    except BaseException:
        process.terminate()
        process.wait(timeout=5)
        raise


def submit(request_id):
    proposal = dict(
        protocol_version=1,
        request_id=request_id,
        agent="fixture",
        agent_version="pty-smoke",
        conversation_id="sanitized-" + request_id,
        event="before_shell_execution",
        command="fixture-proposal sanitized-argument",
        cwd="",
        workspace_roots=[],
    )
    connection = wire("submit", request=proposal, wait_ms=15000)
    connections.append(connection)
    return connection


def read_until(text, offset=0):
    target = text.encode()
    end = time.monotonic() + 5
    while time.monotonic() < end:
        if target in capture[offset:]:
            return
        if select.select([master], [], [], 0.1)[0]:
            data = os.read(master, 65536)
            if not data:
                break
            capture.extend(data)
    raise AssertionError("screen missing " + text)


def result(connection, request_id, permission):
    with connection.makefile("rb") as reader:
        row = json.loads(reader.readline())
    outcome = row["result"]
    assert outcome["request_id"] == request_id, outcome
    assert outcome["permission"] == permission, outcome


def history_states():
    records = json.loads(
        subprocess.check_output([BINARY, "history", "--database", DATABASE, "--json"])
    )
    return {record["request_id"]: record["state"] for record in records}


try:
    daemon = start_daemon()
    one = submit("smoke-one")
    until(lambda: len(operation("list")) == 2)
    two = submit("smoke-two")
    until(lambda: len(operation("list")) == 3)

    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 32, 120, 0, 0))
    original = termios.tcgetattr(slave)
    ui = subprocess.Popen(
        [BINARY, "tui", "--socket", SOCKET],
        stdin=slave,
        stdout=slave,
        stderr=slave,
        env=dict(os.environ, TERM="xterm-256color"),
    )
    read_until("fixture-proposal")
    os.write(master, b"a")
    read_until("ALLOW smoke-one")
    os.write(master, b"\r")
    result(one, "smoke-one", "allow")
    read_until("allowed")
    os.write(master, b"d")
    read_until("DENY smoke-two")
    os.write(master, b"\r")
    result(two, "smoke-two", "deny")
    read_until("denied")

    pending = submit("crash-pending")
    until(lambda: len(operation("list")) == 2)
    read_until("crash-pending")
    crash_offset = len(capture)
    daemon.kill()
    daemon.wait()
    daemon = None
    assert pending.recv(1) == b""
    read_until("DISCONNECTED", crash_offset)
    restart_offset = len(capture)
    daemon = start_daemon()
    read_until("LIVE", restart_offset)
    expected = {
        "smoke-one": "allowed",
        "smoke-two": "denied",
        "crash-pending": "interrupted",
    }
    assert history_states() == expected

    fresh = submit("fresh-after-restart")
    until(lambda: len(operation("list")) == 2)
    read_until("fresh-after-restart", restart_offset)
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    os.kill(ui.pid, signal.SIGWINCH)
    time.sleep(0.2)
    os.write(master, b"d")
    read_until("DENY fresh-after-restart", restart_offset)
    os.write(master, b"\r")
    result(fresh, "fresh-after-restart", "deny")
    expected["fresh-after-restart"] = "denied"
    assert history_states() == expected
    os.write(master, b"q")
    ui.wait(timeout=5)
    assert ui.returncode == 0
    while select.select([master], [], [], 0.1)[0]:
        capture.extend(os.read(master, 65536))
    assert b"\x1b[?1049l" in capture, "alternate screen not restored"

    # Darwin may set PENDIN when canonical input is restored. Ignore that
    # pending-input bookkeeping bit; all user terminal modes must match.
    restored = termios.tcgetattr(slave)
    restored[3] &= ~getattr(termios, "PENDIN", 0)
    original[3] &= ~getattr(termios, "PENDIN", 0)
    assert restored == original, "terminal modes not restored"
    print(json.dumps(dict(
        result="PASS",
        keyboard_results={
            "smoke-one": "allow",
            "smoke-two": "deny",
            "fresh-after-restart": "deny",
        },
        crash_pending="interrupted",
        reconnected=True,
        resized_to="80x24",
        terminal_restored=True,
        proposal_commands_executed=0,
    )))
finally:
    if ui is not None and ui.poll() is None:
        ui.terminate()
        ui.wait(timeout=5)
    if daemon is not None and daemon.poll() is None:
        daemon.terminate()
        daemon.wait(timeout=5)
    for connection in connections:
        connection.close()
    for descriptor in (master, slave):
        if descriptor is not None:
            os.close(descriptor)
    shutil.rmtree(DIRECTORY)
