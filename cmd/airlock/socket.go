package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
)

func socketCommand(ctx context.Context, command string, args []string, in io.Reader, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	socket := flags.String("socket", transport.DefaultSocket(), "private Unix socket path")
	wait := protocol.DefaultWait
	jsonOutput := false
	database := ""
	retention := store.DefaultRetention()
	if command == "daemon" {
		flags.StringVar(&database, "database", "", "persistent history database path")
		flags.DurationVar(&retention.MaxAge, "history-max-age", retention.MaxAge, "maximum age of completed history (default 720h)")
		flags.IntVar(&retention.MaxRecords, "history-max-records", retention.MaxRecords, "maximum completed history records (1..1000000)")
	}
	if command == "daemon" || command == "submit" {
		flags.DurationVar(&wait, "wait", protocol.DefaultWait, "maximum wait (1ms through 24h)")
	}
	if command == "list" {
		flags.BoolVar(&jsonOutput, "json", false, "output a JSON array")
	}
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(diagnostics, "airlock:", err); return 1 }
	if wait < time.Millisecond || wait > protocol.MaxWait {
		return fail(fmt.Errorf("wait must be at least 1ms and at most 24h"))
	}
	if command == "decide" {
		if flags.NArg() != 2 {
			return fail(fmt.Errorf("usage: airlock decide [--socket PATH] <request-id> allow|deny"))
		}
	} else if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected arguments"))
	}
	client := transport.Client{Socket: *socket}
	switch command {
	case "daemon":
		if err := retention.Validate(); err != nil {
			return fail(err)
		}
		if database == "" {
			var err error
			database, err = store.DefaultPath()
			if err != nil {
				return fail(err)
			}
		}
		daemonLock, err := transport.AcquireDaemon(*socket)
		if err != nil {
			return fail(err)
		}
		defer daemonLock.Close()
		history, err := store.OpenWithRetention(database, retention)
		if err != nil {
			return fail(err)
		}
		defer history.Close()
		queue := coordinator.NewWithHistory(history)
		defer queue.Close()
		server, err := daemonLock.Listen(wait, queue)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(diagnostics, "airlock daemon: socket=%s wait=%s database=%s history-max-age=%s history-max-records=%d\n", strconv.Quote(*socket), wait, strconv.Quote(database), retention.MaxAge, retention.MaxRecords)
		if err := serveWithRetention(ctx, server, queue, time.Hour); err != nil {
			return fail(err)
		}
	case "submit":
		data, err := io.ReadAll(io.LimitReader(in, protocol.MaxRequestBytes+1))
		if err != nil || len(data) > protocol.MaxRequestBytes {
			return fail(fmt.Errorf("could not read proposal or proposal exceeds 1 MiB"))
		}
		var request protocol.Request
		if err := json.Unmarshal(data, &request); err != nil {
			return fail(fmt.Errorf("invalid proposal JSON"))
		}
		result, err := client.Submit(ctx, request, wait)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return fail(fmt.Errorf("could not write result"))
		}
	case "list":
		items, err := client.List(ctx)
		if err != nil {
			return fail(err)
		}
		if jsonOutput {
			if err := json.NewEncoder(out).Encode(items); err != nil {
				return fail(fmt.Errorf("could not write queue"))
			}
		} else {
			if len(items) == 0 {
				fmt.Fprintln(out, "No pending requests.")
			}
			for _, p := range items {
				cwd := "unknown"
				if p.Request.CWD != "" {
					cwd = strconv.Quote(p.Request.CWD)
				}
				fmt.Fprintf(out, "%s  agent=%s conversation=%s\n  command=%s\n  cwd=%s deadline=%s\n", p.Request.RequestID, strconv.Quote(p.Request.Agent), strconv.Quote(p.Request.ConversationID), strconv.Quote(p.Request.Command), cwd, p.Deadline.Format(time.RFC3339Nano))
			}
		}
	case "decide":
		result, err := client.Decide(ctx, flags.Arg(0), flags.Arg(1))
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return fail(fmt.Errorf("could not write decision"))
		}
	}
	return 0
}

// Hourly maintenance enforces age limits even when the daemon is idle. Joining
// the worker before closing SQLite keeps shutdown and lock ordering explicit.
func serveWithRetention(ctx context.Context, server *transport.Server, queue *coordinator.Coordinator, interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := queue.PruneHistory(); err != nil {
					if ctx.Err() != nil {
						done <- nil
						return
					}
					done <- err
					cancel()
					return
				}
			}
		}
	}()
	err := server.Serve(ctx)
	cancel()
	maintenanceErr := <-done
	if err != nil {
		return err
	}
	return maintenanceErr
}

func doctor(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	socket := flags.String("socket", transport.DefaultSocket(), "private Unix socket path")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(diagnostics, "airlock: unexpected doctor arguments")
		return 1
	}
	fmt.Fprintf(out, "Airlock %s\nPlatform: %s/%s\nGo build: %s\nStage: manual decisions with persistent SQLite history\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Fprintf(out, "Socket: %s\n", strconv.Quote(*socket))
	if err := (transport.Client{Socket: *socket}).Health(ctx); err != nil {
		fmt.Fprintln(out, "Daemon: unavailable or unsafe")
	} else {
		fmt.Fprintln(out, "Daemon: responding")
	}
	if path, err := store.DefaultPath(); err == nil {
		fmt.Fprintf(out, "Default history database: %s\n", strconv.Quote(path))
	}
	fmt.Fprintln(out, "SQLite history: implemented; TUI: not implemented\nCursor compatibility: partially validated; see docs/compatibility.md")
	return 0
}
