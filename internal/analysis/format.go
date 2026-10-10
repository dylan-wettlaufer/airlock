package analysis

import (
	"fmt"
	"strconv"
	"strings"
)

// Label and Details are shared by terminal clients. All source-derived text is
// quoted before display, including paths that contain terminal control bytes.
func Label(r *Report) string {
	if r == nil {
		return "UNKNOWN · analysis unavailable"
	}
	label := strings.ToUpper(string(r.Severity))
	if !r.Complete {
		label += " · incomplete"
	}
	return label
}
func Details(r *Report) string {
	if r == nil {
		return "Analysis: UNKNOWN · unavailable; restart the daemon with the current binary."
	}
	lines := []string{fmt.Sprintf("Analysis: %s (Bash syntax · rules v%d)", Label(r), r.Version)}
	if len(r.Findings) == 0 {
		lines = append(lines, "No configured warning found; this does not establish safety.")
	}
	for _, f := range r.Findings {
		lines = append(lines, fmt.Sprintf("%s: %s (line %d, column %d)", strings.ToUpper(string(f.Severity)), strconv.QuoteToGraphic(f.Reason), f.Line, f.Column))
	}
	for _, p := range r.CandidatePaths {
		kind := "unresolved expression"
		if p.Literal {
			kind = "literal"
		}
		line := fmt.Sprintf("Candidate %s target (%s): %s", p.Role, kind, strconv.QuoteToGraphic(p.Expression))
		if p.Resolved != "" {
			line += " → " + strconv.QuoteToGraphic(p.Resolved) + " (lexical only)"
		}
		lines = append(lines, line)
	}
	for _, u := range r.UnknownEffects {
		lines = append(lines, "Unknown: "+strconv.QuoteToGraphic(u))
	}
	if r.ExternalEffects {
		lines = append(lines, "External effects: network or remote state may be affected.")
	}
	lines = append(lines, "Executable identity, shell dialect, symlinks and configuration are unverified. Manual review required.")
	return strings.Join(lines, "\n")
}
