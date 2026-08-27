package claudetoopenai

import "bytes"

// StreamFeeder is the incremental bridge between a raw upstream SSE byte
// stream and a StreamConverter: it buffers partial lines, decodes complete
// `data:` lines, feeds their payloads to the converter, and returns the
// emitted Claude events. Create one per request; call Close (the converter's
// Done) when the upstream stream ends.
type StreamFeeder struct {
	converter *StreamConverter
	buf       bytes.Buffer
}

// NewStreamFeeder creates a feeder for the given client-facing model name.
func NewStreamFeeder(model string) *StreamFeeder {
	return &StreamFeeder{converter: NewStreamConverter(model)}
}

// Write consumes more upstream SSE bytes and returns newly emitted events.
func (f *StreamFeeder) Write(p []byte) ([]SSEEvent, error) {
	if _, err := f.buf.Write(p); err != nil {
		return nil, err
	}
	var out []SSEEvent
	for {
		line, err := f.buf.ReadString('\n')
		if err != nil {
			// Incomplete trailing line — keep it buffered.
			f.buf.Reset()
			f.buf.WriteString(line)
			break
		}
		d, ok := parseDataLine(trimEOL(line))
		if !ok {
			continue // event:/comment/blank lines carry no payload
		}
		if d.done {
			continue // [DONE] is finalized by Close
		}
		evts, err := f.converter.Feed(d.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, evts...)
	}
	return out, nil
}

// Close finalizes the stream and returns the terminal events.
func (f *StreamFeeder) Close() ([]SSEEvent, error) {
	return f.converter.Done()
}

// Error terminates with a mid-stream error event.
func (f *StreamFeeder) Error(status int, body []byte) []SSEEvent {
	return f.converter.Error(status, body)
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
