package transport

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
	"airlock/internal/store"
)

func durableServer(t *testing.T) (*store.Store, string, Client) {
	t.Helper()
	path := socketPath(t)
	database := filepath.Join(filepath.Dir(path), "history.sqlite3")
	history, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	queue := coordinator.NewWithHistory(history)
	server, err := ListenWithCoordinator(path, time.Hour, queue)
	if err != nil {
		history.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("durable server shutdown hung")
		}
		history.Close()
	})
	return history, database, Client{Socket: path}
}

func wireCode(t *testing.T, err error, code string) {
	t.Helper()
	var wire *protocol.WireError
	if !errors.As(err, &wire) || wire.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
}

func TestDurableWireAcknowledgmentAndDuplicates(t *testing.T) {
	history, _, client := durableServer(t)
	waiter := rawSubmit(t, client, request("durable"))
	waitCount(t, client, 1)
	records, err := history.History(context.Background(), store.Query{Limit: 1})
	if err != nil || len(records) != 1 || records[0].State != "pending" || len(records[0].Events) != 1 {
		t.Fatalf("review precedes durable acceptance: %+v %v", records, err)
	}
	conflicting := request("durable")
	conflicting.Command = "printf other"
	_, err = client.Submit(context.Background(), conflicting, time.Second)
	wireCode(t, err, "duplicate_request")
	type outcome struct {
		result protocol.Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, permission := range []string{"allow", "deny"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := client.Decide(context.Background(), "durable", permission)
			outcomes <- outcome{r, err}
		}()
	}
	wg.Wait()
	var winner protocol.Result
	wins := 0
	for range 2 {
		o := <-outcomes
		if o.err != nil {
			wireCode(t, o.err, "not_pending")
		} else {
			wins++
			winner = o.result
		}
	}
	if wins != 1 {
		t.Fatalf("%d acknowledgments succeeded", wins)
	}
	waiter.SetReadDeadline(time.Now().Add(time.Second))
	var response protocol.Response
	if err := protocol.ReadFrame(bufio.NewReader(waiter), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result == nil || *response.Result != winner {
		t.Fatalf("hook result differs from acknowledgment: %+v %+v", response, winner)
	}
	records, err = history.History(context.Background(), store.Query{Limit: 1})
	if err != nil || records[0].State != winner.State || len(records[0].Events) != 2 || records[0].Events[1].State != winner.State || records[0].Decision == nil {
		t.Fatalf("ack without atomic audit: %+v %v", records, err)
	}
	for _, permission := range []string{"allow", "deny"} {
		_, err = client.Decide(context.Background(), "durable", permission)
		wireCode(t, err, "not_pending")
	}
	_, err = client.Submit(context.Background(), conflicting, time.Second)
	wireCode(t, err, "duplicate_request")
	records, err = history.History(context.Background(), store.Query{Limit: 1})
	if err != nil || len(records[0].Events) != 2 {
		t.Fatalf("duplicate appended audit: %+v %v", records, err)
	}
}

func TestDurableWireCommitFailureDeniesWaiters(t *testing.T) {
	for _, mode := range []string{"admission", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			history, path, client := durableServer(t)
			one := rawSubmit(t, client, request("one"))
			two := rawSubmit(t, client, request("two"))
			waitCount(t, client, 2)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			condition := "NEW.state!='pending'"
			if mode == "admission" {
				condition = "NEW.state='pending'"
			}
			_, err = db.Exec(`CREATE TABLE commit_fault(reference TEXT REFERENCES actions(request_id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER fail_commit AFTER INSERT ON action_events WHEN ` + condition + ` BEGIN INSERT INTO commit_fault(reference) VALUES('nonexistent-request'); END;`)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "terminal" {
				_, err = client.Decide(context.Background(), "one", "allow")
			} else {
				_, err = client.Submit(context.Background(), request("failed"), time.Second)
			}
			wireCode(t, err, "history_unavailable")
			if err.Error() != "history persistence failed; authorization denied" {
				t.Fatalf("unsafe or unclear persistence diagnostic: %v", err)
			}
			for _, conn := range []net.Conn{one, two} {
				conn.SetReadDeadline(time.Now().Add(time.Second))
				var response protocol.Response
				if err := protocol.ReadFrame(bufio.NewReader(conn), &response); err != nil {
					t.Fatal(err)
				}
				if response.Type != "submit" || response.Result == nil || response.Result.Permission != "deny" || response.Result.State != "interrupted" {
					t.Fatalf("failed audit allowed or stranded hook: %+v", response)
				}
			}
			waitCount(t, client, 0)
			_, err = client.Decide(context.Background(), "two", "deny")
			wireCode(t, err, "history_unavailable")
			_, err = client.Submit(context.Background(), request("fresh"), time.Second)
			wireCode(t, err, "history_unavailable")
			records, err := history.History(context.Background(), store.Query{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 {
				t.Fatalf("partial admission: %+v", records)
			}
			for _, r := range records {
				if r.State != "pending" || r.Decision != nil || len(r.Events) != 1 {
					t.Fatalf("partial terminal write: %+v", r)
				}
			}
		})
	}
}
