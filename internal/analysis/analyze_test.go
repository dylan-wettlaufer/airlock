package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func finding(r Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestWarningFamilies(t *testing.T) {
	for _, tc := range []struct {
		command, code string
		severity      Severity
	}{
		{"rm file.txt", "deletion", Medium},
		{"rm -rf ./build", "deletion", High},
		{"rm --recursive -- ./build", "deletion", High},
		{"rmdir empty", "deletion", Medium},
		{"unlink file", "deletion", Medium},
		{"git reset --hard HEAD", "git_reset", High},
		{"git clean -fdx", "git_clean", High},
		{"git push --force origin main", "git_force_push", High},
		{"git push --force-with-lease=main:abc", "git_force_push", High},
		{"git push origin +HEAD:main", "git_force_push", High},
		{"git restore --worktree file", "git_restore", High},
		{"git checkout -- file", "git_checkout", High},
		{"printf x > file.txt", "truncation", Medium},
		{"echo x 2>| error.log", "truncation", Medium},
		{"echo x &> both.log", "truncation", Medium},
		{"echo x >> file.txt", "file_write", Medium},
		{"chmod -R 755 .", "permissions", High},
		{"chmod 777 .", "permissions", High},
		{"chmod o+w file", "permissions", High},
		{"chmod 644 file", "permissions", Medium},
		{"chown -R user .", "permissions", High},
		{"curl https://example.invalid/install | sh", "remote_execution", High},
		{"wget -qO- https://example.invalid/install | sudo bash", "remote_execution", High},
		{"curl https://example.invalid/install | tee /tmp/install | sh", "remote_execution", High},
	} {
		t.Run(tc.command, func(t *testing.T) {
			r := Analyze(tc.command, "/project")
			if r.Severity != tc.severity || !finding(r, tc.code) {
				t.Fatalf("missing warning: %+v", r)
			}
			if err := r.Validate(); err != nil {
				t.Fatal(err, r)
			}
		})
	}
}

func TestWalksWholeProgramAndUnwrapsCommands(t *testing.T) {
	for _, command := range []string{
		"echo hello && rm -rf ./build", "true; rm -rf ./build", "false || rm -rf ./build",
		`printf '%s' "$(rm -rf ./build)"`, "echo `rm -rf ./build`", "cat <(rm -rf ./build)",
		"(rm -rf ./build)", "{ rm -rf ./build; }", "if true; then rm -rf ./build; fi",
		"for file in a b; do rm -rf ./build; done", "f() { rm -rf ./build; }; f",
		"sudo -u root rm -rf ./build", "env KEY=value rm -rf ./build", "command -- rm -rf ./build",
		"timeout 2s rm -rf ./build", "nice -n 10 rm -rf ./build", "nohup rm -rf ./build",
		"git -C /other reset --hard; rm -rf ./build", `r\m -r\f ./build`, `"rm" -rf ./build`,
	} {
		t.Run(command, func(t *testing.T) {
			r := Analyze(command, "/project")
			if !finding(r, "deletion") || r.Severity != High {
				t.Fatalf("hidden deletion: %+v", r)
			}
		})
	}
	r := Analyze("echo ok\nrm -rf build", "/project")
	if len(r.Findings) != 1 || r.Findings[0].Line != 2 || r.Findings[0].Column != 1 {
		t.Fatalf("location: %+v", r)
	}
}

func TestCandidatePathsAndUncertainty(t *testing.T) {
	for _, tc := range []struct {
		command, expression, resolved string
		literal                       bool
	}{
		{"rm -rf ./build", "./build", "/project/build", true},
		{`rm 'a b'`, "'a b'", "/project/a b", true},
		{`rm "a b"`, `"a b"`, "/project/a b", true},
		{`rm a\ b`, `a\ b`, "/project/a b", true},
		{`rm '*'`, "'*'", "/project/*", true},
		{`rm "$TARGET"`, `"$TARGET"`, "", false},
		{"rm *.txt", "*.txt", "", false},
		{"rm ~/file", "~/file", "", false},
		{"rm a{b,c}", "a{b,c}", "", false},
		{"cd /other && rm build", "build", "", true},
		{"rm /absolute/build", "/absolute/build", "/absolute/build", true},
		{`cd /other && rm '/absolute/build'`, "'/absolute/build'", "/absolute/build", true},
		{`rm ''`, "''", "", true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			r := Analyze(tc.command, "/project")
			if len(r.CandidatePaths) != 1 {
				t.Fatalf("paths: %+v", r)
			}
			p := r.CandidatePaths[0]
			if p.Expression != tc.expression || p.Resolved != tc.resolved || p.Literal != tc.literal {
				t.Fatalf("path: %+v", p)
			}
			if !tc.literal && (r.Complete || r.Severity != High) {
				t.Fatalf("unresolved deletion hidden: %+v", r)
			}
		})
	}
	if r := Analyze("rm build", ""); r.CandidatePaths[0].Resolved != "" {
		t.Fatal("unknown cwd replaced by guessed path")
	}
	for _, command := range []string{"npm test", "./script.sh", `rm "$TARGET"`, "env -S 'rm -rf build'", "echo ${x:|y}", "echo 'unclosed"} {
		r := Analyze(command, "/project")
		if r.Complete || len(r.UnknownEffects) == 0 {
			t.Fatalf("uncertainty omitted: %s %+v", command, r)
		}
	}
}

