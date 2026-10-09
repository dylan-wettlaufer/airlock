package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"airlock/internal/store"
)

func historyCommand(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("history", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	path := flags.String("database", "", "persistent history database path")
	jsonOutput := flags.Bool("json", false, "output a JSON array")
	q := store.Query{}
	flags.IntVar(&q.Limit, "limit", 50, "maximum records (1..200)")
	flags.IntVar(&q.Offset, "offset", 0, "skip records (0..10000)")
	flags.StringVar(&q.RequestID, "request", "", "exact request ID")
	flags.StringVar(&q.Agent, "agent", "", "exact agent name")
	flags.StringVar(&q.ConversationID, "conversation", "", "exact conversation ID")
	flags.StringVar(&q.State, "state", "", "request state")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(diagnostics, "airlock:", err); return 1 }
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected history arguments"))
	}
	if err := q.Validate(); err != nil {
		return fail(err)
	}
	if *path == "" {
		var err error
		*path, err = store.DefaultPath()
		if err != nil {
			return fail(err)
		}
	}
	db, err := store.Read(*path)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	records, err := db.History(ctx, q)
	if err != nil {
		return fail(fmt.Errorf("could not query history"))
	}
	if *jsonOutput {
		if err := json.NewEncoder(out).Encode(records); err != nil {
			return fail(fmt.Errorf("could not write history"))
		}
		return 0
	}
	if len(records) == 0 {
		fmt.Fprintln(out, "No history records.")
	}
	for _, r := range records {
		fmt.Fprintf(out, "%s  %s  state=%s agent=%s conversation=%s\n  command=%s\n  reason=%s execution=%s\n", r.ReceivedAt, r.RequestID, strconv.Quote(r.State), strconv.Quote(r.Agent), strconv.Quote(r.ConversationID), strconv.Quote(r.CommandDisplay), strconv.Quote(r.Reason), strconv.Quote(r.ExecutionState))
	}
	return 0
}
