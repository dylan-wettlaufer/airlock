// Package analysis produces bounded, deterministic syntax warnings. It never
// executes commands, resolves variables, reads scripts, or inspects the filesystem.
package analysis

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const MaxCommandBytes = 64 << 10
const maxDepth = 64
const maxNodes = 8192

const syntaxUnknown = "Unsupported or invalid Bash syntax; command effects are unknown."
const dynamicUnknown = "Variables, substitutions, or expansions cannot be resolved statically."
const scriptUnknown = "Script, function, or unrecognized executable effects are unknown."
const contextUnknown = "Working-directory changes or wrappers make target resolution uncertain."
const limitUnknown = "Analysis limit reached; additional effects may be omitted."

type analyzer struct {
	report          Report
	command         string
	cwd             string
	contextChanged  bool
	absoluteTargets []bool
}

// Analyze uses the Bash syntax model explicitly. The source hook does not supply
// the actual shell dialect; even complete rule coverage is not a safety verdict.
func Analyze(command, cwd string) Report {
	a := analyzer{command: command, cwd: cwd, report: Report{Version: Version, Dialect: "bash", Complete: true, Severity: Low,
		Findings: []Finding{}, CandidatePaths: []CandidatePath{}, UnknownEffects: []string{}}}
	if len(command) > MaxCommandBytes || excessiveNesting(command) {
		a.unknown(limitUnknown)
		return a.finish()
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		// Never analyze only a successful prefix of an invalid program.
		a.unknown(syntaxUnknown)
		return a.finish()
	}
	depth, nodes := 0, 0
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			depth--
			return true
		}
		nodes++
		if depth >= maxDepth || nodes > maxNodes {
			a.unknown(limitUnknown)
			return false
		}
		depth++
		switch n := node.(type) {
		case *syntax.CallExpr:
			a.call(n)
		case *syntax.Redirect:
			a.redirect(n)
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				if hasCommand(n.X, "curl", "wget") && hasCommand(n.Y, "sh", "bash", "zsh", "dash", "ksh", "fish") {
					a.find("remote_execution", High, "Downloaded content is piped to a shell for execution.", n.Pos())
					a.report.ExternalEffects = true
					a.unknown("Remote code and its effects cannot be inspected statically.")
				}
			}
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob:
			a.unknown(dynamicUnknown)
		case *syntax.FuncDecl:
			a.unknown("Shell function bodies are inspected, but invocation and executable lookup are uncertain.")
		}
		return true
	})
	return a.finish()
}

