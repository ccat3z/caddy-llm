package claudetoopenai

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	caddyllm "github.com/ccat3z/caddy-llm/internal"
)

// proxyConfig builds the JSON config: /v1/messages goes through
// claude2openai to the upstream; sub-resources (/v1/messages/*) pass through
// unmanaged; everything else 404s.
func proxyConfig(upstreamDial string) string {
	cfg := map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("localhost:%d", testPorts[1])},
		"apps": map[string]any{
			"http": map[string]any{
				"http_port": testPorts[0],
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen":          []string{fmt.Sprintf("127.0.0.1:%d", testPorts[0])},
						"automatic_https": map[string]any{"disable": true},
						"routes": []any{
							map[string]any{
								"match": []any{map[string]any{
									"path":   []string{"/v1/messages"},
									"method": []string{"POST"},
								}},
								"handle": []any{
									map[string]any{"handler": "rewrite", "uri": "/v1/chat/completions"},
									map[string]any{"handler": "claude2openai"},
									map[string]any{
										"handler":   "reverse_proxy",
										"upstreams": []any{map[string]any{"dial": upstreamDial}},
									},
								},
							},
							// Anthropic sub-resources (count_tokens etc.) pass
							// through unmanaged. JSON routes match in order, so
							// this must precede nothing (exact path above) and
							// follow the messages route.
							map[string]any{
								"match":  []any{map[string]any{"path": []string{"/v1/messages/*"}}},
								"handle": []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": upstreamDial}}}},
							},
							map[string]any{
								"handle": []any{map[string]any{"handler": "static_response", "status_code": 404}},
							},
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// testPorts holds the per-process random ports tests listen on, so parallel
// package runs never collide. Allocated once per test binary.
var testPorts = func() [2]int {
	for range 50 {
		l1, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		p1 := l1.Addr().(*net.TCPAddr).Port
		l2, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			l1.Close()
			continue
		}
		p2 := l2.Addr().(*net.TCPAddr).Port
		l1.Close()
		l2.Close()
		return [2]int{p1, p2}
	}
	panic("no free ports for tests")
}()

// httpBase is the URL tests post to.
func httpBase() string { return fmt.Sprintf("http://127.0.0.1:%d", testPorts[0]) }

// startProxy launches a caddytest server proxying /v1/messages through
// claude2openai to the given upstream.
func startProxy(t *testing.T, upstreamURL string) *caddytest.Tester {
	t.Helper()
	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(proxyConfig(strings.TrimPrefix(upstreamURL, "http://")), "json")
	return tester
}

// post posts a body to the tester's server and returns status + body.
func post(t *testing.T, tester *caddytest.Tester, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(httpBase()+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestNonStreamingRoundTrip: Claude request -> handler -> mock OpenAI upstream
// -> translated Claude response.
func TestNonStreamingRoundTrip(t *testing.T) {
	var gotUpstreamPath string
	var gotUpstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpstreamPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotUpstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc-1","object":"chat.completion","model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"client-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if gotUpstreamPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotUpstreamPath)
	}
	// Upstream saw the translated OpenAI request.
	if gotUpstreamBody["model"] != "client-model" {
		t.Errorf("upstream model = %v", gotUpstreamBody["model"])
	}
	msgs := gotUpstreamBody["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("upstream messages = %v", msgs)
	}
	if gotUpstreamBody["stream"] != false {
		t.Errorf("upstream stream = %v", gotUpstreamBody["stream"])
	}
	// Client got a Claude-format response.
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("client body: %v (%s)", err, body)
	}
	if out["type"] != "message" || out["model"] != "client-model" {
		t.Errorf("client body = %s", body)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello there" {
		t.Errorf("content = %v", content)
	}
}

// TestStreamingRoundTrip: Claude streaming request -> incremental SSE.
func TestStreamingRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n",
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"client-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		"event: message_start",
		`"model":"client-model"`,
		"event: content_block_start",
		`"type":"text_delta"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		`"input_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q\nstream: %s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("[DONE] leaked to client")
	}
}

// TestUpstreamErrorTranslation: 429 from upstream becomes a Claude error body.
func TestUpstreamErrorTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	if status != 429 {
		t.Fatalf("status = %d %s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	e := out["error"].(map[string]any)
	if e["type"] != "rate_limit" || e["message"] != "slow down" {
		t.Errorf("error = %v", e)
	}
}

// TestCountTokensPassthrough: sub-resources like /v1/messages/count_tokens
// are outside the matched route, so they reach the upstream untouched.
func TestCountTokensPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":5}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if status != 200 || strings.TrimSpace(body) != `{"input_tokens":5}` {
		t.Errorf("count_tokens passthrough broken: %d %s", status, body)
	}
}

// TestUnmatchedPathNotProxied: paths outside the Claude matcher never reach
// the upstream chain.
func TestUnmatchedPathNotProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be reached")
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, _ := post(t, tester, "/v1/models", "")
	if status != 404 {
		t.Errorf("unmatched path status = %d, want 404", status)
	}
}

