// Package protocol defines source facts shared by adapters and the future daemon.
package protocol

const Version = 1

// Request is an immutable proposal, not evidence of command execution.
type Request struct {
	ProtocolVersion  int      `json:"protocol_version"`
	RequestID        string   `json:"request_id"`
	Agent            string   `json:"agent"`
	AgentVersion     string   `json:"agent_version"`
	ConversationID   string   `json:"conversation_id"`
	SourceToolCallID *string  `json:"source_tool_call_id"`
	Event            string   `json:"event"`
	Command          string   `json:"command"`
	CWD              string   `json:"cwd"` // Empty means unknown; workspace roots are not a substitute.
	WorkspaceRoots   []string `json:"workspace_roots"`
}
