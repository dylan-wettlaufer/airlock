package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airlock/internal/analysis"
	"airlock/internal/coordinator"
	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
	tea "charm.land/bubbletea/v2"
)

func TestProgramDecisionsRouteToRealSubmittersAndAudit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "al-tui-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := store.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := transport.ListenWithCoordinator(filepath.Join(dir, "s.sock"), 10*time.Second, coordinator.NewWithHistory(db))
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	defer func() {
		stopServer()
		if err := await(t, serverDone); err != nil {
			t.Error(err)
		}
	}()
	client := transport.Client{Socket: filepath.Join(dir, "s.sock")}
	type submission struct {
		result protocol.Result
		err    error
	}
	submit := func(id string) <-chan submission {
		done := make(chan submission, 1)
		go func() {
			request := fixture(id, time.Now()).Request
			request.Command = `echo "$(rm -rf "$TARGET")" > ./result`
			r, err := client.Submit(context.Background(), request, 10*time.Second)
			done <- submission{r, err}
		}()
		return done
	}
	one := submit("one")
	waitQueue := func(n int) {
		end := time.Now().Add(3 * time.Second)
		for time.Now().Before(end) {
			p, err := client.List(context.Background())
			if err == nil && len(p) == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("queue did not reach %d", n)
	}
	waitQueue(1)
	two := submit("two")
	waitQueue(2)
	// List and subscription snapshots must carry the daemon's findings intact.
	items, err := client.List(context.Background())
	if err != nil || len(items) != 2 {
		t.Fatalf("list: %v", err)
	}
	for _, p := range items {
		if p.Analysis == nil || p.Analysis.Severity != analysis.High || p.Analysis.Complete || len(p.Analysis.Findings) != 2 {
			t.Fatalf("wire analysis: %+v", p.Analysis)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFeed(ctx, client)
	go f.run()
	defer func() { cancel(); await(t, f.done) }()
	ready := make(chan struct{})
	acknowledged := make(chan struct{})
	var once, ackOnce sync.Once
	program := tea.NewProgram(newModel(ctx, f), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithContext(ctx), tea.WithoutSignalHandler(), tea.WithoutRenderer(), tea.WithWindowSize(120, 32), tea.WithFilter(func(_ tea.Model, message tea.Msg) tea.Msg {
		if msg, ok := message.(snapshotMsg); ok {
			for _, p := range msg.snapshot.Items {
				if p.Analysis == nil || p.Analysis.Severity != analysis.High || p.Analysis.Complete {
					t.Error("subscription lost analysis")
				}
			}
			once.Do(func() { close(ready) })
		}
		if _, ok := message.(decisionMsg); ok {
			ackOnce.Do(func() { close(acknowledged) })
		}
		if b, ok := message.(programBarrier); ok {
			close(b.done)
		}
		return message
	}))
	programDone := make(chan error, 1)
	go func() { _, err := program.Run(); programDone <- err }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("program did not synchronize")
	}
	program.Send(tea.KeyPressMsg{Code: 'a', Text: "a"})
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	result := await(t, one)
	if result.err != nil || result.result.RequestID != "one" || result.result.Permission != "allow" {
		t.Fatalf("allow routing: %+v", result)
	}
	// Wait until the first acknowledgment is handled before sending another
	// decision. The filter observes delivery; the event loop processes it before
	// accepting the next key sent through Program.Send.
	// A barrier message avoids sleeps and inspecting a running model.
	await(t, acknowledged)
	barrier := make(chan struct{})
	program.Send(programBarrier{barrier})
	await(t, barrier)
	// The terminal event may be queued immediately after the acknowledgment.
	waitQueue(1)
	// Select the remaining request explicitly if its event is still in transit.
	program.Send(tea.KeyPressMsg{Code: 'j', Text: "j"})
	program.Send(tea.KeyPressMsg{Code: 'd', Text: "d"})
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	result = await(t, two)
	if result.err != nil || result.result.RequestID != "two" || result.result.Permission != "deny" {
		t.Fatalf("deny routing: %+v", result)
	}
	program.Send(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if err := await(t, programDone); err != nil {
		t.Fatal(err)
	}
	records, err := db.History(context.Background(), store.Query{Limit: 10})
	if err != nil || len(records) != 2 {
		t.Fatalf("audit: %v", err)
	}
	for _, r := range records {
		expected := "allowed"
		if r.RequestID == "two" {
			expected = "denied"
		}
		if r.State != expected || len(r.Events) != 2 || r.Decision == nil {
			t.Fatalf("UI/audit mismatch: %+v", r)
		}
	}
}

type programBarrier struct{ done chan struct{} }