// TestInvalidClaudeBody: malformed request body gets a 400 Claude error
// without reaching the next handler.
func TestInvalidClaudeBody(t *testing.T) {
	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	cfg, err := json.Marshal(map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("localhost:%d", testPorts[1])},
		"apps": map[string]any{
			"http": map[string]any{
				"http_port": testPorts[0],
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen":          []string{fmt.Sprintf("127.0.0.1:%d", testPorts[0])},
						"automatic_https": map[string]any{"disable": true},
						"routes": []any{
							map[string]any{
								"handle": []any{
									map[string]any{"handler": "claude2openai"},
									map[string]any{"handler": "static_response", "status_code": 200, "body": "unreachable"},
								},
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tester.InitServer(string(cfg), "json")

	status, body := post(t, tester, "/v1/messages", `{not json`)
	if status != 400 {
		t.Fatalf("status = %d %s", status, body)
	}
	if !strings.Contains(body, "invalid_request_error") {
		t.Errorf("body = %s", body)
	}
}

// TestGetBodyUsable: the replaced request body must be re-readable
// (reverse_proxy retry support).
func TestGetBodyUsable(t *testing.T) {
	var reads int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, _ := post(t, tester, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if reads == 0 {
		t.Error("upstream never called")
	}
}

// ---------- recorded-corpus fixtures (testdata/*.json) ----------
//
// Files without the synthetic_ prefix are sanitized extracts of real
// CLIProxyAPI proxy exchanges (credentials, cookies, session IDs, hostnames,
// and user paths redacted — guarded by TestFixturesSanitized below);
// synthetic_*.json are hand-written cases for request/response shapes the
// recorded traffic never contained. One file per exchange:
//
//	request  — the client's Anthropic /v1/messages request (header + body)
//	response — the upstream OpenAI chat-completions response that answered it:
//	           header carries ":status" (plus Content-Type), body holds the
//	           buffered JSON payload (or a string for non-JSON bodies), and
//	           sse lists the upstream `data:` payloads in arrival order.
//
// Expectations in these tests are computed from the recorded upstream data by
// independent readers (not the translator's own code), so a translation bug
// has to break the invariant, not just echo through it.

// fixture is one parsed testdata exchange.
type fixture struct {
	Name string `json:"name"`
	// File is the testdata file the exchange was loaded from.
	File    string `json:"-"`
	Request struct {
		Header map[string]string `json:"header"`
		Body   json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Header map[string]string `json:"header"`
		Body   json.RawMessage   `json:"body"`
		SSE    []string          `json:"sse"`
	} `json:"response"`
}

// loadFixtures reads every testdata exchange, sorted by name.
func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	var out []fixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var f fixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		if f.Name == "" {
			f.Name = strings.TrimSuffix(e.Name(), ".json")
		}
		f.File = e.Name()
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found in testdata")
	}
	return out
}

// status is the recorded upstream status code.
func (f *fixture) status() int {
	n, _ := strconv.Atoi(f.Response.Header[":status"])
	return n
}

// requestStream reports whether the client asked for streaming.
func (f *fixture) requestStream() bool {
	var r struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(f.Request.Body, &r)
	return r.Stream
}

// requestModel is the model name the client asked for.
func (f *fixture) requestModel() string {
	var r struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(f.Request.Body, &r)
	return r.Model
}

// upstreamStream reports whether the recorded upstream response was SSE.
func (f *fixture) upstreamStream() bool {
	return strings.Contains(f.Response.Header["Content-Type"], "event-stream") ||
		(len(f.Response.SSE) > 0 && len(f.Response.Body) == 0)
}

// upstreamBodyBytes returns the raw recorded upstream body: the JSON value in
// Body (an object for JSON bodies, an unquoted string for non-JSON ones).
func (f *fixture) upstreamBodyBytes(t *testing.T) []byte {
	t.Helper()
	if len(f.Response.Body) == 0 {
		return nil
	}
	if f.Response.Body[0] == '"' {
		var s string
		if err := json.Unmarshal(f.Response.Body, &s); err != nil {
			t.Fatalf("%s: upstream body string: %v", f.Name, err)
		}
		return []byte(s)
	}
	return f.Response.Body
}

// ---------- independent upstream readers ----------

// upTool is one tool call accumulated from upstream data.
type upTool struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Args  string `json:"args"`
}

// upDigest is the logical content of an upstream response, assembled without
// using any translator code.
type upDigest struct {
	Text     string
	Thinking string
	Tools    []upTool
	Finish   string
	Usage    struct {
		Prompt       int
		Completion   int
		CachedTokens int
		HasUsage     bool
	}
}

// readUpstreamStream assembles the digest of a recorded upstream SSE stream.
func readUpstreamStream(t *testing.T, payloads []string) upDigest {
	t.Helper()
	var d upDigest
	byIndex := map[int]int{}
	addTool := func(index int) *upTool {
		if i, ok := byIndex[index]; ok {
			return &d.Tools[i]
		}
		d.Tools = append(d.Tools, upTool{Index: index})
		byIndex[index] = len(d.Tools) - 1
		return &d.Tools[len(d.Tools)-1]
	}
	for _, p := range payloads {
		if p == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(p), &ch); err != nil {
			continue // unparseable chunks are dropped by the translator too
		}
		if ch.Usage != nil {
			d.Usage.Prompt = ch.Usage.PromptTokens
			d.Usage.Completion = ch.Usage.CompletionTokens
			d.Usage.HasUsage = true
			if ch.Usage.PromptTokensDetails != nil {
				d.Usage.CachedTokens = ch.Usage.PromptTokensDetails.CachedTokens
			}
		}
		for _, c := range ch.Choices {
			d.Text += c.Delta.Content
			d.Thinking += c.Delta.ReasoningContent
			for _, tc := range c.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				tool := addTool(idx)
				if tc.ID != "" && tool.ID == "" {
					tool.ID = tc.ID
				}
				if tc.Function.Name != "" && tool.Name == "" {
					tool.Name = tc.Function.Name
				}
				tool.Args += tc.Function.Arguments
			}
			if c.FinishReason != nil {
				d.Finish = *c.FinishReason
			}
		}
	}
	// A tool call that never carried a name is unusable and dropped.
	named := d.Tools[:0]
	for _, tt := range d.Tools {
		if tt.Name != "" {
			named = append(named, tt)
		}
	}
	d.Tools = named
	sort.Slice(d.Tools, func(i, j int) bool { return d.Tools[i].Index < d.Tools[j].Index })
	return d
}