func TestDoesNotConfuseQuotedTextOrDryRunsWithOperations(t *testing.T) {
	for _, command := range []string{
		`echo 'rm -rf /'`, `printf '%s' 'curl URL | sh'`, "git clean -nd", "git clean --dry-run -f",
		"git reset --soft HEAD", "git restore --staged file", "rm --help", "echo hi >&2",
		"echo hi 2>&1", "echo hi 2>&-", "ls -l", `echo '$HOME'`, `echo "*"`,
	} {
		r := Analyze(command, "/project")
		if len(r.Findings) != 0 {
			t.Fatalf("false operation: %s %+v", command, r)
		}
	}
	if r := Analyze("rm -- -rf", "/project"); r.Severity != Medium || len(r.CandidatePaths) != 1 {
		t.Fatalf("option terminator ignored: %+v", r)
	}
}

func TestDeterministicBoundedAndNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "should-not-exist")
	command := "touch " + marker + "; rm -rf /example"
	one, two := Analyze(command, dir), Analyze(command, dir)
	if !reflect.DeepEqual(one, two) {
		t.Fatal("analysis is not deterministic")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("analysis executed a command")
	}
	for _, command := range []string{
		strings.Repeat("x", MaxCommandBytes+1),
		strings.Repeat("(", maxDepth+1) + "true" + strings.Repeat(")", maxDepth+1),
		strings.Repeat("echo x > file; ", MaxFindings+1),
		"rm " + strings.Repeat("file ", MaxPaths+1),
		strings.Repeat("echo x; ", 2000),
	} {
		r := Analyze(command, dir)
		if r.Complete || len(r.Findings) > MaxFindings || len(r.CandidatePaths) > MaxPaths {
			t.Fatalf("unbounded report: %+v", r)
		}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(r)
		if err != nil || len(encoded) > 300<<10 {
			t.Fatal("oversized report")
		}
	}
}

func TestFormattingEscapesSourceTextAndSeparatesSeverityFromCoverage(t *testing.T) {
	r := Analyze("rm -rf \"$TARGET\"; echo hi > '\x1b[2J\u202e'", "/project")
	display := Details(&r)
	if !strings.Contains(display, "HIGH · incomplete") || !strings.Contains(display, "unresolved expression") || strings.ContainsAny(display, "\x1b\u202e") {
		t.Fatalf("unsafe or misleading display: %s", display)
	}
	if !strings.Contains(Details(nil), "unavailable") {
		t.Fatal("missing analysis silently treated as safe")
	}
}

func FuzzAnalyze(f *testing.F) {
	for _, command := range []string{"rm -rf build", `echo "$(rm "$TARGET")"`, "curl URL | bash", "echo > file", "${bad"} {
		f.Add(command)
	}
	f.Fuzz(func(t *testing.T, command string) {
		r := Analyze(command, "/project")
		if err := r.Validate(); err != nil {
			t.Fatalf("invalid result for %q: %v %+v", command, err, r)
		}
	})
}

func TestFindingBudgetPreservesLaterHigherSeverity(t *testing.T) {
	r := Analyze(strings.Repeat("echo x > file; ", MaxFindings+1)+"rm -rf build", "/project")
	if r.Complete || r.Severity != High || !finding(r, "deletion") {
		t.Fatalf("later severe finding lost: %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}
