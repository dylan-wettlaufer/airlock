package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxFrameBytes   = 2 << 20
	MaxRequestBytes = 1 << 20
	DefaultWait     = 25 * time.Second
	MaxWait         = 24 * time.Hour
)

// Message is one client operation per connection. Submit keeps the connection open.
type Message struct {
	ProtocolVersion int      `json:"protocol_version"`
	Type            string   `json:"type"`
	Request         *Request `json:"request,omitempty"`
	RequestID       string   `json:"request_id,omitempty"`
	Permission      string   `json:"permission,omitempty"`
	WaitMS          int64    `json:"wait_ms,omitempty"`
}

type Pending struct {
	Request    Request   `json:"request"`
	ReceivedAt time.Time `json:"received_at"`
	Deadline   time.Time `json:"deadline"`
}

type Result struct {
	RequestID  string `json:"request_id"`
	State      string `json:"state"`
	Permission string `json:"permission"`
	Reason     string `json:"reason"`
}

// List sends zero or more pending frames followed by a list frame. This keeps
// every frame bounded even when the queue contains many large commands.
type Response struct {
	ProtocolVersion int        `json:"protocol_version"`
	Type            string     `json:"type"`
	Pending         *Pending   `json:"pending,omitempty"`
	Result          *Result    `json:"result,omitempty"`
	Error           *WireError `json:"error,omitempty"`
}

type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *WireError) Error() string { return e.Message }

func Error(code, message string) *WireError { return &WireError{Code: code, Message: message} }

func (r Request) Validate() error {
	if r.ProtocolVersion != Version || r.RequestID == "" || len(r.RequestID) > 128 ||
		strings.TrimSpace(r.Agent) == "" || strings.TrimSpace(r.ConversationID) == "" ||
		r.Event != "before_shell_execution" || strings.TrimSpace(r.Command) == "" {
		return Error("invalid_request", "invalid proposal fields or version")
	}
	for _, ch := range r.RequestID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return Error("invalid_request", "request ID must use letters, digits, hyphens, or underscores")
		}
	}
	if r.CWD != "" && !filepath.IsAbs(r.CWD) {
		return Error("invalid_request", "cwd must be absolute when supplied")
	}
	for _, root := range r.WorkspaceRoots {
		if !filepath.IsAbs(root) {
			return Error("invalid_request", "workspace roots must be absolute")
		}
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxRequestBytes {
		return Error("invalid_request", "proposal exceeds 1 MiB")
	}
	return nil
}

func (m Message) Validate() error {
	if m.ProtocolVersion != Version {
		return Error("unsupported_version", "unsupported protocol version")
	}
	switch m.Type {
	case "submit":
		if m.Request == nil || m.RequestID != "" || m.Permission != "" || m.WaitMS < 0 || m.WaitMS > MaxWait.Milliseconds() {
			return Error("invalid_message", "invalid submit fields or wait duration")
		}
		return m.Request.Validate()
	case "decide":
		if m.RequestID == "" || (m.Permission != "allow" && m.Permission != "deny") || m.Request != nil || m.WaitMS != 0 {
			return Error("invalid_message", "decision requires a request ID and allow or deny")
		}
	case "list", "health":
		if m.Request != nil || m.RequestID != "" || m.Permission != "" || m.WaitMS != 0 {
			return Error("invalid_message", "unexpected operation fields")
		}
	default:
		return Error("invalid_message", "unknown operation")
	}
	return nil
}

// ReadFrame requires a newline terminator and rejects unknown wire fields.
func ReadFrame(r *bufio.Reader, v interface{}) error {
	var data []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(data)+len(part) > MaxFrameBytes {
			return Error("frame_too_large", "frame exceeds 2 MiB")
		}
		data = append(data, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return Error("invalid_frame", "invalid JSON frame")
	}
	var extra interface{}
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return Error("invalid_frame", "frame must contain one JSON value")
	}
	return nil
}

func WriteFrame(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxFrameBytes {
		return Error("frame_too_large", "frame exceeds 2 MiB")
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (r Result) Validate(requestID string) error {
	if r.RequestID != requestID {
		return errors.New("mismatched result ID")
	}
	switch r.State {
	case "allowed":
		if r.Permission == "allow" {
			return nil
		}
	case "denied", "expired", "cancelled", "interrupted":
		if r.Permission == "deny" {
			return nil
		}
	}
	return errors.New("invalid authorization result")
}
