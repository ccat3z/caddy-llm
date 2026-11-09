package claudeservertool

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxResponseSize = 128 << 20

type responseWriter struct {
	w           http.ResponseWriter
	tools       map[string]toolSpec
	status      int
	committed   bool
	stream      bool
	compressed  bool
	passthrough bool
	buf         bytes.Buffer
	index       int
	indices     map[int]int
	pending     map[int]map[string]any
	serverIDs   map[string]bool
	clientCalls bool
	serverCalls bool
	completed   map[int]bool
	err         error
}

func newResponseWriter(w http.ResponseWriter, tools map[string]toolSpec) *responseWriter {
	return &responseWriter{w: w, tools: tools, indices: make(map[int]int), pending: make(map[int]map[string]any), serverIDs: make(map[string]bool), completed: make(map[int]bool)}
}
func (rw *responseWriter) Header() http.Header { return rw.w.Header() }
func (rw *responseWriter) WriteHeader(status int) {
	if rw.status != 0 {
		return
	}
	rw.status = status
	rw.stream = strings.Contains(rw.Header().Get("Content-Type"), "text/event-stream")
	rw.compressed = rw.Header().Get("Content-Encoding") == "gzip"
	rw.passthrough = status != http.StatusOK || (!rw.stream && !strings.Contains(rw.Header().Get("Content-Type"), "application/json"))
	if rw.passthrough {
		rw.commit()
	}
}
func (rw *responseWriter) commit() {
	if rw.committed {
		return
	}
	if !rw.passthrough {
		rw.Header().Del("Content-Length")
		rw.Header().Del("Content-Encoding")
	}
	rw.w.WriteHeader(rw.status)
	rw.committed = true
}
func (rw *responseWriter) Write(p []byte) (int, error) {
	if rw.status == 0 {
		rw.WriteHeader(http.StatusOK)
	}
	if rw.passthrough {
		return rw.w.Write(p)
	}
	if rw.err != nil {
		return 0, rw.err
	}
	if rw.buf.Len()+len(p) > maxResponseSize {
		rw.err = fmt.Errorf("claudeservertool: response exceeds size limit")
		return 0, rw.err
	}
	rw.buf.Write(p)
	if rw.stream && !rw.compressed {
		rw.err = rw.drain(false)
	}
	if rw.err != nil {
		return 0, rw.err
	}
	return len(p), nil
}
func (rw *responseWriter) Flush() {
	if rw.committed {
		if f, ok := rw.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (rw *responseWriter) emit(event string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	rw.commit()
	_, err = fmt.Fprintf(rw.w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
func (rw *responseWriter) drain(final bool) error {
	for {
		b := rw.buf.Bytes()
		n := bytes.IndexByte(b, '\n')
		// Consume one complete SSE event, allowing CRLF and arbitrary Write splits.
		end := -1
		pos := 0
		for n >= 0 {
			if len(bytes.TrimSpace(b[pos:pos+n])) == 0 {
				end = pos + n + 1
				break
			}
			pos += n + 1
			n = bytes.IndexByte(b[pos:], '\n')
		}
		if end < 0 {
			if !final || len(b) == 0 {
				return nil
			}
			end = len(b)
		}
		raw := append([]byte(nil), rw.buf.Next(end)...)
		var parts []string
		event := ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(line[6:])
			}
			if strings.HasPrefix(line, "data:") {
				parts = append(parts, strings.TrimPrefix(line[5:], " "))
			}
		}
		if len(parts) == 0 {
			rw.commit()
			if _, err := rw.w.Write(raw); err != nil {
				return err
			}
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.Join(parts, "\n")), &data); err != nil {
			rw.commit()
			if _, err := rw.w.Write(raw); err != nil {
				return err
			}
			continue
		}
		if event == "" {
			event, _ = data["type"].(string)
		}
		if err := rw.transformEvent(event, data); err != nil {
			return err
		}
	}
}
func (rw *responseWriter) transformEvent(event string, data map[string]any) error {
	old, _ := data["index"].(float64)
	switch event {
	case "content_block_start":
		block, _ := data["content_block"].(map[string]any)
		kind, _ := block["type"].(string)
		id, _ := block["tool_use_id"].(string)
		if rw.serverIDs[id] {
			for index, result := range rw.pending {
				if result["tool_use_id"] == id {
					delete(rw.pending, index)
				}
			}
		}
		rw.indices[int(old)] = rw.index
		data["index"] = rw.index
		rw.index++
		name, _ := block["name"].(string)
		s, ok := rw.callSpec(name, kind)
		if ok && (kind == "tool_use" || kind == "server_tool_use") {
			block["type"] = "server_tool_use"
			rw.serverCalls = true
			callID, _ := block["id"].(string)
			rw.serverIDs[callID] = true
			rw.pending[int(old)] = s.unavailable(callID)
			if _, exists := block["caller"]; !exists {
				block["caller"] = map[string]any{"type": "direct"}
			}
		} else if kind == "tool_use" {
			rw.clientCalls = true
		}
	case "content_block_delta", "content_block_stop":
		mapped, ok := rw.indices[int(old)]
		if !ok {
			return fmt.Errorf("claudeservertool: event references unknown content block")
		}
		if mapped < 0 {
			return nil
		}
		data["index"] = mapped
		if event == "content_block_stop" {
			rw.completed[int(old)] = true
		}
	case "message_delta":
		delta, _ := data["delta"].(map[string]any)
		stop, _ := delta["stop_reason"].(string)
		if stop == "max_tokens" || stop == "refusal" {
			rw.pending = make(map[int]map[string]any)
		}
		for index := 0; index < rw.index; index++ {
			if result, ok := rw.pending[index]; ok && rw.completed[index] {
				i := rw.index
				rw.index++
				if err := rw.emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": result}); err != nil {
					return err
				}
				if err := rw.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}); err != nil {
					return err
				}
				delete(rw.pending, index)
			}
		}
		if rw.serverCalls && !rw.clientCalls && len(rw.pending) == 0 {
			if delta, ok := data["delta"].(map[string]any); ok && delta["stop_reason"] == "tool_use" {
				delta["stop_reason"] = "end_turn"
			}
		}
	}
	return rw.emit(event, data)
}
func (rw *responseWriter) finish() error {
	if rw.err != nil {
		return rw.err
	}
	if rw.status == 0 {
		rw.WriteHeader(http.StatusOK)
	}
	if rw.passthrough {
		return nil
	}
	if rw.compressed {
		gz, err := gzip.NewReader(bytes.NewReader(rw.buf.Bytes()))
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(gz, maxResponseSize+1))
		gz.Close()
		if err != nil {
			return err
		}
		if len(b) > maxResponseSize {
			return fmt.Errorf("claudeservertool: decompressed response exceeds size limit")
		}
		rw.buf.Reset()
		rw.buf.Write(b)
	}
	if rw.stream {
		return rw.drain(true)
	}
	var msg map[string]any
	if err := json.Unmarshal(rw.buf.Bytes(), &msg); err != nil {
		rw.commit()
		_, err = rw.w.Write(rw.buf.Bytes())
		return err
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		rw.commit()
		_, err := rw.w.Write(rw.buf.Bytes())
		return err
	}
	if msg["stop_reason"] == "max_tokens" || msg["stop_reason"] == "refusal" {
		rw.commit()
		_, err := rw.w.Write(rw.buf.Bytes())
		return err
	}
	results := make(map[string]bool)
	for _, v := range blocks {
		b, _ := v.(map[string]any)
		id, _ := b["tool_use_id"].(string)
		if id != "" {
			results[id] = true
		}
	}
	out := make([]any, 0, len(blocks)*2)
	server, client := false, false
	for _, v := range blocks {
		b, _ := v.(map[string]any)
		kind, _ := b["type"].(string)
		name, _ := b["name"].(string)
		s, ok := rw.callSpec(name, kind)
		if ok && (kind == "tool_use" || kind == "server_tool_use") {
			b["type"] = "server_tool_use"
			if _, exists := b["caller"]; !exists {
				b["caller"] = map[string]any{"type": "direct"}
			}
			out = append(out, b)
			callID, _ := b["id"].(string)
			if !results[callID] {
				out = append(out, s.unavailable(b["id"]))
			}
			server = true
			continue
		}
		if kind == "tool_use" {
			client = true
		}
		out = append(out, v)
	}
	if !server {
		rw.commit()
		_, err := rw.w.Write(rw.buf.Bytes())
		return err
	}
	msg["content"] = out
	if !client && msg["stop_reason"] == "tool_use" {
		msg["stop_reason"] = "end_turn"
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	rw.commit()
	_, err = rw.w.Write(raw)
	return err
}

func (rw *responseWriter) callSpec(name, kind string) (toolSpec, bool) {
	if s, ok := rw.tools[name]; ok {
		return s, true
	}
	if kind != "server_tool_use" {
		return toolSpec{}, false
	}
	for _, family := range []string{"web_search_", "web_fetch_", "code_execution_20250522", "code_execution_20260521", "tool_search_tool_regex_", "tool_search_tool_bm25_", "advisor_"} {
		for _, s := range serverSpecs(family) {
			if s.name == name {
				return s, true
			}
		}
	}
	return toolSpec{}, false
}

var _ http.Flusher = (*responseWriter)(nil)