// readUpstreamBody assembles the digest of a recorded buffered upstream
// response (a chat.completion object, or an error body).
func readUpstreamBody(t *testing.T, raw []byte) upDigest {
	t.Helper()
	var d upDigest
	var body struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: upstream body: %v", t.Name(), err)
	}
	if len(body.Choices) > 0 {
		c := body.Choices[0]
		if len(c.Message.Content) > 0 && c.Message.Content[0] == '"' {
			var s string
			_ = json.Unmarshal(c.Message.Content, &s)
			d.Text = s
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(c.Message.Content, &parts); err == nil {
				for _, p := range parts {
					if p.Type == "text" {
						d.Text += p.Text
					}
				}
			}
		}
		d.Thinking = c.Message.ReasoningContent
		for i, tc := range c.Message.ToolCalls {
			d.Tools = append(d.Tools, upTool{Index: i, ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if c.FinishReason != nil {
			d.Finish = *c.FinishReason
		}
	}
	if body.Usage != nil {
		d.Usage.Prompt = body.Usage.PromptTokens
		d.Usage.Completion = body.Usage.CompletionTokens
		d.Usage.HasUsage = true
		if body.Usage.PromptTokensDetails != nil {
			d.Usage.CachedTokens = body.Usage.PromptTokensDetails.CachedTokens
		}
	}
	return d
}

// expectedStopReason maps an upstream finish reason to Claude's stop_reason —
// content wins when tool calls are present.
func expectedStopReason(d upDigest) string {
	if len(d.Tools) > 0 {
		return "tool_use"
	}
	switch d.Finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// toolInputExpected applies the same validity rule as the wire contract: a
// tool-call arguments string is passed through when it is valid JSON, else
// collapsed to {}.
func toolInputExpected(args string) string {
	if args == "" {
		return "{}"
	}
	var check any
	if err := json.Unmarshal([]byte(args), &check); err != nil {
		return "{}"
	}
	return args
}

// ---------- request translation ----------

// TestFixtureRequestTranslation runs every fixture's client request through
// TranslateRequest and checks the OpenAI request shape against the input.
func TestFixtureRequestTranslation(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			in := &caddyllm.LazyJsonNode{Val: fx.Request.Body}
			outNode, err := TranslateRequest(in)
			if err != nil {
				t.Fatalf("TranslateRequest: %v", err)
			}
			out, err := outNode.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("translated request unparseable: %v (%s)", err, out)
			}

			// Only chat-completions fields may appear.
			allowed := map[string]bool{
				"model": true, "max_tokens": true, "stream": true,
				"temperature": true, "top_p": true, "stop": true,
				"reasoning_effort": true, "messages": true, "tools": true,
				"tool_choice": true, "stream_options": true,
			}
			for k := range got {
				if !allowed[k] {
					t.Errorf("unexpected field %q in translated request", k)
				}
			}

			var want struct {
				Model     string           `json:"model"`
				MaxTokens int              `json:"max_tokens"`
				Stream    bool             `json:"stream"`
				Messages  []map[string]any `json:"messages"`
				Tools     []struct {
					Name string `json:"name"`
				} `json:"tools"`
				ToolChoice *struct {
					Type string `json:"type"`
				} `json:"tool_choice"`
				System      json.RawMessage `json:"system"`
				Temperature *float64        `json:"temperature"`
				TopP        *float64        `json:"top_p"`
			}
			if err := json.Unmarshal(fx.Request.Body, &want); err != nil {
				t.Fatalf("fixture request: %v", err)
			}

			if want.Model != "" && got["model"] != want.Model {
				t.Errorf("model = %v, want %v", got["model"], want.Model)
			}
			if got["stream"] != want.Stream {
				t.Errorf("stream = %v, want %v", got["stream"], want.Stream)
			}
			// stream_options rides along exactly for streaming requests.
			if want.Stream {
				so, ok := got["stream_options"].(map[string]any)
				if !ok || so["include_usage"] != true {
					t.Errorf("stream_options = %v, want include_usage:true", got["stream_options"])
				}
			} else if _, ok := got["stream_options"]; ok {
				t.Error("stream_options present on a non-streaming request")
			}

			// Tools pass through one-to-one, in order.
			if gotTools, ok := got["tools"].([]any); ok {
				if len(gotTools) != len(want.Tools) {
					t.Errorf("tools: translated %d, want %d", len(gotTools), len(want.Tools))
				} else {
					for i, gt := range gotTools {
						fn := gt.(map[string]any)["function"].(map[string]any)
						if fn["name"] != want.Tools[i].Name {
							t.Errorf("tools[%d].name = %v, want %v", i, fn["name"], want.Tools[i].Name)
						}
						if _, ok := fn["parameters"]; !ok {
							t.Errorf("tools[%d].parameters missing", i)
						}
					}
				}
			} else if len(want.Tools) > 0 {
				t.Errorf("tools dropped: %d in, none out", len(want.Tools))
			}

			// tool_choice mapping.
			if want.ToolChoice != nil {
				switch want.ToolChoice.Type {
				case "any":
					if got["tool_choice"] != "required" {
						t.Errorf("tool_choice = %v, want required", got["tool_choice"])
					}
				case "tool":
					tc, ok := got["tool_choice"].(map[string]any)
					if !ok || tc["type"] != "function" {
						t.Errorf("tool_choice = %v, want function object", got["tool_choice"])
					}
				default:
					if got["tool_choice"] != "auto" {
						t.Errorf("tool_choice = %v, want auto", got["tool_choice"])
					}
				}
			}

			// The system prompt becomes the first message when present
			// (whitespace-only text carries nothing).
			hasSystem := false
			switch string(want.System) {
			case "", "null", "[]", `""`:
			default:
				if s, err := strconv.Unquote(string(want.System)); err == nil {
					hasSystem = strings.TrimSpace(s) != ""
				} else {
					hasSystem = true
				}
			}
			msgs, _ := got["messages"].([]any)
			if len(msgs) == 0 {
				t.Fatal("no messages in translated request")
			}
			first := msgs[0].(map[string]any)
			if hasSystem && first["role"] != "system" {
				t.Errorf("first message role = %v, want system", first["role"])
			}
			if !hasSystem && first["role"] == "system" {
				t.Error("system message fabricated without a system prompt")
			}

			// Message-level invariants: OpenAI roles only, tool messages only
			// after the assistant tool_calls they answer, OpenAI part shapes.
			sawToolCalls := false
			msgAllowed := map[string]bool{
				"role": true, "content": true, "reasoning_content": true,
				"tool_calls": true, "tool_call_id": true, "name": true,
			}
			for i, m := range msgs {
				m := m.(map[string]any)
				for k := range m {
					if !msgAllowed[k] {
						t.Errorf("messages[%d]: unexpected key %q", i, k)
					}
				}
				switch m["role"] {
				case "system", "user", "assistant":
				case "tool":
					if !sawToolCalls {
						t.Errorf("messages[%d]: role tool before any assistant tool_calls", i)
					}
				default:
					t.Errorf("messages[%d]: role %v", i, m["role"])
				}
				if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
					sawToolCalls = true
					for _, tc := range tcs {
						fn := tc.(map[string]any)["function"].(map[string]any)
						if fn["name"] == "" {
							t.Errorf("messages[%d]: tool_call without name", i)
						}
						var args any
						if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
							t.Errorf("messages[%d]: tool_call arguments not JSON: %v", i, err)
						}
					}
				}
				if parts, ok := m["content"].([]any); ok {
					for _, p := range parts {
						switch p.(map[string]any)["type"] {
						case "text", "image_url":
						default:
							t.Errorf("messages[%d]: content part type %v", i, p.(map[string]any)["type"])
						}
					}
				}
			}
		})
	}
}

