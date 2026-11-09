package claudeservertool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/ccat3z/caddy-llm/claudetoopenai"
	jsonbody "github.com/ccat3z/caddy-llm/jsonbody"
)

const searchRequest = `{"model":"test","tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"search"}]}`

func TestMultipleAndMixedCalls(t *testing.T) {
	for _, client := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("client=%v/stream=%v", client, stream), func(t *testing.T) {
				calls := []any{
					map[string]any{"type": "tool_use", "id": "s1", "name": "web_search", "input": map[string]any{"query": "first"}},
					map[string]any{"type": "tool_use", "id": "s2", "name": "web_search", "input": map[string]any{"query": "second"}},
				}
				if client {
					calls = append(calls, map[string]any{"type": "tool_use", "id": "c1", "name": "Read", "input": map[string]any{"path": "test"}})
				}
				rec := httptest.NewRecorder()
				err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(searchRequest)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						return json.NewEncoder(w).Encode(map[string]any{"content": calls, "stop_reason": "tool_use", "id": "msg_keep", "usage": map[string]any{"output_tokens": 15}})
					}
					w.Header().Set("Content-Type", "text/event-stream")
					var b bytes.Buffer
					emit := func(kind string, data map[string]any) {
						raw, _ := json.Marshal(data)
						fmt.Fprintf(&b, "event: %s\r\ndata:%s\r\n\r\n", kind, raw)
					}
					emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_keep", "content": []any{}}})
					// Interleave the starts/deltas to exercise independent indices.
					for i, c := range calls {
						block := c.(map[string]any)
						emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": block})
					}
					for i := range calls {
						emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}})
						emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
					}
					emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 15}})
					emit("message_stop", map[string]any{"type": "message_stop"})
					for _, v := range b.Bytes() {
						if _, err := w.Write([]byte{v}); err != nil {
							return err
						}
					}
					return nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				if rec.Code != 200 {
					t.Fatal(rec.Code)
				}
				wantStop := "end_turn"
				if client {
					wantStop = "tool_use"
				}
				if stream {
					var results int
					var stops string
					indices := map[int]bool{}
					for _, event := range strings.Split(rec.Body.String(), "\n\n") {
						for _, line := range strings.Split(event, "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							var d map[string]any
							if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &d); err != nil {
								t.Fatal(err)
							}
							if d["type"] == "content_block_start" {
								idx := int(d["index"].(float64))
								if indices[idx] {
									t.Fatalf("duplicate index %d", idx)
								}
								indices[idx] = true
								b := d["content_block"].(map[string]any)
								if b["type"] == "web_search_tool_result" {
									results++
									if b["content"].(map[string]any)["error_code"] != "unavailable" {
										t.Fatal(b)
									}
								}
							}
							if d["type"] == "message_delta" {
								stops = d["delta"].(map[string]any)["stop_reason"].(string)
							}
						}
					}
					if results != 2 || stops != wantStop {
						t.Fatalf("results %d stop %s: %s", results, stops, rec.Body.String())
					}
				} else {
					var d map[string]any
					json.Unmarshal(rec.Body.Bytes(), &d)
					blocks := d["content"].([]any)
					if len(blocks) != len(calls)+2 || d["stop_reason"] != wantStop || d["id"] != "msg_keep" {
						t.Fatalf("message %s", rec.Body.String())
					}
				}
			})
		}
	}
}

func TestDeclarationWithoutCallAndErrors(t *testing.T) {
	for _, status := range []int{200, 429, 502} {
		rec := httptest.NewRecorder()
		raw := `{"content":[{"type":"text","text":"no tool needed"}],"stop_reason":"end_turn"}`
		err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(searchRequest)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, e := w.Write([]byte(raw))
			return e
		}))
		if err != nil || rec.Code != status || rec.Body.String() != raw {
			t.Fatalf("%d: %v %s", status, err, rec.Body.String())
		}
	}
}

func TestUnknownTypeNotBlacklisted(t *testing.T) {
	body := `{"tools":[{"type":"future_client_20300101","name":"web_search"}]}`
	rec := httptest.NewRecorder()
	err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		got, _ := jsonbody.FromBody(r)
		defs, _ := got.Get("tools").List()
		kind, _ := defs[0].Get("type").String()
		if kind != "future_client_20300101" {
			t.Fatalf("type %s", kind)
		}
		_, e := w.Write([]byte("untouched"))
		return e
	}))
	if err != nil || rec.Body.String() != "untouched" {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
}

func TestExistingResultAndTruncation(t *testing.T) {
	for _, stop := range []string{"end_turn", "max_tokens", "refusal"} {
		raw := fmt.Sprintf(`{"content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"hi"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":[{"type":"web_search_result","url":"https://example.com"}]}],"stop_reason":%q}`, stop)
		rec := httptest.NewRecorder()
		err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(searchRequest)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(raw))
			return err
		}))
		if err != nil || strings.Contains(rec.Body.String(), "unavailable") {
			t.Fatalf("existing result changed: %v %s", err, rec.Body.String())
		}
	}
}

func TestTranslatorIntegration(t *testing.T) {
	rec := httptest.NewRecorder()
	translator := &claudetoopenai.Claude2OpenAI{}
	err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(searchRequest)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return translator.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			body, err := jsonbody.FromBody(r)
			if err != nil {
				return err
			}
			defs, _ := body.Get("tools").List()
			params := defs[0].Get("function").Get("parameters")
			if params.Type() != jsonbody.TypeObject {
				t.Fatal("missing function schema")
			}
			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write([]byte(`{"id":"msg_integration","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"hi\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`))
			return err
		}))
	}))
	if err != nil || rec.Code != 200 || !strings.Contains(rec.Body.String(), `"error_code":"unavailable"`) || !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("integration: %v %s", err, rec.Body.String())
	}
}

func TestHistoryMixedResults(t *testing.T) {
	raw := `{"model":"test","tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"hi"}},{"type":"web_search_tool_result","tool_use_id":"s1","content":{"type":"web_search_tool_result_error","error_code":"unavailable"}},{"type":"tool_use","id":"c1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"},{"type":"text","text":"continue"}]}]}`
	jb, err := jsonbody.NewJsonBodyFromBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	prepared, _ := prepareRequest(&jb.LazyJsonNode)
	translated, err := claudetoopenai.TranslateRequest(prepared)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := translated.Marshal()
	var req map[string]any
	json.Unmarshal(wire, &req)
	messages := req["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages %s", wire)
	}
	for i, role := range []string{"assistant", "tool", "tool", "user"} {
		if messages[i].(map[string]any)["role"] != role {
			t.Fatalf("history %s", wire)
		}
	}
	if !strings.Contains(messages[1].(map[string]any)["content"].(string), "unavailable") {
		t.Fatalf("error lost %s", wire)
	}
}
