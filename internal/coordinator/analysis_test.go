package coordinator

import (
	"strings"
	"testing"
	"time"

	"airlock/internal/analysis"
)

func TestAnalysisIsDaemonGeneratedAndIsolatedAcrossViews(t *testing.T) {
	c := New()
	defer c.Close()
	peer, err := c.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	r := proposal("analyzed")
	r.Command = `echo "$(rm -rf "$TARGET")" > ./result`
	r.CWD = "/project"
	result, err := c.Submit(r, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	event := nextEvent(t, peer)
	a := event.Pending.Analysis
	if a == nil || a.Severity != analysis.High || a.Complete || len(a.Findings) != 2 || len(a.CandidatePaths) != 2 {
		t.Fatalf("admission analysis: %+v", a)
	}
	a.Findings[0].Reason = "changed"
	a.CandidatePaths[0].Expression = "changed"
	a.UnknownEffects[0] = "changed"
	list := c.List()
	if list[0].Analysis.Findings[0].Reason == "changed" || list[0].Analysis.CandidatePaths[0].Expression == "changed" || list[0].Analysis.UnknownEffects[0] == "changed" {
		t.Fatal("event analysis mutated coordinator")
	}
	list[0].Analysis.Severity = analysis.Low
	snapshot, err := c.Snapshot()
	if err != nil || snapshot.Items[0].Analysis.Severity != analysis.High {
		t.Fatal("list analysis mutated snapshot")
	}
	// High severity and unknown effects are advisory; explicit manual allowance
	// still follows the same coordinator transition and submitter response.
	if _, err := c.Decide(r.RequestID, "allow"); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got.Permission != "allow" {
		t.Fatalf("manual review changed: %+v", got)
	}
}

func TestUnsupportedAndOversizedAnalysisStillEnterManualReview(t *testing.T) {
	for _, command := range []string{"echo 'unclosed", strings.Repeat("x", analysis.MaxCommandBytes+1)} {
		c := New()
		r := proposal("unknown")
		r.Command = command
		result, err := c.Submit(r, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		p := c.List()[0]
		if p.Analysis == nil || p.Analysis.Complete || p.Analysis.Severity != analysis.Unknown {
			t.Fatalf("unknown incorrectly classified: %+v", p.Analysis)
		}
		if _, err := c.Decide(r.RequestID, "deny"); err != nil {
			t.Fatal(err)
		}
		if got := <-result; got.Permission != "deny" {
			t.Fatal(got)
		}
		c.Close()
	}
}
