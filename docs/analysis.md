# Command analysis

The daemon analyzes proposals before queue admission with `mvdan.cc/sh/v3/syntax`
v3.14.1, explicitly using the Bash syntax model. It walks the full syntax tree,
including chains, pipelines, substitutions, compound statements and redirects.
No command, substitution, script or interpreter is executed. Analysis does not
read files, resolve executables, inspect configuration or contact the network.

The queue and `airlock list` display the daemon-generated report. Severity is the
highest known finding (`medium` or `high`). `low` means no configured warning was
found with complete static rule coverage; it does not mean safe. `unknown` means
no known finding was established and coverage is incomplete. A known high finding
remains high when its targets or other effects are unknown.

The details include reasons, source line/column, candidate target expressions,
unknown effects and whether a network or remote effect is identified. Literal
paths can be joined lexically to the supplied working directory. That calculation
does not resolve symlinks, inspect files or prove workspace containment. Relative
paths remain unresolved when `cd`, Git context options or execution wrappers make
working-directory interpretation uncertain. An absent working directory is never
replaced with a workspace root.

| Family | Examples | Finding |
| --- | --- | --- |
| Deletion | `rm file`, `rm -rf build`, `rmdir dir`, `unlink file` | Medium deletion; high for recursion or unresolved targets |
| Destructive Git | `git reset --hard`, `git clean -fd`, `git restore file`, `git checkout -- file` | High potential loss of tracked or untracked work |
| Remote Git | `git push --force`, `--force-with-lease`, force refspecs | High remote history mutation |
| Redirection | `> file`, `>| file`, `&> file`, `>> file` | Medium truncation or file modification |
| Permissions | `chmod 777 file`, `chmod -R 755 dir`, `chown -R user dir` | Medium changes; high recursion or world-write permissions |
| Remote execution | `curl URL \| sh`, `wget -qO- URL \| sudo bash` | High downloaded content piped to a shell |

Common wrappers (`sudo`, `env`, `command`, `builtin`, `exec`, `nohup`, `nice`,
`timeout`) are inspected to uncover the wrapped command, while wrapper behavior
remains uncertain. This is a warning catalog, not a complete option interpreter.
Git configuration, hooks and helpers are uninspected and therefore incomplete.
Variables, expansions, shell functions, unknown executables, project scripts,
package runners, `eval`, and shell/interpreter code strings are incomplete. Quoted
command-like text is not treated as an executed command. Known dry-run forms such
as `git clean -n` do not produce a deletion warning.

The source hook currently supplies no shell dialect. The report labels its Bash
syntax model; Zsh-specific, invalid or otherwise unsupported syntax yields an
unknown report, never a successful-prefix safety claim. Even a complete report
cannot establish the identity or actual behavior of a command named `ls` or `git`.
Commands outside this small catalog can modify files or external systems.

Analysis is bounded to 64 KiB of command text, a conservative pre-parse nesting
bound of 64 delimiter levels, 64 traversal levels, 8,192 visited nodes, 64 findings,
128 paths, and 1,024 bytes per displayed path field. Delimiter counting includes
quoted strings and may conservatively reject unusually nested-looking literals.
Exceeding a limit produces incomplete coverage. If the finding budget fills, a
later higher-severity finding replaces a lower-severity entry. Larger proposals
within the protocol's 1 MiB limit still enter manual review with unknown analysis.

Reports are advisory and do not change allow/deny policy. Unknown proposals still
enter review and can be explicitly allowed or denied. Reports and exact candidate
expressions stay in the pending queue only; retained SQLite history keeps its
existing command redaction and does not persist analysis paths or arguments.

Rebuild with `make build`, then restart the daemon and TUI with the same socket.
An already-running daemon retains its old implementation until restarted. The new
TUI labels missing reports from an older daemon as unavailable. Older client
binaries reject the newly added response field; use the rebuilt CLI and TUI.
Restarting interrupts pending requests, so finish current reviews first.

Validation covers warning families, nested and escaped commands, wrappers, literal
and dynamic paths, dry runs, quoted text, limits, deterministic output, terminal
escaping, wire validation, queue-copy isolation, and real-socket TUI decisions.
Run `make check`, `make build`, and `python3 scripts/tui-smoke.py`. The fuzz target
is `./scripts/go test ./internal/analysis -run '^$' -fuzz '^FuzzAnalyze$' -fuzztime=15s`.
