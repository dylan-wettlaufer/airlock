// Package cursor translates Cursor's beforeShellExecution hook payloads.
package cursor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"airlock/internal/protocol"
)

const MaxInputBytes = 1 << 20

// Input deliberately omits emails, transcripts, and model metadata.
type Input struct {
	ConversationID string   `json:"conversation_id"`
	HookEventName  string   `json:"hook_event_name"`
	CursorVersion  string   `json:"cursor_version"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Command        string   `json:"command"`
	CWD            string   `json:"cwd"` // Empty means Cursor did not supply the command's working directory.
	Sandbox        bool     `json:"sandbox"`
}

type Response struct {
	Permission   string `json:"permission"`
	UserMessage  string `json:"user_message,omitempty"`
	AgentMessage string `json:"agent_message,omitempty"`
}

// ReadInput bounds payload size and accepts additional native fields for compatibility.
func ReadInput(r io.Reader) (Input, error) {
	var input Input
	data, err := io.ReadAll(io.LimitReader(r, MaxInputBytes+1))
	if err != nil {
		return input, fmt.Errorf("read payload: %w", err)
	}
	if len(data) > MaxInputBytes {
		return input, errors.New("payload exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return input, fmt.Errorf("decode payload: %w", err)
	}
	if input.HookEventName != "beforeShellExecution" {
		return input, errors.New("expected beforeShellExecution event")
	}
	if strings.TrimSpace(input.Command) == "" {
		return input, errors.New("command is required")
	}
	if input.ConversationID == "" {
		return input, errors.New("conversation_id is required")
	}
	if input.CWD != "" && !filepath.IsAbs(input.CWD) {
		return input, errors.New("cwd must be absolute when supplied")
	}
	for _, root := range input.WorkspaceRoots {
		if !filepath.IsAbs(root) {
			return input, errors.New("workspace roots must be absolute")
		}
	}
	return input, nil
}

func (in Input) Proposal() (protocol.Request, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return protocol.Request{}, err
	}
	return protocol.Request{
		ProtocolVersion: protocol.Version, RequestID: hex.EncodeToString(id[:]),
		Agent: "cursor", AgentVersion: in.CursorVersion, ConversationID: in.ConversationID,
		Event: "before_shell_execution", Command: in.Command, CWD: in.CWD,
		WorkspaceRoots: append([]string(nil), in.WorkspaceRoots...),
	}, nil
}
