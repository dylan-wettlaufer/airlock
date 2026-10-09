// Package reliability exercises real sockets and SQLite without executing proposals.
package reliability

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"airlock/internal/coordinator"
	"airlock/internal/protocol"
	"airlock/internal/store"
	"airlock/internal/transport"
)

// The child is this test executable, never a proposal command. SIGKILL leaves
// pending transactions and the socket for normal startup recovery to handle.
func TestDaemonHelper(t *testing.T) {
	if os.Getenv("AIRLOCK_STRESS_CHILD") != "1" {
		t.Skip("daemon helper runs only in the isolated child process")
	}
	lock, err := transport.AcquireDaemon(os.Getenv("AIRLOCK_STRESS_SOCKET"))
	must(t, err)
	defer lock.Close()
	limit := store.DefaultMaxRecords
	if os.Getenv("AIRLOCK_STRESS_RETAIN") == "20" {
		limit = 20
	}
	db, err := store.OpenWithRetention(os.Getenv("AIRLOCK_STRESS_DB"), store.Retention{MaxAge: store.DefaultMaxAge, MaxRecords: limit})
	must(t, err)
	defer db.Close()
	server, err := lock.Listen(15*time.Second, coordinator.NewWithHistory(db))
	must(t, err)
	must(t, server.Serve(context.Background()))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func request(id string) protocol.Request {
	return protocol.Request{ProtocolVersion: protocol.Version, RequestID: id, Agent: "fixture", AgentVersion: "stress-v1", ConversationID: "sanitized-" + id, Event: "before_shell_execution", Command: "fixture-proposal sanitized-argument", WorkspaceRoots: []string{}}
}
func code(err error, want string) bool {
	var w *protocol.WireError
	return errors.As(err, &w) && w.Code == want
}
func until(t *testing.T, f func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not converge within 5s")
}
func start(t *testing.T, socket, database string, retain int) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonHelper$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "AIRLOCK_STRESS_CHILD=1", "AIRLOCK_STRESS_SOCKET="+socket, "AIRLOCK_STRESS_DB="+database, fmt.Sprintf("AIRLOCK_STRESS_RETAIN=%d", retain))
	// A file avoids unsynchronized buffer reads while the child runs.
	log, err := os.CreateTemp(filepath.Dir(database), "daemon-*.log")
	must(t, err)
	t.Cleanup(func() { log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	must(t, cmd.Start())
	var once sync.Once
	stop := func() {
		once.Do(func() {
			must(t, cmd.Process.Kill())
			if err := cmd.Wait(); err == nil {
				t.Error("killed daemon exited successfully")
			}
			data, err := os.ReadFile(log.Name())
			must(t, err)
			if strings.Contains(string(data), "WARNING: DATA RACE") {
				t.Fatalf("child race detector: %s", data)
			}
		})
	}
	t.Cleanup(stop)
	client := transport.Client{Socket: socket}
	ready := false
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if client.Health(context.Background()) == nil {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		stop()
		data, err := os.ReadFile(log.Name())
		must(t, err)
		t.Fatalf("daemon startup failed: %s", data)
	}
	return stop
}
func history(t *testing.T, database string) []store.Record {
	t.Helper()
	db, err := store.Read(database)
	must(t, err)
	defer db.Close()
	records, err := db.History(context.Background(), store.Query{Limit: 200})
	must(t, err)
	return records
}
func pendingCount(t *testing.T, c transport.Client, n int) []protocol.Pending {
	t.Helper()
	var items []protocol.Pending
	until(t, func() bool {
		var err error
		items, err = c.List(context.Background())
		must(t, err)
		return len(items) == n
	})
	return items
}
func submit(t *testing.T, socket, id string, wait time.Duration) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	must(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
	r := request(id)
	must(t, protocol.WriteFrame(conn, protocol.Message{ProtocolVersion: protocol.Version, Type: "submit", Request: &r, WaitMS: wait.Milliseconds()}))
	return conn
}

type observation struct {
	result  protocol.Result
	err     error
	latency time.Duration
}

func TestStress(t *testing.T) {
	const seed = 42
	const count = 100
	// Short private path also fits Darwin's Unix socket path limit.
	dir, err := os.MkdirTemp("/tmp", "al-stress-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket, database := filepath.Join(dir, "s.sock"), filepath.Join(dir, "history.sqlite3")
	stop := start(t, socket, database, store.DefaultMaxRecords)
	client := transport.Client{Socket: socket}
	ctx := context.Background()
	order := rand.New(rand.NewSource(seed)).Perm(count)
	kinds := make([]string, count)
	for rank, i := range order {
		switch {
		case rank < 30:
			kinds[i] = "allow"
		case rank < 60:
			kinds[i] = "deny"
		case rank < 80:
			kinds[i] = "conflict"
		case rank < 90:
			kinds[i] = "disconnect"
		default:
			kinds[i] = "expiry"
		}
	}
	ids := make([]string, count)
	conns := make([]net.Conn, count)
	observations := make([]observation, count)
	started := make([]time.Time, count)
	// Open/write concurrently, then wait for the explicit admission barrier.
	var writers sync.WaitGroup
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		ids[i] = fmt.Sprintf("stress-%03d", i)
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			started[i] = time.Now()
			conn, err := net.Dial("unix", socket)
			if err != nil {
				failures <- err
				return
			}
			conns[i] = conn
			if err = conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				failures <- err
				return
			}
			r := request(ids[i])
			err = protocol.WriteFrame(conn, protocol.Message{ProtocolVersion: protocol.Version, Type: "submit", Request: &r, WaitMS: 8000})
			if err != nil {
				failures <- err
			}
		}(i)
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		must(t, err)
	}
	defer func() {
		for _, conn := range conns {
			if conn != nil {
				conn.Close()
			}
		}
	}()
	items := pendingCount(t, client, count)
	deadlines := map[string]time.Time{}
	for _, p := range items {
		deadlines[p.Request.RequestID] = p.Deadline
	}
	var readers sync.WaitGroup
	for i, conn := range conns {
		if kinds[i] == "disconnect" {
			must(t, conn.Close())
			continue
		}
		readers.Add(1)
		go func(i int, conn net.Conn) {
			defer readers.Done()
			var response protocol.Response
			err := protocol.ReadFrame(bufio.NewReader(conn), &response)
			if err == nil && (response.ProtocolVersion != protocol.Version || response.Type != "submit" || response.Result == nil || response.Error != nil) {
				err = errors.New("invalid submit response")
			}
			if err == nil {
				observations[i].result = *response.Result
				err = response.Result.Validate(ids[i])
			}
			observations[i].err = err
			observations[i].latency = time.Since(started[i])
			conn.Close()
		}(i, conn)
	}
	type decision struct {
		index  int
		result protocol.Result
		err    error
	}
	decisions := make(chan decision, 100)
	var deciders sync.WaitGroup
	for i, kind := range kinds {
		permissions := []string{}
		if kind == "allow" || kind == "deny" {
			permissions = []string{kind}
		}
		if kind == "conflict" {
			permissions = []string{"allow", "deny"}
		}
		for _, permission := range permissions {
			deciders.Add(1)
			go func(i int, p string) {
				defer deciders.Done()
				r, err := client.Decide(ctx, ids[i], p)
				decisions <- decision{i, r, err}
			}(i, permission)
		}
	}
	deciders.Wait()
	close(decisions)
	winners := map[int]protocol.Result{}
	conflictRejections := 0
	for d := range decisions {
		if d.err != nil {
			if kinds[d.index] != "conflict" || !code(d.err, "not_pending") {
				t.Fatalf("unexpected decision error: %v", d.err)
			}
			conflictRejections++
			continue
		}
		if _, exists := winners[d.index]; exists {
			t.Fatal("two decisions won")
		}
		winners[d.index] = d.result
	}
	if len(winners) != 80 || conflictRejections != 20 {
		t.Fatalf("winners=%d conflict_rejections=%d", len(winners), conflictRejections)
	}
	readers.Wait()
	pendingCount(t, client, 0)
	outcomes := map[string]int{}
	latency := []float64{}
	manualLatency := []float64{}
	records := history(t, database)
	if len(records) != count {
		t.Fatalf("audit count %d", len(records))
	}
	byID := map[string]store.Record{}
	for _, r := range records {
		byID[r.RequestID] = r
	}
	lateRejections := 0
	for i, id := range ids {
		r, ok := byID[id]
		if !ok {
			t.Fatalf("missing audit %s", id)
		}
		expected := kinds[i]
		switch expected {
		case "allow":
			expected = "allowed"
		case "deny":
			expected = "denied"
		case "disconnect":
			expected = "cancelled"
		case "expiry":
			expected = "expired"
		case "conflict":
			expected = winners[i].State
		}
		if r.State != expected || len(r.Events) != 2 || r.Events[0].State != "pending" || r.Events[1].State != expected || r.FinishedAt == nil || r.CommandDisplay != "fixture-proposal [arguments redacted]" {
			t.Fatalf("incorrect audit for %s: %+v", id, r)
		}
		finished, err := time.Parse(time.RFC3339Nano, *r.FinishedAt)
		must(t, err)
		if r.State == "allowed" && !finished.Before(deadlines[id]) {
			t.Fatal("expired approval")
		}
		manual := r.State == "allowed" || r.State == "denied"
		if manual != (r.Decision != nil) {
			t.Fatal("incorrect decision audit")
		}
		if kinds[i] != "disconnect" {
			o := observations[i]
			must(t, o.err)
			if o.result.State != r.State || o.result.Reason != r.Reason {
				t.Fatal("result/audit mismatch")
			}
			if manual && (o.result != winners[i] || r.Decision.Permission != o.result.Permission) {
				t.Fatal("decision routing mismatch")
			}
			latency = append(latency, float64(o.latency)/float64(time.Millisecond))
			if manual {
				manualLatency = append(manualLatency, float64(o.latency)/float64(time.Millisecond))
			}
		}
		if kinds[i] == "expiry" {
			_, err := client.Decide(ctx, id, "allow")
			if !code(err, "not_pending") {
				t.Fatalf("late approval: %v", err)
			}
			lateRejections++
		}
		outcomes[r.State]++
	}
	// Actual process crash with one pending waiter, followed by recovery on the
	// same socket/database. Completed history is checked before tightening limits.
	crashID := "crash-pending"
	crashConn := submit(t, socket, crashID, 15*time.Second)
	pendingCount(t, client, 1)
	stop()
	var response protocol.Response
	if err := protocol.ReadFrame(bufio.NewReader(crashConn), &response); err == nil {
		t.Fatal("crashed waiter received an authorization")
	}
	stop = start(t, socket, database, store.DefaultMaxRecords)
	pendingCount(t, client, 0)
	recovered := history(t, database)
	if len(recovered) != 101 {
		t.Fatal("restart lost completed history")
	}
	for _, r := range recovered {
		if r.RequestID == crashID {
			if r.State != "interrupted" || len(r.Events) != 2 {
				t.Fatal("pending not recovered")
			}
		} else if !reflect.DeepEqual(r, byID[r.RequestID]) {
			t.Fatal("restart changed completed state")
		}
	}
	stop()
	stop = start(t, socket, database, 20)
	retained := history(t, database)
	if len(retained) != 20 {
		t.Fatal("count retention failed")
	}
	recoveredByID := map[string]store.Record{}
	for _, r := range recovered {
		recoveredByID[r.RequestID] = r
	}
	for _, r := range retained {
		if !reflect.DeepEqual(r, recoveredByID[r.RequestID]) {
			t.Fatal("retention changed retained audit")
		}
	}
	allIDs := append(append([]string{}, ids...), crashID)
	duplicateRejections := 0
	verifyIDs := func() {
		for _, id := range allIDs {
			proposal := request(id)
			proposal.Command = "different-fixture sanitized-argument"
			proposal.ConversationID = "different-sanitized-conversation"
			_, err := client.Submit(ctx, proposal, time.Second)
			if !code(err, "duplicate_request") {
				t.Fatalf("old ID %s accepted: %v", id, err)
			}
			duplicateRejections++
		}
		pendingCount(t, client, 0)
	}
	verifyIDs()
	stop()
	db, err := store.OpenWithRetention(database, store.Retention{MaxAge: store.DefaultMaxAge, MaxRecords: 20})
	must(t, err)
	// Advance maintenance time explicitly rather than waiting 30 days.
	must(t, db.Prune(time.Now().Add(31*24*time.Hour)))
	must(t, db.Close())
	stop = start(t, socket, database, 20)
	defer stop()
	if len(history(t, database)) != 0 {
		t.Fatal("age retention failed")
	}
	verifyIDs()
	stats := func(values []float64) map[string]float64 {
		sort.Float64s(values)
		total := 0.0
		for _, v := range values {
			total += v
		}
		return map[string]float64{"min": values[0], "mean": total / float64(len(values)), "p50": values[(len(values)-1)/2], "p95": values[(95*len(values)+99)/100-1], "max": values[len(values)-1]}
	}
	report := struct {
		Seed      int                `json:"seed"`
		Requests  int                `json:"concurrent_requests"`
		Platform  string             `json:"platform"`
		Go        string             `json:"go"`
		Outcomes  map[string]int     `json:"outcomes"`
		Errors    int                `json:"unexpected_errors"`
		Conflict  int                `json:"conflict_rejections"`
		Late      int                `json:"late_rejections"`
		Duplicate int                `json:"duplicate_rejections"`
		Crash     int                `json:"crash_pending_requests"`
		Latency   map[string]float64 `json:"connected_latency_ms"`
		Manual    map[string]float64 `json:"manual_latency_ms"`
	}{seed, count, runtime.GOOS + "/" + runtime.GOARCH, runtime.Version(), outcomes, 0, conflictRejections, lateRejections, duplicateRejections, 1, stats(latency), stats(manualLatency)}
	data, err := json.Marshal(report)
	must(t, err)
	t.Log(string(data))
}
