package analysis

import "errors"

const Version = 1
const MaxFindings = 64
const MaxPaths = 128
const MaxFieldBytes = 1024

// Severity describes known findings, independently of incomplete coverage.
type Severity string

const (
	Unknown Severity = "unknown"
	Low     Severity = "low"
	Medium  Severity = "medium"
	High    Severity = "high"
)

type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Reason   string   `json:"reason"`
	Line     uint     `json:"line"`
	Column   uint     `json:"column"`
}

// CandidatePath is a syntax-derived target, never proof of filesystem effects.
// Resolved is lexical only; it does not follow symlinks or establish containment.
type CandidatePath struct {
	Expression string `json:"expression"`
	Role       string `json:"role"`
	Literal    bool   `json:"literal"`
	Resolved   string `json:"resolved,omitempty"`
}

type Report struct {
	Version         int             `json:"version"`
	Dialect         string          `json:"dialect"`
	Severity        Severity        `json:"severity"`
	Complete        bool            `json:"analysis_complete"`
	Findings        []Finding       `json:"findings"`
	CandidatePaths  []CandidatePath `json:"candidate_paths"`
	UnknownEffects  []string        `json:"unknown_effects"`
	ExternalEffects bool            `json:"external_effects"`
}

func rank(s Severity) int {
	switch s {
	case Low:
		return 1
	case Medium:
		return 2
	case High:
		return 3
	}
	return 0
}

func (r Report) Clone() Report {
	r.Findings = append([]Finding(nil), r.Findings...)
	r.CandidatePaths = append([]CandidatePath(nil), r.CandidatePaths...)
	r.UnknownEffects = append([]string(nil), r.UnknownEffects...)
	return r
}

func (r Report) Validate() error {
	invalid := errors.New("invalid command analysis")
	if r.Version != Version || r.Dialect != "bash" || (r.Severity != Unknown && rank(r.Severity) == 0) ||
		len(r.Findings) > MaxFindings || len(r.CandidatePaths) > MaxPaths || len(r.UnknownEffects) > MaxFindings ||
		r.Complete != (len(r.UnknownEffects) == 0) {
		return invalid
	}
	severity := Low
	for _, f := range r.Findings {
		if f.Code == "" || len(f.Code) > 64 || f.Reason == "" || len(f.Reason) > MaxFieldBytes || rank(f.Severity) < 2 || f.Line == 0 || f.Column == 0 {
			return invalid
		}
		if rank(f.Severity) > rank(severity) {
			severity = f.Severity
		}
	}
	if len(r.Findings) == 0 && !r.Complete {
		severity = Unknown
	}
	if r.Severity != severity {
		return invalid
	}
	for _, p := range r.CandidatePaths {
		if p.Expression == "" || len(p.Expression) > MaxFieldBytes || len(p.Resolved) > MaxFieldBytes ||
			(p.Role != "delete" && p.Role != "write" && p.Role != "permissions" && p.Role != "git-worktree") || (!p.Literal && p.Resolved != "") {
			return invalid
		}
	}
	for _, u := range r.UnknownEffects {
		if u == "" || len(u) > MaxFieldBytes {
			return invalid
		}
	}
	return nil
}
