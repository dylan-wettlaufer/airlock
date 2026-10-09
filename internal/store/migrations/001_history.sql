CREATE TABLE sessions (
    id INTEGER PRIMARY KEY,
    agent TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    UNIQUE(agent, conversation_id)
);
CREATE TABLE actions (
    request_id TEXT PRIMARY KEY,
    session_id INTEGER NOT NULL REFERENCES sessions(id),
    protocol_version INTEGER NOT NULL,
    agent_version TEXT NOT NULL,
    source_tool_call_id TEXT,
    event TEXT NOT NULL,
    command_display TEXT NOT NULL,
    cwd TEXT NOT NULL,
    workspace_roots TEXT NOT NULL,
    received_at TEXT NOT NULL,
    deadline TEXT NOT NULL,
    finished_at TEXT,
    state TEXT NOT NULL CHECK(state IN ('pending','allowed','denied','expired','cancelled','interrupted')),
    reason TEXT NOT NULL,
    execution_state TEXT NOT NULL DEFAULT 'unobserved' CHECK(execution_state = 'unobserved')
);
CREATE INDEX actions_history ON actions(received_at DESC, request_id DESC);
CREATE TABLE decisions (
    request_id TEXT PRIMARY KEY REFERENCES actions(request_id),
    permission TEXT NOT NULL CHECK(permission IN ('allow','deny')),
    source TEXT NOT NULL,
    reason TEXT NOT NULL,
    decided_at TEXT NOT NULL
);
CREATE TABLE action_events (
    id INTEGER PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES actions(request_id),
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    occurred_at TEXT NOT NULL
);
CREATE INDEX action_events_request ON action_events(request_id, id);
