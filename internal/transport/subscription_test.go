package transport

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("stream callback did not arrive")
	}
	var zero T
	return zero
}
func observe(t *testing.T, c Client) (<-chan protocol.Snapshot, <-chan protocol.QueueEvent, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	snapshots := make(chan protocol.Snapshot, 1)
	events := make(chan protocol.QueueEvent, 16)
	done := make(chan error, 1)
	go func() {
		done <- c.Subscribe(ctx, func(s protocol.Snapshot) error { snapshots <- s; return nil }, func(e protocol.QueueEvent) error {
			select {
			case events <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		err := receive(t, done)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled subscription: %v", err)
		}
	}
	t.Cleanup(stop)
	return snapshots, events, stop
}

func TestSnapshotSubscribeRoutingAndReconnect(t *testing.T) {
	_, client := startServer(t, time.Hour)
	one := rawSubmit(t, client, request("one"))
	waitCount(t, client, 1)
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || len(snapshot.Items) != 1 || snapshot.Cursor.Sequence != 1 {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	snapshots, events, stop := observe(t, client)
	initial := receive(t, snapshots)
	if initial.Cursor != snapshot.Cursor {
		t.Fatal("snapshot differs without mutations")
	}
	two := rawSubmit(t, client, request("two"))
	waitCount(t, client, 2)
	event := receive(t, events)
	if event.Pending == nil || event.Pending.Request.RequestID != "two" || event.Cursor.Sequence != 2 {
		t.Fatal("incorrect admission")
	}
	r, err := client.Decide(context.Background(), "one", "deny")
	if err != nil {
		t.Fatal(err)
	}
	event = receive(t, events)
	if event.Result == nil || *event.Result != r || event.Cursor.Sequence != 3 {
		t.Fatal("incorrect decision")
	}
	one.Close()
	stop()
	_, err = client.Decide(context.Background(), "two", "deny")
	if err != nil {
		t.Fatal(err)
	}
	two.Close()
	waitCount(t, client, 0)
	snapshots, _, stop = observe(t, client)
	fresh := receive(t, snapshots)
	if len(fresh.Items) != 0 || fresh.Cursor.Epoch != snapshot.Cursor.Epoch || fresh.Cursor.Sequence != 4 {
		t.Fatal("reconnect did not replace old snapshot")
	}
	stop()
}

func TestLargeSubscriptionSnapshotAndIdleCancellation(t *testing.T) {
	_, client := startServer(t, time.Hour)
	for _, id := range []string{"large-one", "large-two", "large-three", "large-four"} {
		r := request(id)
		r.Command = strings.Repeat("x", 700000)
		rawSubmit(t, client, r)
	}
	waitCount(t, client, 4)
	snapshots, _, stop := observe(t, client)
	snapshot := receive(t, snapshots)
	if len(snapshot.Items) != 4 {
		t.Fatal("incomplete large snapshot")
	}
	for _, p := range snapshot.Items {
		if len(p.Request.Command) != 700000 {
			t.Fatal("truncated proposal")
		}
	}
	// An idle stream must survive the ordinary two-second operation timeout.
	time.Sleep(ioTimeout + 50*time.Millisecond)
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
}

func TestStreamRejectsIncompleteSnapshotsAndInvalidEvents(t *testing.T) {
	cursor := protocol.Cursor{Epoch: strings.Repeat("a", 32), Sequence: 1}
	p := protocol.Pending{Request: request("one"), ReceivedAt: time.Now(), Deadline: time.Now().Add(time.Hour)}
	begin := protocol.Response{Type: "snapshot_begin", Cursor: &cursor}
	item := protocol.Response{Type: "pending", Pending: &p}
	end := protocol.Response{Type: "snapshot", Cursor: &cursor}
	terminal := protocol.Result{RequestID: "one", State: "denied", Permission: "deny"}
	event := func(epoch string, seq uint64, result protocol.Result) protocol.Response {
		return protocol.Response{Type: "event", Event: &protocol.QueueEvent{Cursor: protocol.Cursor{Epoch: epoch, Sequence: seq}, Result: &result}}
	}
	for _, tc := range []struct {
		name     string
		frames   []protocol.Response
		snapshot bool
	}{
		{"partial", []protocol.Response{begin, item}, false},
		{"duplicate", []protocol.Response{begin, item, item, end}, false},
		{"wrong-boundary", []protocol.Response{begin, item, {Type: "snapshot", Cursor: &protocol.Cursor{Epoch: cursor.Epoch, Sequence: 2}}}, false},
		{"gap", []protocol.Response{begin, item, end, event(cursor.Epoch, 3, terminal)}, true},
		{"repeated-sequence", []protocol.Response{begin, item, end, event(cursor.Epoch, 1, terminal)}, true},
		{"epoch-change", []protocol.Response{begin, item, end, event(strings.Repeat("b", 32), 2, terminal)}, true},
		{"unknown-terminal", []protocol.Response{begin, item, end, event(cursor.Epoch, 2, protocol.Result{RequestID: "other", State: "denied", Permission: "deny"})}, true},
		{"expired-allow", []protocol.Response{begin, item, end, event(cursor.Epoch, 2, protocol.Result{RequestID: "one", State: "expired", Permission: "allow"})}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				var m protocol.Message
				if protocol.ReadFrame(bufio.NewReader(conn), &m) != nil {
					return
				}
				for _, f := range tc.frames {
					f.ProtocolVersion = protocol.Version
					if protocol.WriteFrame(conn, f) != nil {
						return
					}
				}
			}()
			snapshots, events := 0, 0
			err = (Client{Socket: path}).Subscribe(context.Background(), func(protocol.Snapshot) error { snapshots++; return nil }, func(protocol.QueueEvent) error { events++; return nil })
			if err == nil || events != 0 || (snapshots == 1) != tc.snapshot {
				t.Fatalf("accepted invalid stream: snapshots=%d events=%d err=%v", snapshots, events, err)
			}
			<-done
		})
	}
}

func TestSlowWireSubscriberIsDisconnectedWithoutBlockingQueue(t *testing.T) {
	server, client := startServer(t, time.Hour)
	conn, err := net.Dial("unix", client.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.Message{ProtocolVersion: protocol.Version, Type: "subscribe"}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for range 2 {
		var r protocol.Response
		if err := protocol.ReadFrame(reader, &r); err != nil {
			t.Fatal(err)
		}
	}
	r := request("slow")
	r.Command = strings.Repeat("x", 700000)
	// Stop reading while producing more events than the bounded subscriber queue.
	for i := 0; i < coordinator.SubscriptionBuffer+5; i++ {
		r.RequestID = "slow-" + strings.Repeat("x", i+1)
		result, err := server.queue.Submit(r, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Decide(context.Background(), r.RequestID, "deny"); err != nil {
			t.Fatal(err)
		}
		<-result
	}
	for {
		var response protocol.Response
		if err := protocol.ReadFrame(reader, &response); err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatal("slow subscriber was not disconnected")
			}
			break
		}
	}
	fresh, err := client.Snapshot(context.Background())
	if err != nil || len(fresh.Items) != 0 {
		t.Fatalf("queue blocked by slow subscriber: %v", err)
	}
}

func TestSubscribeAfterDaemonRestartUsesNewEpoch(t *testing.T) {
	path := socketPath(t)
	run := func() (Client, context.CancelFunc, <-chan error) {
		s, err := Listen(path, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()
		return Client{Socket: path}, cancel, done
	}
	client, cancel, serverDone := run()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	snapshots := make(chan protocol.Snapshot, 1)
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(ctx, func(s protocol.Snapshot) error { snapshots <- s; return nil }, func(protocol.QueueEvent) error { return nil })
	}()
	old := receive(t, snapshots)
	cancel()
	if err := receive(t, serverDone); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err == nil {
		t.Fatal("shutdown left stream connected")
	}
	client, cancel, serverDone = run()
	defer func() {
		cancel()
		if err := receive(t, serverDone); err != nil {
			t.Error(err)
		}
	}()
	fresh, err := client.Snapshot(context.Background())
	if err != nil || fresh.Cursor.Epoch == old.Cursor.Epoch || fresh.Cursor.Sequence != 0 || len(fresh.Items) != 0 {
		t.Fatalf("restart snapshot: %+v %v", fresh, err)
	}
}

func TestDurableSubscriptionInvalidatesOnCommitFailure(t *testing.T) {
	_, database, client := durableServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snapshots := make(chan protocol.Snapshot, 1)
	events := make(chan protocol.QueueEvent, 2)
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(ctx, func(s protocol.Snapshot) error { snapshots <- s; return nil }, func(e protocol.QueueEvent) error { events <- e; return nil })
	}()
	receive(t, snapshots)
	waiter := rawSubmit(t, client, request("storage-failure"))
	waitCount(t, client, 1)
	if receive(t, events).Pending == nil {
		t.Fatal("missing committed admission")
	}
	injectSubscriptionCommitFailure(t, database)
	_, err := client.Decide(context.Background(), "storage-failure", "allow")
	wireCode(t, err, "history_unavailable")
	if err := receive(t, done); err == nil {
		t.Fatal("storage failure left observer connected")
	}
	select {
	case e := <-events:
		t.Fatalf("uncommitted terminal event exposed: %+v", e)
	default:
	}
	waiter.SetReadDeadline(time.Now().Add(time.Second))
	var response protocol.Response
	if err := protocol.ReadFrame(bufio.NewReader(waiter), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result == nil || response.Result.Permission != "deny" {
		t.Fatal("storage failure allowed waiter")
	}
	_, err = client.Snapshot(context.Background())
	wireCode(t, err, "history_unavailable")
}

func injectSubscriptionCommitFailure(t *testing.T, database string) {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE subscription_fault(reference TEXT REFERENCES actions(request_id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER subscription_fail_commit AFTER INSERT ON action_events WHEN NEW.state!='pending' BEGIN INSERT INTO subscription_fault(reference) VALUES('missing-request'); END;`)
	if err != nil {
		t.Fatal(err)
	}
}
