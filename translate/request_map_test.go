package translate

import (
	"encoding/json"
	"testing"
)

// bothPaths runs the same input through the struct path
// (unmarshal → TranslateRequest → marshal) and the map path
// (TranslateRequestMap → marshal), returning both wire forms.
func bothPaths(t *testing.T, in string) (structPath, mapPath []byte) {
	t.Helper()
	var req AnthropicRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	out, err := TranslateRequest(&req)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	structPath, err = json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal struct path: %v", err)
	}

	var inMap map[string]any
	if err := json.Unmarshal([]byte(in), &inMap); err != nil {
		t.Fatalf("unmarshal input to map: %v", err)
	}
	outMap, err := TranslateRequestMap(inMap)
	if err != nil {
		t.Fatalf("TranslateRequestMap: %v", err)
	}
	mapPath, err = json.Marshal(outMap)
	if err != nil {
		t.Fatalf("marshal map path: %v", err)
	}
	return structPath, mapPath
}

// TestTranslateRequestMapEquivalence locks the map path to the struct path
// on a matrix of representative request shapes.
func TestTranslateRequestMapEquivalence(t *testing.T) {
	cases := map[string]string{
		"minimal":             `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		"streaming":           `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"system string":       `{"model":"m","max_tokens":8,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`,
		"system blocks":       `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`,
		"content block array": `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
		"image base64":        `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},{"type":"text","text":"what is this"}]}]}`,
		"image url":           `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}]}`,
		"assistant tool_use roundtrip": `{"model":"m","max_tokens":8,"messages":[
			{"role":"user","content":"run it"},
			{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{"x":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"42"}]},
			{"role":"user","content":"and now?"}
		]}`,
		"tool_result image content": `{"model":"m","max_tokens":8,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}
		]}`,
		"signed thinking":         `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"}]},{"role":"user","content":"go"}]}`,
		"mid-conversation system": `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"a"},{"role":"system","content":"note"},{"role":"user","content":"b"}]}`,
		"tools":                   `{"model":"m","max_tokens":8,"tools":[{"name":"f","description":"do","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}],"messages":[{"role":"user","content":"hi"}]}`,
		"tools empty properties":  `{"model":"m","max_tokens":8,"tools":[{"name":"f","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`,
		"tool_choice any":         `{"model":"m","max_tokens":8,"tool_choice":{"type":"any","disable_parallel_tool_use":false},"messages":[{"role":"user","content":"hi"}]}`,
		"tool_choice tool":        `{"model":"m","max_tokens":8,"tool_choice":{"type":"tool","name":"f"},"messages":[{"role":"user","content":"hi"}]}`,
		"stop sequences single":   `{"model":"m","max_tokens":8,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`,
		"stop sequences multi":    `{"model":"m","max_tokens":8,"stop_sequences":["A","B"],"messages":[{"role":"user","content":"hi"}]}`,
		"temperature":             `{"model":"m","max_tokens":8,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`,
		"top_p (no temperature)":  `{"model":"m","max_tokens":8,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`,
		"thinking enabled low":    `{"model":"m","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`,
		"thinking adaptive":       `{"model":"m","max_tokens":8,"thinking":{"type":"adaptive"},"output_config":{"effort":"High"},"messages":[{"role":"user","content":"hi"}]}`,
		"metadata":                `{"model":"m","max_tokens":8,"metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"hi"}]}`,
		"empty messages":          `{"model":"m","max_tokens":8,"messages":[]}`, // struct path emits messages:null — not a wire difference
		"no messages field":       `{"model":"m","max_tokens":8}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			structPath, mapPath := bothPaths(t, in)
			var a, b any
			if err := json.Unmarshal(structPath, &a); err != nil {
				t.Fatalf("struct path output not JSON: %v (%s)", err, structPath)
			}
			if err := json.Unmarshal(mapPath, &b); err != nil {
				t.Fatalf("map path output not JSON: %v (%s)", err, mapPath)
			}
			if diff := jsonSemanticsDiff("", a, b); len(diff) > 0 {
				t.Errorf("paths differ:\n%s\nstruct: %s\nmap:    %s", diff, structPath, mapPath)
			}
		})
	}
}

// TestTranslateRequestMapErrors checks the error paths.
func TestTranslateRequestMapErrors(t *testing.T) {
	// messages typed wrong fails struct decoding
	if _, err := TranslateRequestMap(map[string]any{"model": "m", "messages": "nope"}); err == nil {
		t.Error("bad messages type should fail")
	}
	// max_tokens typed wrong fails struct decoding
	if _, err := TranslateRequestMap(map[string]any{"model": "m", "max_tokens": "big"}); err == nil {
		t.Error("bad max_tokens type should fail")
	}
}

// TestEmptyMessagesPathsAgree locks the alignment fix: a request whose
// messages all translate away must produce the same wire form on both paths
// (the struct path used to emit "messages":null while the map path omitted
// the key — production traffic uses the map path).
func TestEmptyMessagesPathsAgree(t *testing.T) {
	structPath, mapPath := bothPaths(t, `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[]}]}`)
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(structPath, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mapPath, &b); err != nil {
		t.Fatal(err)
	}
	_, hasA := a["messages"]
	_, hasB := b["messages"]
	if hasA != hasB {
		t.Errorf("messages presence differs: struct=%v map=%v\nstruct: %s\nmap: %s", hasA, hasB, structPath, mapPath)
	}
}