// ---------- handler replay ----------

// observingRecorder wraps a ResponseRecorder with ObserveStatus, mimicking
// llm_route's peekWriter: buffered (fallthrough-candidate) statuses are
// observed without being committed.
type observingRecorder struct {
	*httptest.ResponseRecorder
	observed []int
}

func (o *observingRecorder) ObserveStatus(code int) {
	o.observed = append(o.observed, code)
}

// replayFixture drives one fixture through the real handler: the client
// request goes in, a fake upstream replays the recorded response, and the
// client-facing result is returned.
func replayFixture(t *testing.T, fx fixture) *observingRecorder {
	t.Helper()
	h := &Claude2OpenAI{}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(fx.Request.Body))
	for k, v := range fx.Request.Header {
		r.Header.Set(k, v)
	}
	rec := &observingRecorder{ResponseRecorder: httptest.NewRecorder()}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		for k, v := range fx.Response.Header {
			if k == ":status" {
				continue
			}
			w.Header().Set(k, v)
		}
		status := fx.status()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if fx.upstreamStream() {
			fl := w.(http.Flusher)
			for _, p := range fx.Response.SSE {
				if _, err := io.WriteString(w, "data: "+p+"\n\n"); err != nil {
					return err
				}
				fl.Flush()
			}
			return nil
		}
		_, err := w.Write(fx.upstreamBodyBytes(t))
		return err
	})
	if err := h.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return rec
}

