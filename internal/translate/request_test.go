package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestTranslateRequestBasics(t *testing.T) {
	temp := 0.7
	in := &AnthropicRequest{
		Model:     "m",
		MaxTokens: 100,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		System:      json.RawMessage(`"be brief"`),
		Temperature: &temp,
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.Model != "m" || out.MaxTokens != 100 {
		t.Errorf("model/max_tokens = %q/%d", out.Model, out.MaxTokens)
	}
	if out.Temperature == nil || *out.Temperature != 0.7 {
		t.Error("temperature not mapped")
	}
	if out.TopP != nil {
		t.Error("top_p should be unset when temperature present")
	}
	// system + user
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %s", mustJSON(t, out.Messages))
	}
	if out.Messages[0].Role != "system" || string(out.Messages[0].Content) != `[{"type":"text","text":"be brief"}]` {
		t.Errorf("system message = %s", mustJSON(t, out.Messages[0]))
	}
	if out.Messages[1].Role != "user" || string(out.Messages[1].Content) != `"hello"` {
		t.Errorf("user message = %s", mustJSON(t, out.Messages[1]))
	}
	if out.StreamOptions != nil {
		t.Error("stream_options should be nil for non-streaming")
	}
}

func TestTranslateRequestTemperatureTopPMutuallyExclusive(t *testing.T) {
	temp, topP := 1.0, 0.5
	in := &AnthropicRequest{
		Messages:    []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
		Temperature: &temp,
		TopP:        &topP,
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if out.Temperature == nil || out.TopP != nil {
		t.Error("temperature should win over top_p")
	}
}

func TestTranslateRequestStopSequences(t *testing.T) {
	in := &AnthropicRequest{
		Messages:      []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
		StopSequences: []string{"a"},
	}
	out, _ := TranslateRequest(in)
	if string(out.Stop) != `"a"` {
		t.Errorf("single stop = %s", out.Stop)
	}
	in.StopSequences = []string{"a", "b"}
	out, _ = TranslateRequest(in)
	if string(out.Stop) != `["a","b"]` {
		t.Errorf("multi stop = %s", out.Stop)
	}
}

func TestTranslateRequestReasoningEffort(t *testing.T) {
	cases := []struct {
		thinking *AnthropicThinking
		want     string
	}{
		{nil, ""},
		{&AnthropicThinking{Type: "enabled", BudgetTokens: 512}, "minimal"},
		{&AnthropicThinking{Type: "enabled", BudgetTokens: 1024}, "low"},
		{&AnthropicThinking{Type: "enabled", BudgetTokens: 8192}, "medium"},
		{&AnthropicThinking{Type: "enabled", BudgetTokens: 24576}, "high"},
		{&AnthropicThinking{Type: "enabled", BudgetTokens: 99999}, "xhigh"},
		{&AnthropicThinking{Type: "adaptive"}, "xhigh"},
		{&AnthropicThinking{Type: "disabled"}, "none"},
	}
	for _, c := range cases {
		in := &AnthropicRequest{
			Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
			Thinking: c.thinking,
		}
		out, err := TranslateRequest(in)
		if err != nil {
			t.Fatalf("TranslateRequest(%+v): %v", c.thinking, err)
		}
		if out.ReasoningEffort != c.want {
			t.Errorf("thinking %+v => effort %q, want %q", c.thinking, out.ReasoningEffort, c.want)
		}
	}
}

func TestTranslateRequestToolsAndChoice(t *testing.T) {
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
		Tools: []AnthropicTool{
			{Name: "get_weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		},
		ToolChoice: &AnthropicToolChoice{Type: "any"},
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %s", mustJSON(t, out.Tools))
	}
	var params map[string]any
	if err := json.Unmarshal(out.Tools[0].Function.Parameters, &params); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	if params["type"] != "object" {
		t.Errorf("parameters = %s", out.Tools[0].Function.Parameters)
	}
	if string(out.ToolChoice) != `"required"` {
		t.Errorf("any => %s, want \"required\"", out.ToolChoice)
	}
	in.ToolChoice = &AnthropicToolChoice{Type: "tool", Name: "get_weather"}
	out, _ = TranslateRequest(in)
	var tc map[string]any
	if err := json.Unmarshal(out.ToolChoice, &tc); err != nil {
		t.Fatalf("tool_choice: %v", err)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" || tc["type"] != "function" {
		t.Errorf("tool => %s", out.ToolChoice)
	}
	in.ToolChoice = &AnthropicToolChoice{Type: "weird"}
	out, _ = TranslateRequest(in)
	if string(out.ToolChoice) != `"auto"` {
		t.Errorf("unknown => %s, want \"auto\"", out.ToolChoice)
	}
}

func TestTranslateRequestToolRoundTrip(t *testing.T) {
	// assistant tool_use followed by user tool_result — the OpenAI shape must
	// be assistant(tool_calls) then tool messages, then the user's own text.
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"what's the weather?"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"Let me check."},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"72F and sunny"},
				{"type":"text","text":"thanks!"}
			]`)},
		},
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Messages) != 4 {
		t.Fatalf("messages = %s", mustJSON(t, out.Messages))
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("msg[1] role = %s", asst.Role)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "toolu_1" ||
		asst.ToolCalls[0].Function.Name != "get_weather" ||
		asst.ToolCalls[0].Function.Arguments != `{"city":"SF"}` {
		t.Errorf("assistant tool_calls = %s", mustJSON(t, asst.ToolCalls))
	}
	// Assistant text and tool_calls share one message.
	if string(asst.Content) != `[{"type":"text","text":"Let me check."}]` {
		t.Errorf("assistant content = %s", asst.Content)
	}
	toolMsg := out.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "toolu_1" || string(toolMsg.Content) != `"72F and sunny"` {
		t.Errorf("tool message = %s", mustJSON(t, toolMsg))
	}
	userMsg := out.Messages[3]
	if userMsg.Role != "user" || string(userMsg.Content) != `[{"type":"text","text":"thanks!"}]` {
		t.Errorf("user message = %s", mustJSON(t, userMsg))
	}
}

func TestTranslateRequestToolResultWithImages(t *testing.T) {
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"t1","content":[
					{"type":"text","text":"screenshot"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}
				]}
			]`)},
		},
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %s", mustJSON(t, out.Messages))
	}
	var parts []map[string]any
	if err := json.Unmarshal(out.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content: %v (%s)", err, out.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %s", out.Messages[0].Content)
	}
	if parts[0]["type"] != "text" || parts[0]["text"] != "screenshot" {
		t.Errorf("part0 = %v", parts[0])
	}
	iu := parts[1]["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,aGk=" {
		t.Errorf("image_url = %v", iu)
	}
}