// This conservative pre-parse bound counts delimiters even inside quotes. It
// avoids handing deeply nested input to the parser, at the cost of incompleteness
// for unusually delimiter-heavy literal strings.
func excessiveNesting(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(', '{':
			depth++
			if depth > maxDepth {
				return true
			}
		case ')', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return false
}

func (a *analyzer) unknown(reason string) {
	a.report.Complete = false
	for _, old := range a.report.UnknownEffects {
		if old == reason {
			return
		}
	}
	if len(a.report.UnknownEffects) < MaxFindings {
		a.report.UnknownEffects = append(a.report.UnknownEffects, reason)
	}
}
func (a *analyzer) find(code string, severity Severity, reason string, pos syntax.Pos) {
	if len(a.report.Findings) >= MaxFindings {
		a.unknown(limitUnknown)
		// Preserve a later, more severe finding even when the display budget is
		// exhausted by earlier warnings.
		for i := len(a.report.Findings) - 1; i >= 0; i-- {
			if rank(severity) > rank(a.report.Findings[i].Severity) {
				a.report.Findings[i] = Finding{code, severity, reason, pos.Line(), pos.Col()}
				break
			}
		}
		return
	}
	a.report.Findings = append(a.report.Findings, Finding{code, severity, reason, pos.Line(), pos.Col()})
}
func (a *analyzer) finish() Report {
	if a.contextChanged {
		for i := range a.report.CandidatePaths {
			if !a.absoluteTargets[i] {
				a.report.CandidatePaths[i].Resolved = ""
			}
		}
	}
	for _, f := range a.report.Findings {
		if rank(f.Severity) > rank(a.report.Severity) {
			a.report.Severity = f.Severity
		}
	}
	if len(a.report.Findings) == 0 && !a.report.Complete {
		a.report.Severity = Unknown
	}
	return a.report
}
func (a *analyzer) path(w *syntax.Word, role string) {
	if len(a.report.CandidatePaths) >= MaxPaths {
		a.unknown(limitUnknown)
		return
	}
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	expr := a.command[start:end]
	value, literal := literalWord(w)
	resolved := ""
	if literal && value != "" {
		if filepath.IsAbs(value) {
			resolved = filepath.Clean(value)
		} else if filepath.IsAbs(a.cwd) {
			resolved = filepath.Join(a.cwd, value)
		}
	} else if !literal {
		a.unknown(dynamicUnknown)
	}
	if len(expr) > MaxFieldBytes || len(resolved) > MaxFieldBytes {
		a.unknown(limitUnknown)
		return
	}
	a.report.CandidatePaths = append(a.report.CandidatePaths, CandidatePath{expr, role, literal, resolved})
	a.absoluteTargets = append(a.absoluteTargets, literal && filepath.IsAbs(value))
}

// literalWord performs only quote removal. Expansion and executable lookup are
// deliberately excluded, including glob, tilde, brace and ANSI-C quoting.
func literalWord(w *syntax.Word) (string, bool) { return literalParts(w.Parts, false) }
func literalParts(parts []syntax.WordPart, quoted bool) (string, bool) {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			for i := 0; i < len(p.Value); i++ {
				c := p.Value[i]
				if c == '\\' && i+1 < len(p.Value) {
					next := p.Value[i+1]
					if !quoted || strings.ContainsRune("$`\"\\\n", rune(next)) {
						i++
						if next != '\n' {
							b.WriteByte(next)
						}
						continue
					}
				}
				if !quoted && strings.ContainsRune("*?[{}~", rune(c)) {
					return "", false
				}
				b.WriteByte(c)
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			v, ok := literalParts(p.Parts, true)
			if !ok {
				return "", false
			}
			b.WriteString(v)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// Unwrap common execution wrappers without executing them. Unknown wrapper
// options remain uncertain; env -S string interpretation is intentionally opaque.
func commandWords(words []*syntax.Word) (string, []*syntax.Word, bool) {
	wrapped := false
	for depth := 0; depth < 8 && len(words) > 0; depth++ {
		name, ok := literalWord(words[0])
		if !ok {
			return "", words[1:], wrapped
		}
		base := filepath.Base(name)
		switch base {
		case "sudo", "env", "command", "builtin", "exec", "nohup", "nice", "timeout":
			wrapped = true
			words = words[1:]
			for len(words) > 0 {
				v, ok := literalWord(words[0])
				if !ok {
					return "", nil, true
				}
				if v == "--" {
					words = words[1:]
					break
				}
				if base == "env" && strings.Contains(v, "=") && !strings.HasPrefix(v, "-") {
					words = words[1:]
					continue
				}
				if !strings.HasPrefix(v, "-") || v == "-" {
					break
				}
				if base == "env" && (strings.HasPrefix(v, "-S") || strings.HasPrefix(v, "--split-string")) {
					return "", nil, true
				}
				words = words[1:]
				if (base == "sudo" && (v == "-u" || v == "-g" || v == "-h" || v == "-p" || v == "-C" || v == "-T")) ||
					(base == "env" && (v == "-u" || v == "--unset" || v == "-C" || v == "--chdir")) ||
					(base == "nice" && v == "-n") || (base == "exec" && v == "-a") ||
					(base == "timeout" && (v == "-s" || v == "-k" || v == "--signal" || v == "--kill-after")) {
					if len(words) == 0 {
						return "", nil, true
					}
					words = words[1:]
				}
			}
			if base == "timeout" && len(words) > 0 {
				words = words[1:]
			}
		default:
			return base, words[1:], wrapped
		}
	}
	return "", nil, wrapped
}

func (a *analyzer) call(c *syntax.CallExpr) {
	if len(c.Args) == 0 {
		return
	}
	name, args, wrapped := commandWords(c.Args)
	if wrapped {
		a.contextChanged = true
		a.unknown(contextUnknown)
	}
	for _, w := range c.Args {
		if _, ok := literalWord(w); !ok {
			a.unknown(dynamicUnknown)
		}
	}
	if first, ok := literalWord(c.Args[0]); ok && strings.Contains(first, "/") {
		a.unknown(scriptUnknown)
	}
	switch name {
	case "rm", "rmdir", "unlink":
		if flag(args, "--help", "--version") {
			return
		}
		severity, reason := Medium, "Deletion may remove files or directories."
		if shortFlag(args, 'r') || shortFlag(args, 'R') || flag(args, "--recursive") {
			severity, reason = High, "Recursive deletion may remove an entire directory tree."
		}
		for _, w := range operands(args) {
			if _, ok := literalWord(w); !ok {
				severity = High
			}
			a.path(w, "delete")
		}
		a.find("deletion", severity, reason, c.Pos())
	case "git":
		a.git(args, c.Pos())
	case "chmod", "chown", "chgrp":
		if flag(args, "--help", "--version") {
			return
		}
		severity := Medium
		if shortFlag(args, 'R') || flag(args, "--recursive") {
			severity = High
		}
		ops := operands(args)
		if name == "chmod" && len(ops) > 0 {
			mode, _ := literalWord(ops[0])
			if worldWritable(mode) {
				severity = High
			}
		}
		a.find("permissions", severity, "Permission or ownership changes may broaden access; recursive changes affect a tree.", c.Pos())
		if len(ops) > 1 {
			for _, w := range ops[1:] {
				a.path(w, "permissions")
			}
		} else {
			a.unknown("Permission targets could not be established.")
		}
	case "cd", "pushd", "popd":
		a.contextChanged = true
		a.unknown(contextUnknown)
	case "eval", "source", ".", "sh", "bash", "zsh", "dash", "ksh", "fish", "python", "python3", "perl", "ruby", "node", "npm", "npx", "yarn", "pnpm", "make", "just":
		a.unknown(scriptUnknown)
	case "curl", "wget":
		a.report.ExternalEffects = true
		a.unknown("Network requests and downloaded content have effects outside static shell analysis.")
	case "echo", "printf", "pwd", "ls", "cat", "head", "tail", "wc", "true", "false", ":", "test", "[":
		// No configured command warning. Redirects and substitutions are still walked.
	default:
		a.unknown(scriptUnknown)
	}
}

func flag(args []*syntax.Word, values ...string) bool {
	for _, w := range args {
		v, ok := literalWord(w)
		if !ok {
			continue
		}
		for _, value := range values {
			if v == value {
				return true
			}
		}
		if v == "--" {
			break
		}
	}
	return false
}
func shortFlag(args []*syntax.Word, ch byte) bool {
	for _, w := range args {
		v, ok := literalWord(w)
		if v == "--" {
			break
		}
		if ok && strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "--") && strings.ContainsRune(v[1:], rune(ch)) {
			return true
		}
	}
	return false
}
func operands(args []*syntax.Word) []*syntax.Word {
	var out []*syntax.Word
	options := true
	for _, w := range args {
		v, ok := literalWord(w)
		if options && ok && v == "--" {
			options = false
			continue
		}
		if options && ok && strings.HasPrefix(v, "-") && v != "-" {
			continue
		}
		out = append(out, w)
	}
	return out
}
func worldWritable(mode string) bool {
	if len(mode) >= 3 && len(mode) <= 4 && strings.Trim(mode, "01234567") == "" {
		return (mode[len(mode)-1]-'0')&2 != 0
	}
	for _, clause := range strings.Split(mode, ",") {
		i := strings.IndexAny(clause, "+=")
		if i < 0 {
			continue
		}
		who, perms := clause[:i], clause[i+1:]
		if (who == "" || strings.ContainsAny(who, "ao")) && strings.Contains(perms, "w") {
			return true
		}
	}
	return false
}

func (a *analyzer) git(args []*syntax.Word, pos syntax.Pos) {
	for len(args) > 0 {
		v, ok := literalWord(args[0])
		if !ok {
			a.unknown(dynamicUnknown)
			return
		}
		if !strings.HasPrefix(v, "-") {
			break
		}
		args = args[1:]
		if v == "-C" || v == "-c" || v == "--git-dir" || v == "--work-tree" {
			a.contextChanged = true
			a.unknown(contextUnknown)
			if len(args) == 0 {
				return
			}
			args = args[1:]
		} else {
			a.unknown("Git global options may change repository context or behavior.")
			a.contextChanged = true
		}
	}
	if len(args) == 0 {
		return
	}
	sub, _ := literalWord(args[0])
	args = args[1:]
	switch sub {
	case "reset":
		if flag(args, "--hard", "--merge", "--keep") {
			a.find("git_reset", High, "Git reset may discard uncommitted tracked changes.", pos)
		}
	case "clean":
		if !shortFlag(args, 'n') && !flag(args, "--dry-run", "--help") {
			a.find("git_clean", High, "Git clean may delete untracked files or directories.", pos)
			for _, w := range operands(args) {
				a.path(w, "delete")
			}
		}
	case "push":
		a.report.ExternalEffects = true
		if shortFlag(args, 'f') || flag(args, "--force", "--force-with-lease", "--force-if-includes") || hasPrefix(args, "--force-with-lease=") || hasPrefix(args, "+") {
			a.find("git_force_push", High, "Force push may rewrite remote history; a local backup cannot reverse the external effect.", pos)
		}
	case "restore":
		if !shortFlag(args, 'S') && !flag(args, "--staged") || shortFlag(args, 'W') || flag(args, "--worktree") {
			a.find("git_restore", High, "Git restore may discard uncommitted worktree changes.", pos)
			for _, w := range operands(args) {
				a.path(w, "git-worktree")
			}
		}
	case "checkout":
		if shortFlag(args, 'f') || flag(args, "--force", "--") {
			a.find("git_checkout", High, "Git checkout may overwrite uncommitted worktree changes.", pos)
		}
	case "status", "diff", "log", "show", "rev-parse":
	default:
		a.unknown("Git operation effects are outside the configured warning rules.")
	}
	// Git can invoke aliases, hooks, helpers and configured programs. Its name
	// alone never establishes complete effects, even for status/diff.
	a.unknown("Git configuration, aliases, hooks, and helpers are not inspected.")
}
func hasPrefix(args []*syntax.Word, prefix string) bool {
	for _, w := range args {
		if v, ok := literalWord(w); ok && strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}
func (a *analyzer) redirect(r *syntax.Redirect) {
	switch r.Op {
	case syntax.RdrOut, syntax.RdrClob, syntax.RdrAll:
		a.find("truncation", Medium, "Output redirection may create or truncate a file.", r.Pos())
		a.path(r.Word, "write")
	case syntax.AppOut, syntax.AppAll, syntax.RdrInOut:
		a.find("file_write", Medium, "Redirection may create or modify a file.", r.Pos())
		a.path(r.Word, "write")
	case syntax.DplOut:
		v, ok := literalWord(r.Word)
		if !ok || (v != "-" && strings.Trim(v, "0123456789-") != "") {
			a.find("truncation", Medium, "Output descriptor redirection may open and truncate a file.", r.Pos())
			a.path(r.Word, "write")
		}
	}
}

func hasCommand(stmt *syntax.Stmt, names ...string) bool {
	found := false
	depth, nodes := 0, 0
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if node == nil {
			depth--
			return true
		}
		nodes++
		if found || depth >= maxDepth || nodes > maxNodes {
			return false
		}
		depth++
		if c, ok := node.(*syntax.CallExpr); ok {
			name, _, _ := commandWords(c.Args)
			for _, wanted := range names {
				if name == wanted {
					found = true
					break
				}
			}
		}
		return true
	})
	return found
}