// TestFixtureHandlerReplay replays every fixture end to end and checks the
// client-facing response against an independent reading of the upstream data.
func TestFixtureHandlerReplay(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			rec := replayFixture(t, fx)
			body := rec.Body.String()

			if fx.status() >= 400 {
				checkErrorReplay(t, fx, rec)
				return
			}
			if fx.upstreamStream() || json.Valid(fx.upstreamBodyBytes(t)) {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body: %s", rec.Code, truncStr(body, 400))
				}
			}
			if fx.upstreamStream() {
				checkStreamReplay(t, fx, body)
			} else if json.Valid(fx.upstreamBodyBytes(t)) {
				checkBufferedReplay(t, fx, body)
			} else {
				// A 2xx body that is not JSON cannot be translated: 502.
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, truncStr(body, 200))
				}
				wantType, wantMsg := expectedError(http.StatusBadGateway, fx.upstreamBodyBytes(t))
				var out struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("client body: %v (%s)", err, body)
				}
				if out.Error.Type != wantType || out.Error.Message != wantMsg {
					t.Errorf("error = %q/%q, want %q/%q", out.Error.Type, out.Error.Message, wantType, wantMsg)
				}
			}
		})
	}
}

// checkStreamReplay verifies a translated SSE stream: event structure plus
// logical content against the upstream digest.
func checkStreamReplay(t *testing.T, fx fixture, body string) {
	t.Helper()
	events := parseSSEBody(body)
	if len(events) == 0 {
		t.Fatalf("no SSE events in client response: %s", truncStr(body, 400))
	}
	if events[0].Name != "message_start" {
		t.Errorf("first event = %q, want message_start", events[0].Name)
	}
	if events[len(events)-1].Name != "message_stop" {
		t.Errorf("last event = %q, want message_stop", events[len(events)-1].Name)
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("[DONE] leaked into the client stream")
	}

	// Structural: every opened block closes, with matching indexes.
	type block struct {
		kind  string
		index int
	}
	open := map[int]string{}     // block index -> block type
	closed := map[int]bool{}     // block index -> stopped
	openTool := map[int]string{} // block index -> tool "name\x00id" key
	text, thinking, model, stopReason := "", "", "", ""
	tools := map[string]*strings.Builder{}
	var toolMeta []string // "name\x00id" in start order
	usage := map[string]float64{}
	for _, e := range events {
		var v struct {
			Index int `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				StopReason  string `json:"stop_reason"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			Usage map[string]float64 `json:"usage"`
		}
		if err := json.Unmarshal([]byte(e.Data), &v); err != nil {
			t.Fatalf("event %s payload: %v (%s)", e.Name, err, e.Data)
		}
		switch e.Name {
		case "message_start":
			model = v.Message.Model
		case "content_block_start":
			if _, dup := open[v.Index]; dup {
				t.Errorf("block index %d opened twice", v.Index)
			}
			open[v.Index] = v.Block.Type
			if v.Block.Type == "tool_use" {
				key := v.Block.Name + "\x00" + v.Block.ID
				openTool[v.Index] = key
				if _, dup := tools[key]; !dup {
					tools[key] = &strings.Builder{}
					toolMeta = append(toolMeta, key)
				}
			}
		case "content_block_stop":
			if _, isOpen := open[v.Index]; !isOpen {
				t.Errorf("block index %d stopped while not open", v.Index)
			}
			closed[v.Index] = true
		case "content_block_delta":
			switch v.Delta.Type {
			case "text_delta":
				text += v.Delta.Text
			case "thinking_delta":
				thinking += v.Delta.Thinking
			case "input_json_delta":
				key := openTool[v.Index]
				if key == "" {
					t.Errorf("input_json_delta for non-tool block %d", v.Index)
				} else {
					tools[key].WriteString(v.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if v.Delta.StopReason != "" {
				stopReason = v.Delta.StopReason
			}
			for k, n := range v.Usage {
				usage[k] = n
			}
		}
	}
	for idx := range open {
		if !closed[idx] {
			t.Errorf("block index %d (%s) never stopped", idx, open[idx])
		}
	}

	want := readUpstreamStream(t, fx.Response.SSE)
	if text != want.Text {
		t.Errorf("text:\n want %.200q\n  got %.200q", want.Text, text)
	}
	if thinking != want.Thinking {
		t.Errorf("thinking:\n want %.200q\n  got %.200q", want.Thinking, thinking)
	}
	// Compare tools by name: block-start order follows each tool's name
	// arrival, which need not match the index order arguments flush in.
	sortedKeys := append([]string(nil), toolMeta...)
	sort.Slice(sortedKeys, func(i, j int) bool {
		n1, i1, _ := strings.Cut(sortedKeys[i], "\x00")
		n2, i2, _ := strings.Cut(sortedKeys[j], "\x00")
		if n1 != n2 {
			return n1 < n2
		}
		return i1 < i2
	})
	sortedWant := append([]upTool(nil), want.Tools...)
	sort.Slice(sortedWant, func(i, j int) bool {
		if sortedWant[i].Name != sortedWant[j].Name {
			return sortedWant[i].Name < sortedWant[j].Name
		}
		return sortedWant[i].ID < sortedWant[j].ID
	})
	if len(sortedKeys) != len(sortedWant) {
		t.Errorf("tool count = %d, want %d", len(sortedKeys), len(sortedWant))
	} else {
		for i, key := range sortedKeys {
			name, id, _ := strings.Cut(key, "\x00")
			if name != sortedWant[i].Name || (id != sortedWant[i].ID && sortedWant[i].ID != "") {
				t.Errorf("tool[%d] = %q/%q, want %q/%q", i, name, id, sortedWant[i].Name, sortedWant[i].ID)
			}
			if gotArgs, wantArgs := tools[key].String(), toolInputExpected(sortedWant[i].Args); !jsonEq(gotArgs, wantArgs) {
				t.Errorf("tool[%d] input = %s, want %s", i, gotArgs, wantArgs)
			}
		}
	}
	if stopReason != expectedStopReason(want) {
		t.Errorf("stop_reason = %q, want %q", stopReason, expectedStopReason(want))
	}
	if m := fx.requestModel(); m != "" && model != m {
		t.Errorf("message_start model = %q, want %q", model, m)
	}
	if want.Usage.HasUsage {
		if got := usage["input_tokens"]; got != float64(want.Usage.Prompt-want.Usage.CachedTokens) {
			t.Errorf("usage.input_tokens = %v, want %d", got, want.Usage.Prompt-want.Usage.CachedTokens)
		}
		if got := usage["output_tokens"]; got != float64(want.Usage.Completion) {
			t.Errorf("usage.output_tokens = %v, want %d", got, want.Usage.Completion)
		}
	}
}

// checkBufferedReplay verifies a translated non-streaming response.
func checkBufferedReplay(t *testing.T, fx fixture, body string) {
	t.Helper()
	var out struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Input    json.RawMessage `json:"input"`
			Name     string          `json:"name"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("client body: %v (%s)", err, truncStr(body, 400))
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("type/role = %q/%q", out.Type, out.Role)
	}
	if m := fx.requestModel(); m != "" && out.Model != m {
		t.Errorf("model = %q, want %q", out.Model, m)
	}

	want := readUpstreamBody(t, fx.upstreamBodyBytes(t))

	// Block sequence: thinking, then text, then tool_use blocks.
	var gotText, gotThinking string
	var gotTools []string
	for _, b := range out.Content {
		switch b.Type {
		case "thinking":
			if gotThinking != "" || gotText != "" || len(gotTools) > 0 {
				t.Error("thinking block after other content")
			}
			gotThinking = b.Thinking
		case "text":
			if len(gotTools) > 0 {
				t.Error("text block after tool_use")
			}
			gotText += b.Text
		case "tool_use":
			gotTools = append(gotTools, b.Name)
		default:
			t.Errorf("unexpected block type %q", b.Type)
		}
	}
	if gotText != want.Text {
		t.Errorf("text:\n want %.200q\n  got %.200q", want.Text, gotText)
	}
	if gotThinking != want.Thinking {
		t.Errorf("thinking:\n want %.200q\n  got %.200q", want.Thinking, gotThinking)
	}
	if len(gotTools) != len(want.Tools) {
		t.Errorf("tool count = %d, want %d", len(gotTools), len(want.Tools))
	} else {
		toolIdx := 0
		for _, b := range out.Content {
			if b.Type != "tool_use" {
				continue
			}
			w := want.Tools[toolIdx]
			if b.Name != w.Name {
				t.Errorf("tool[%d].name = %q, want %q", toolIdx, b.Name, w.Name)
			}
			if !jsonEq(string(b.Input), toolInputExpected(w.Args)) {
				t.Errorf("tool[%d].input = %s, want %s", toolIdx, b.Input, toolInputExpected(w.Args))
			}
			toolIdx++
		}
	}
	if out.StopReason != expectedStopReason(want) {
		t.Errorf("stop_reason = %q, want %q", out.StopReason, expectedStopReason(want))
	}
	if want.Usage.HasUsage {
		if in := want.Usage.Prompt - want.Usage.CachedTokens; out.Usage.InputTokens != in {
			t.Errorf("usage.input_tokens = %d, want %d", out.Usage.InputTokens, in)
		}
		if out.Usage.CacheReadInputTokens != want.Usage.CachedTokens {
			t.Errorf("usage.cache_read_input_tokens = %d, want %d", out.Usage.CacheReadInputTokens, want.Usage.CachedTokens)
		}
		if out.Usage.OutputTokens != want.Usage.Completion {
			t.Errorf("usage.output_tokens = %d, want %d", out.Usage.OutputTokens, want.Usage.Completion)
		}
	}
}

// checkErrorReplay verifies an upstream error translated to a Claude error.
//
// For non-streaming requests the handler writes the translated error itself.
// For streaming requests the handler deliberately commits NOTHING (not even
// the status): inside llm_route the error status travels via ObserveStatus so
// the route can fall through to a fallback with the pristine request. The
// replay mimics that contract: the status must be observed, no bytes may
// reach the client, and the error envelope the final fallback would emit
// (TranslateError) must match expectations.
func checkErrorReplay(t *testing.T, fx fixture, rec *observingRecorder) {
	t.Helper()
	wantType, wantMsg := expectedError(fx.status(), fx.upstreamBodyBytes(t))
	assertErrEnvelope := func(body []byte) {
		var out struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("client body: %v (%s)", err, body)
		}
		if out.Type != "error" {
			t.Errorf("type = %q, want error", out.Type)
		}
		if out.Error.Type != wantType {
			t.Errorf("error.type = %q, want %q", out.Error.Type, wantType)
		}
		if out.Error.Message != wantMsg {
			t.Errorf("error.message = %q, want %q", out.Error.Message, wantMsg)
		}
	}

	if !fx.requestStream() {
		if rec.Code != fx.status() {
			t.Fatalf("status = %d, want %d (body: %s)", rec.Code, fx.status(), truncStr(rec.Body.String(), 400))
		}
		assertErrEnvelope(rec.Body.Bytes())
		return
	}

	// Streaming: nothing is committed; the observed status drives fallthrough.
	if len(rec.observed) == 0 || rec.observed[len(rec.observed)-1] != fx.status() {
		t.Errorf("observed statuses = %v, want last %d", rec.observed, fx.status())
	}
	if rec.Body.Len() > 0 {
		t.Errorf("bytes committed on a fallthrough error: %s", truncStr(rec.Body.String(), 200))
	}
	got := TranslateError(fx.status(), fx.upstreamBodyBytes(t))
	assertErrEnvelope(got)
}

// expectedError independently derives the Claude error type/message from an
// upstream status + body (the CLIProxyAPI-derived semantics).
func expectedError(status int, body []byte) (string, string) {
	typ := "invalid_request_error"
	switch {
	case status == 401:
		typ = "authentication_error"
	case status == 402:
		typ = "billing_error"
	case status == 403:
		typ = "permission_error"
	case status == 404:
		typ = "not_found_error"
	case status == 413:
		typ = "request_too_large"
	case status == 429:
		typ = "rate_limit_error"
	case status == 504:
		typ = "timeout_error"
	case status == 529:
		typ = "overloaded_error"
	case status >= 500:
		typ = "api_error"
	}
	msg := string(body)
	if msg == "" {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "upstream error"
		}
	}
	var parsed struct {
		Error *struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		e := parsed.Error
		if e == nil && parsed.Type != "" {
			e = &struct {
				Type    string          `json:"type"`
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
			}{Type: parsed.Type, Message: parsed.Message}
		}
		if e != nil {
			if t := strings.TrimSpace(e.Type); t != "" && t != "error" {
				typ = t
			}
			if m := strings.TrimSpace(e.Message); m != "" {
				msg = m
			} else if len(e.Code) > 0 && e.Code[0] == '"' {
				var code string
				_ = json.Unmarshal(e.Code, &code)
				if strings.TrimSpace(code) != "" {
					msg = strings.TrimSpace(code)
				}
			}
		}
	}
	return typ, msg
}

// ---------- sse helpers ----------

// parseSSEBody splits a client-facing SSE body into events.
func parseSSEBody(body string) []sseEventOut {
	var events []sseEventOut
	var cur sseEventOut
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
			events = append(events, cur)
			cur = sseEventOut{}
		}
	}
	return events
}

type sseEventOut struct {
	Name string
	Data string
}

// jsonEq compares two JSON strings semantically.
func jsonEq(a, b string) bool {
	var av, bv any
	if json.Unmarshal([]byte(a), &av) != nil || json.Unmarshal([]byte(b), &bv) != nil {
		return a == b
	}
	return jsonDeepEqT(av, bv)
}

func jsonDeepEqT(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, ok := bv[k]
			if !ok || !jsonDeepEqT(v, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonDeepEqT(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// TestFixturesSanitized guards the committed fixtures: redacted material from
// the source corpus must never leak into the repository.
func TestFixturesSanitized(t *testing.T) {
	// Literal markers that must never survive sanitization. Home-directory
	// bans require a user segment (a bare "/home/" also matches repo-relative
	// paths like "modules/home/" in pasted documentation).
	banned := []string{
		"sankuai", "bigmodel", "acw_tc", "meituan", "zhipu",
		"device_id", "account_uuid",
	}
	bannedRe := []*regexp.Regexp{
		regexp.MustCompile(`(?i)ccat3z`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`/(?:home|Users)/[A-Za-z0-9_.-]+/`),
	}
	for _, fx := range loadFixtures(t) {
		t.Run(fx.File, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", fx.File))
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for _, p := range banned {
				if strings.Contains(s, p) {
					t.Errorf("sanitization leak: %q present", p)
				}
			}
			for _, re := range bannedRe {
				if re.MatchString(s) {
					t.Errorf("sanitization leak: %q present", re.String())
				}
			}
		})
	}
}

// TestParseCaddyfileDirective: the claude2openai directive takes no arguments.
func TestParseCaddyfileDirective(t *testing.T) {
	h := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser("claude2openai")}
	mh, err := parseCaddyfile(h)
	if err != nil {
		t.Fatalf("parseCaddyfile: %v", err)
	}
	if _, ok := mh.(*Claude2OpenAI); !ok {
		t.Fatalf("wrong module type %T", mh)
	}

	h = httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser("claude2openai /")}
	if _, err := parseCaddyfile(h); err == nil {
		t.Error("positional argument should be rejected")
	}
}

// TestStreamFeederError: the feeder's error path emits a Claude error event.
func TestStreamFeederError(t *testing.T) {
	f := NewStreamFeeder("m")
	evts := f.Error(429, []byte(`{"error":{"message":"slow"}}`))
	if len(evts) != 1 || evts[0].Name != "error" {
		t.Fatalf("events = %+v", evts)
	}
	if !strings.Contains(string(evts[0].Data), "slow") {
		t.Errorf("error event data = %s", evts[0].Data)
	}
	// Done is idempotent: a second close yields nothing.
	if _, err := f.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if evts, err := f.Close(); err != nil || len(evts) != 0 {
		t.Errorf("second Close = %v, %v", evts, err)
	}
}

// TestTranslateRequestErrors covers the malformed-input branches the recorded
// corpus never exercises (clients send well-formed requests).
func TestTranslateRequestErrors(t *testing.T) {
	node := func(s string) *caddyllm.LazyJsonNode {
		return &caddyllm.LazyJsonNode{Val: json.RawMessage(s)}
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not an object", `[]`, "not a JSON object"},
		{"messages object", `{"messages":{"role":"user"}}`, "messages is not an array"},
		{"stop non-string", `{"stop_sequences":[1, 2]}`, "stop_sequences"},
		{"tool without name", `{"tools":[{"input_schema":{}}]}`, "tool missing name"},
		{"tool_choice not object", `{"tool_choice":"auto"}`, "tool_choice is not an object"},
		{"image without source", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`, "missing source"},
		{"unsupported source type", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file"}}]}]}`, "unsupported source type"},
		{"tool_result bad content", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":5}]}]}`, "parse content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := TranslateRequest(node(c.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want substring %q", err, c.want)
			}
		})
	}
}

// TestHandlerEdgePaths covers handler routes the recorded corpus cannot
// produce: non-POST pass-through, malformed bodies, chain errors, and
// upstreams that skip WriteHeader or answer gzip.
func TestHandlerEdgePaths(t *testing.T) {
	h := &Claude2OpenAI{}
	post := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	claudeReq := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	chatJSON := `{"id":"e1","object":"chat.completion","model":"u","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	t.Run("non-post passthrough", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusNoContent)
			return nil
		})
		if err := h.ServeHTTP(rec, r, next); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			t.Error("next reached on a malformed body")
			return nil
		})
		if err := h.ServeHTTP(rec, post("{not json"), next); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request_error") {
			t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("chain error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			return errors.New("boom")
		})
		if err := h.ServeHTTP(rec, post(claudeReq), next); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Error.Type != "api_error" || out.Error.Message != "boom" {
			t.Errorf("error = %+v", out.Error)
		}
	})

	t.Run("no writeheader upstream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			_, err := w.Write([]byte(chatJSON))
			return err
		})
		if err := h.ServeHTTP(rec, post(claudeReq), next); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) {
			t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("gzip buffered upstream", func(t *testing.T) {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write([]byte(chatJSON))
		zw.Close()
		rec := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			_, err := w.Write(gz.Bytes())
			return err
		})
		if err := h.ServeHTTP(rec, post(claudeReq), next); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) {
			t.Errorf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Encoding") != "" {
			t.Error("Content-Encoding not stripped after decompression")
		}
	})

	t.Run("client write failure mid-stream", func(t *testing.T) {
		sse := "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"x\"}}]}\n\n"
		fail := &failingWriter{ResponseRecorder: httptest.NewRecorder()}
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusOK)
			_, err := io.WriteString(w, sse)
			return err
		})
		if err := h.ServeHTTP(fail, post(`{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), next); err != nil {
			t.Fatalf("ServeHTTP: %v", err)
		}
	})
}

// failingWriter makes the translated client stream fail on write.
type failingWriter struct {
	*httptest.ResponseRecorder
	failAfter int
	n         int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n > 1 {
		return 0, errors.New("client went away")
	}
	return f.ResponseRecorder.Write(p)
}

func (f *failingWriter) Flush() {}
