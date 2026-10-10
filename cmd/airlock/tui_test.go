package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTUIOptionsAndHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		exit int
		want string
	}{
		{[]string{"--help"}, 0, "socket"},
		{[]string{"--unknown"}, 1, "flag provided"},
		{[]string{"unexpected"}, 1, "usage: airlock tui"},
	} {
		var out, diagnostics bytes.Buffer
		if code := run(context.Background(), append([]string{"tui"}, tc.args...), strings.NewReader(""), &out, &diagnostics); code != tc.exit || !strings.Contains(diagnostics.String(), tc.want) {
			t.Fatalf("options %v: exit %d, %s", tc.args, code, diagnostics.String())
		}
	}
	var help bytes.Buffer
	usage(&help)
	if !strings.Contains(help.String(), "tui [--socket PATH]") {
		t.Fatal("TUI missing from help")
	}
}
