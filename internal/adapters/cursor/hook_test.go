package cursor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/cursor/before-shell-execution.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadInputAndProposal(t *testing.T) {
	input, err := ReadInput(strings.NewReader(string(fixture(t))))
	if err != nil {
		t.Fatal(err)
	}
	first, err := input.Proposal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := input.Proposal()
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestID == second.RequestID || len(first.RequestID) != 32 {
		t.Fatal("each invocation needs a fresh request ID")
	}
	if first.Command != input.Command || first.ConversationID != input.ConversationID || first.SourceToolCallID != nil {
		t.Fatal("source facts changed")
	}
	input.WorkspaceRoots[0] = "/changed"
	if first.WorkspaceRoots[0] == "/changed" {
		t.Fatal("proposal aliases native payload")
	}
}

func TestRejectInvalidInput(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "[]", "{", string(fixture(t)) + "{}", strings.Repeat("x", MaxInputBytes+1)} {
		if _, err := ReadInput(strings.NewReader(raw)); err == nil {
			t.Errorf("accepted invalid payload of length %d", len(raw))
		}
	}
	for _, field := range []string{"command", "conversation_id", "cwd", "hook_event_name"} {
		t.Run(field, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(fixture(t), &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, field)
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadInput(strings.NewReader(string(data))); err == nil {
				t.Fatal("accepted missing required field")
			}
		})
	}
}
