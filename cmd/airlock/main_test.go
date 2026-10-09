package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"airlock/internal/adapters/cursor"
)

func TestHookProcess(t *testing.T) {
	payload, err := os.ReadFile("../../testdata/cursor/before-shell-execution.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		args    []string
		payload string
		want    string
	}{
		{"deny", []string{"--spike"}, string(payload), "deny"},
		{"allow", []string{"--spike", "--decision", "allow"}, string(payload), "allow"},
		{"default closed", nil, string(payload), "deny"},
		{"allow requires spike", []string{"--decision", "allow"}, string(payload), "deny"},
		{"malformed input", []string{"--spike", "--decision", "allow"}, "{", "deny"},
		{"invalid options", []string{"--spike", "--decision", "yes"}, string(payload), "deny"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--", "hook", "--agent", "cursor"}, test.args...)...)
			cmd.Env = append(os.Environ(), "AIRLOCK_TEST_HELPER=1")
			cmd.Stdin = strings.NewReader(test.payload)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			var response cursor.Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("stdout must contain only one native JSON response: %q", stdout.String())
			}
			if response.Permission != test.want {
				t.Fatalf("got %s, want %s", response.Permission, test.want)
			}
			if strings.Contains(stderr.String(), "printf") {
				t.Fatal("command leaked to diagnostics")
			}
		})
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("AIRLOCK_TEST_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(run(context.Background(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr))
		}
	}
	os.Exit(1)
}

func TestDelayCancellation(t *testing.T) {
	payload, err := os.ReadFile("../../testdata/cursor/before-shell-execution.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := hook(ctx, []string{"--spike", "--decision", "allow", "--delay", "1h"}, bytes.NewReader(payload), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var response cursor.Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Permission != "deny" {
		t.Fatal("cancelled probe allowed")
	}
}

func TestFailureProbes(t *testing.T) {
	payload, err := os.ReadFile("../../testdata/cursor/before-shell-execution.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"nonzero", "empty", "malformed"} {
		var stdout, stderr bytes.Buffer
		code := hook(context.Background(), []string{"--spike", "--failure", mode}, bytes.NewReader(payload), &stdout, &stderr)
		if mode == "nonzero" && (code != 1 || stdout.Len() != 0) {
			t.Fatal("nonzero probe must fail with no stdout")
		}
		if mode == "empty" && (code != 0 || stdout.Len() != 0) {
			t.Fatal("empty probe must succeed with no stdout")
		}
		if mode == "malformed" && (code != 0 || json.Valid(stdout.Bytes())) {
			t.Fatal("malformed probe must write invalid JSON")
		}
	}
}
