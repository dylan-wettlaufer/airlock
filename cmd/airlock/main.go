package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"airlock/internal/adapters/cursor"
)

const version = "0.0.0-dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) int {
	if len(args) == 0 {
		usage(out)
		return 0
	}
	switch args[0] {
	case "help", "--help", "-h":
		usage(out)
		return 0
	case "version", "--version":
		fmt.Fprintln(out, version)
		return 0
	case "hook":
		return hook(ctx, args[1:], in, out, diagnostics)
	case "doctor":
		fmt.Fprintf(out, "Airlock %s\nPlatform: %s/%s\nGo build: %s\nStage: Milestone 0 integration spike\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		fmt.Fprintln(out, "Daemon, SQLite, and TUI: not implemented\nCursor compatibility: unverified; follow docs/compatibility.md")
		return 0
	case "demo":
		return demo(ctx, out, diagnostics)
	case "daemon", "tui":
		fmt.Fprintf(diagnostics, "%s is not implemented yet. Complete the Cursor compatibility gate in docs/compatibility.md first.\n", args[0])
		return 1
	default:
		fmt.Fprintf(diagnostics, "unknown subcommand %q\n", args[0])
		usage(diagnostics)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `Airlock — local terminal approval inbox for coding agents

Usage: airlock <subcommand>
  hook --agent cursor          Return a native denial (daemon not built yet)
  hook --agent cursor --spike  Run a controlled integration probe
       --decision allow|deny  Probe response (default deny)
       --delay 5s             Delay the probe response
       --failure nonzero|malformed|empty  Probe native failure handling
  doctor                      Report build and implementation status
  demo                        Run a simulated no-account hook demo
  version                     Print development version
  daemon / tui                Reserved for later milestones

Spike flags are for disposable compatibility tests only.
See docs/compatibility.md before installing any hook configuration.`)
}

func hook(ctx context.Context, args []string, in io.Reader, out, diagnostics io.Writer) int {
	deny := func(reason string) int {
		fmt.Fprintln(diagnostics, "airlock:", reason)
		return writeResponse(out, diagnostics, cursor.Response{Permission: "deny", UserMessage: reason, AgentMessage: reason})
	}
	flags := flag.NewFlagSet("hook", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	agent := flags.String("agent", "cursor", "native adapter")
	spike := flags.Bool("spike", false, "enable disposable integration probe")
	decision := flags.String("decision", "deny", "probe permission")
	delay := flags.Duration("delay", 0, "probe response delay")
	failure := flags.String("failure", "", "probe failure mode")
	if err := flags.Parse(args); err != nil {
		return deny("invalid hook options")
	}
	if flags.NArg() != 0 || *agent != "cursor" {
		return deny("unsupported hook invocation")
	}
	if *decision != "allow" && *decision != "deny" {
		return deny("decision must be allow or deny")
	}
	if *delay < 0 {
		return deny("delay must not be negative")
	}
	if *failure != "" && *failure != "nonzero" && *failure != "malformed" && *failure != "empty" {
		return deny("unknown failure mode")
	}
	input, err := cursor.ReadInput(in)
	if err != nil {
		// Do not echo payload data or parser errors that might contain secrets.
		return deny("invalid Cursor hook payload")
	}
	if !*spike {
		return deny("Airlock daemon is not implemented; authorization denied")
	}
	proposal, err := input.Proposal()
	if err != nil {
		return deny("could not create request ID")
	}
	// Only metadata is logged. Never print the command, email, or transcript.
	fmt.Fprintf(diagnostics, "airlock spike: request=%s delay=%s\n", proposal.RequestID, delay.String())
	timer := time.NewTimer(*delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return deny("hook interrupted")
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return deny("hook interrupted")
	}
	switch *failure {
	case "nonzero":
		fmt.Fprintln(diagnostics, "airlock spike: intentional exit 1")
		return 1
	case "malformed":
		if _, err := io.WriteString(out, "intentional-invalid-json\n"); err != nil {
			return 1
		}
		return 0
	case "empty":
		return 0
	}
	return writeResponse(out, diagnostics, cursor.Response{Permission: *decision, UserMessage: "Airlock integration spike (simulated decision)"})
}

func writeResponse(out, diagnostics io.Writer, response cursor.Response) int {
	if err := json.NewEncoder(out).Encode(response); err != nil {
		fmt.Fprintln(diagnostics, "airlock: could not write native response")
		return 1
	}
	return 0
}

func demo(ctx context.Context, out, diagnostics io.Writer) int {
	fmt.Fprintln(out, "SIMULATED hook demo — no commands execute; no live agent compatibility is established.")
	for _, decision := range []string{"deny", "allow"} {
		payload, err := json.Marshal(cursor.Input{
			ConversationID: "simulated-" + decision, HookEventName: "beforeShellExecution",
			CursorVersion: "fixture", WorkspaceRoots: []string{"/tmp/airlock-disposable"},
			Command: "printf 'airlock-test\\n'", CWD: "/tmp/airlock-disposable",
		})
		if err != nil {
			return 1
		}
		var response bytes.Buffer
		if hook(ctx, []string{"--agent", "cursor", "--spike", "--decision", decision}, bytes.NewReader(payload), &response, diagnostics) != 0 {
			return 1
		}
		var decoded cursor.Response
		if err := json.Unmarshal(response.Bytes(), &decoded); err != nil || decoded.Permission != decision {
			fmt.Fprintln(diagnostics, errors.New("demo response did not match decision"))
			return 1
		}
		fmt.Fprintf(out, "%s: %s", decision, response.String())
	}
	return 0
}
