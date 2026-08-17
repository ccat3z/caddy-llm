package translate

import (
	"bytes"
	"fmt"
	"strings"
)

// SSEEvent is one Anthropic Messages API server-sent event.
type SSEEvent struct {
	Name string // event name, e.g. "message_start"; "" for data-only
	Data []byte // JSON payload
}

// Encode renders the event in SSE wire format.
func (e SSEEvent) Encode() []byte {
	var b bytes.Buffer
	if e.Name != "" {
		fmt.Fprintf(&b, "event: %s\n", e.Name)
	}
	b.Write(e.Data)
	b.WriteByte('\n')
	b.WriteByte('\n')
	return b.Bytes()
}

// EncodeAll renders a sequence of events.
func EncodeAll(events []SSEEvent) []byte {
	var b bytes.Buffer
	for _, e := range events {
		b.Write(e.Encode())
	}
	return b.Bytes()
}

// ScanSSEData extracts the JSON payloads of complete `data:` lines from an SSE
// buffer, returning the payloads and any trailing incomplete line. `data:
// [DONE]` is returned as a nil payload with done=true in order.
type sseScanner struct {
	buf     bytes.Buffer
	pending []sseData // decoded data lines from the last Scan call
}

type sseData struct {
	payload []byte // nil for [DONE]
	done    bool
}

// Write consumes more upstream SSE bytes.
func (s *sseScanner) Write(p []byte) error {
	_, err := s.buf.Write(p)
	return err
}

// Scan splits buffered bytes at newline boundaries and returns complete data
// lines. Incomplete trailing bytes stay buffered.
func (s *sseScanner) Scan() []sseData {
	s.pending = nil
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// Incomplete line — put it back.
			s.buf.Reset()
			s.buf.WriteString(line)
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if payload, ok := parseDataLine(line); ok {
			s.pending = append(s.pending, payload)
		}
		// Non-data lines (event:, comments, blanks) are ignored: OpenAI
		// chunks are self-describing JSON.
	}
	return s.pending
}

// parseDataLine recognizes "data: <payload>" lines.
func parseDataLine(line string) (sseData, bool) {
	if !strings.HasPrefix(line, "data:") {
		return sseData{}, false
	}
	rest := strings.TrimLeft(line[5:], " ")
	if rest == "[DONE]" {
		return sseData{done: true}, true
	}
	return sseData{payload: []byte(rest)}, true
}