func TestTranslateRequestThinkingBlocks(t *testing.T) {
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"thinking","thinking":"hmm","signature":"sig"},
				{"type":"thinking","thinking":"more","signature":""},
				{"type":"redacted_thinking","data":"opaque"},
				{"type":"text","text":"answer"}
			]`)},
		},
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %s", mustJSON(t, out.Messages))
	}
	if out.Messages[0].Reasoning != "hmm" {
		t.Errorf("reasoning = %q, want only the signed thinking", out.Messages[0].Reasoning)
	}
}

func TestTranslateRequestImageBlocks(t *testing.T) {
	for _, src := range []string{
		`{"type":"url","url":"https://x/img.png"}`,
		`{"type":"base64","media_type":"image/jpeg","data":"aGk="}`,
		`{"type":"base64","data":"aGk="}`,
	} {
		in := &AnthropicRequest{
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"image","source":` + src + `}]`)},
			},
		}
		out, err := TranslateRequest(in)
		if err != nil {
			t.Fatalf("TranslateRequest(%s): %v", src, err)
		}
		var parts []map[string]any
		if err := json.Unmarshal(out.Messages[0].Content, &parts); err != nil {
			t.Fatalf("content: %v", err)
		}
		url := parts[0]["image_url"].(map[string]any)["url"].(string)
		switch {
		case strings.HasPrefix(src, `{"type":"url"`):
			if url != "https://x/img.png" {
				t.Errorf("url = %q", url)
			}
		case strings.Contains(src, "image/jpeg"):
			if url != "data:image/jpeg;base64,aGk=" {
				t.Errorf("url = %q", url)
			}
		default:
			if url != "data:application/octet-stream;base64,aGk=" {
				t.Errorf("default media type: url = %q", url)
			}
		}
	}
}

func TestTranslateRequestWhitespaceTextDropped(t *testing.T) {
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"  \n\t "}]`)},
		},
	}
	out, err := TranslateRequest(in)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	// Message with no renderable content is dropped entirely.
	if len(out.Messages) != 0 {
		t.Errorf("messages = %s", mustJSON(t, out.Messages))
	}
}

func TestTranslateRequestStreamOptions(t *testing.T) {
	in := &AnthropicRequest{
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
		Stream:   true,
	}
	out, _ := TranslateRequest(in)
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage missing")
	}
}
