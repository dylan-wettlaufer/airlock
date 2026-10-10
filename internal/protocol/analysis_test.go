package protocol

import (
	"strings"
	"testing"
	"time"

	"airlock/internal/analysis"
)

func TestPendingRejectsMalformedAnalysisAndClonesAllSlices(t *testing.T) {
	r := analysis.Analyze(`rm -rf "$TARGET"`, "/project")
	p := Pending{Request: Request{ProtocolVersion: Version, RequestID: "analysis", Agent: "cursor", ConversationID: "chat", Event: "before_shell_execution", Command: `rm -rf "$TARGET"`}, ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Hour), Analysis: &r}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*analysis.Report){
		func(r *analysis.Report) { r.Version++ },
		func(r *analysis.Report) { r.Severity = "safe" },
		func(r *analysis.Report) { r.Severity = analysis.Low },
		func(r *analysis.Report) { r.Complete = true },
		func(r *analysis.Report) { r.Findings[0].Reason = strings.Repeat("x", analysis.MaxFieldBytes+1) },
		func(r *analysis.Report) { r.CandidatePaths[0].Resolved = "/invented" },
		func(r *analysis.Report) { r.UnknownEffects = make([]string, analysis.MaxFindings+1) },
	} {
		bad := p.Clone()
		mutate(bad.Analysis)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted malformed analysis: %+v", bad.Analysis)
		}
	}
	copy := p.Clone()
	copy.Analysis.Findings[0].Reason = "changed"
	copy.Analysis.CandidatePaths[0].Expression = "changed"
	copy.Analysis.UnknownEffects[0] = "changed"
	if p.Analysis.Findings[0].Reason == "changed" || p.Analysis.CandidatePaths[0].Expression == "changed" || p.Analysis.UnknownEffects[0] == "changed" {
		t.Fatal("clone leaked analysis slices")
	}
	p.Analysis = nil
	if err := p.Validate(); err != nil {
		t.Fatal("older daemon report rejected", err)
	}
}
