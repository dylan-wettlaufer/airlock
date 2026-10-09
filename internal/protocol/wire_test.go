package protocol

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestFrameBoundaries(t *testing.T) {
	for _, input := range []string{
		"{\n", "null\n", "{} {}\n", "{}", "{\"protocol_version\":1,\"type\":\"health\",\"unknown\":true}\n",
		strings.Repeat(" ", MaxFrameBytes) + "\n",
	} {
		var m Message
		err := ReadFrame(bufio.NewReader(strings.NewReader(input)), &m)
		if err == nil {
			err = m.Validate()
		}
		if err == nil {
			t.Fatalf("accepted invalid frame of length %d", len(input))
		}
	}
	var b bytes.Buffer
	m := Message{ProtocolVersion: Version, Type: "health"}
	if err := WriteFrame(&b, m); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, m); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(&b, 16)
	for i := 0; i < 2; i++ {
		var got Message
		if err := ReadFrame(reader, &got); err != nil {
			t.Fatal(err)
		}
		if err := got.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResultValidation(t *testing.T) {
	for _, r := range []Result{
		{RequestID: "wrong", State: "allowed", Permission: "allow"},
		{RequestID: "one", State: "expired", Permission: "allow"},
		{RequestID: "one", State: "pending", Permission: "allow"},
		{RequestID: "one", State: "allowed", Permission: "deny"},
	} {
		if err := r.Validate("one"); err == nil {
			t.Fatalf("accepted invalid result: %+v", r)
		}
	}
}
