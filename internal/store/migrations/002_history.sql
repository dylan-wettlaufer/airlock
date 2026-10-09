-- This registry is deliberately independent of retained action details.
-- IDs are never pruned, so deleting history cannot enable authorization replay.
CREATE TABLE used_request_ids (
    request_id TEXT PRIMARY KEY
) WITHOUT ROWID;
INSERT INTO used_request_ids(request_id) SELECT request_id FROM actions;
CREATE INDEX actions_finished ON actions(finished_at DESC, request_id DESC) WHERE state != 'pending';
CREATE INDEX actions_session ON actions(session_id);
