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
	for _, field := range []string{"command", "conversation_id", "hook_event_name"} {
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

func TestUnknownCWD(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/cursor/before-shell-execution-empty-cwd.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, roots := range [][]string{{"/tmp/airlock-disposable"}, {"/tmp/first", "/tmp/second"}, nil} {
		for _, omit := range []bool{false, true} {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if omit {
				delete(fields, "cwd")
			}
			fields["workspace_roots"], err = json.Marshal(roots)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			input, err := ReadInput(strings.NewReader(string(payload)))
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := input.Proposal()
			if err != nil {
				t.Fatal(err)
			}
			if proposal.CWD != "" {
				t.Fatal("unknown cwd must not be inferred from workspace roots")
			}
			if len(proposal.WorkspaceRoots) != len(roots) {
				t.Fatal("workspace roots changed")
			}
		}
	}
}

func TestRejectRelativeCWD(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t), &fields); err != nil {
		t.Fatal(err)
	}
	fields["cwd"] = json.RawMessage(`"relative/path"`)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInput(strings.NewReader(string(data))); err == nil {
		t.Fatal("accepted relative cwd")
	}
}
