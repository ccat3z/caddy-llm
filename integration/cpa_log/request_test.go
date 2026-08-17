package cpa_log

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ccat3z/caddy-llm/translate"
)

// logsDir is the CLIProxyAPI request-log corpus, controlled by CPA_LOG_DIR
// (default: ../CLIproxyAPI/data/logs relative to the repo root). Tests skip
// when the directory is absent.
var logsDir = func() string {
	if d := os.Getenv("CPA_LOG_DIR"); d != "" {
		return d
	}
	return filepath.Join("..", "..", "..", "CLIproxyAPI", "data", "logs")
}()

const (
	maxGoldenFile = 512 * 1024 // skip unusually large transcripts
	defaultSample = 300        // files exercised per run
)

// corpusFiles deterministically samples usable log files: sorted by name,
// filtered by pattern and size, then every Kth file up to defaultSample.
// CORPUS_ALL=1 uses every matching file; CORPUS_SAMPLE=N overrides the target.
func corpusFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Skipf("logs dir not available: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "v1-messages-2") || !strings.HasSuffix(n, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > maxGoldenFile {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	if os.Getenv("CORPUS_ALL") == "1" {
		return names
	}
	target := defaultSample
	if v := os.Getenv("CORPUS_SAMPLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			target = n
		}
	}
	if len(names) <= target {
		return names
	}
	step := len(names) / target
	out := make([]string, 0, target)
	for i := 0; i < len(names) && len(out) < target; i += step {
		out = append(out, names[i])
	}
	return out
}

// TestCorpusRequestTranslation replays (client Claude request -> upstream
// OpenAI request) pairs from real logs through translate.TranslateRequest and compares
// semantically (unmarshal + reflect.DeepEqual) against what CLIProxyAPI sent.
func TestCorpusRequestTranslation(t *testing.T) {
	files := corpusFiles(t)
	if len(files) == 0 {
		t.Skip("no usable golden files")
	}
	ran := 0
	for _, name := range files {
		lg, err := parseLogFile(name)
		if err != nil {
			continue
		}
		if !lg.IsChatCompletions() || len(lg.APIRequests) == 0 || len(lg.RequestBody) == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			var in translate.AnthropicRequest
			if err := json.Unmarshal(lg.RequestBody, &in); err != nil {
				t.Skipf("unparseable client body: %v", err)
			}
			out, err := translate.TranslateRequest(&in)
			if err != nil {
				t.Fatalf("translate.TranslateRequest: %v", err)
			}
			got, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// CLIProxyAPI's deployment config applied payload overrides (e.g.
			// max_tokens: 128000 for glm-5.1) after translation. Our translator
			// passes the client's value through, so normalize the golden's
			// max_tokens back to the client's before comparing.
			golden := normalizeGoldenOverrides(lg.APIRequests[0].Body, &in)
			assertJSONDiffEq(t, "translated request", golden, got)
		})
		ran++
	}
	if ran == 0 {
		t.Skip("no chat-completions pairs found in sample")
	}
	t.Logf("verified %d/%d files", ran, len(files))
}

// jsonDeepEq compares decoded JSON values, treating nil vs empty-slice and
// nil vs empty-map as equal (marshaling differences).
func jsonDeepEq(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, ok := bv[k]
			if !ok || !jsonDeepEq(v, bvv) {
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
			if !jsonDeepEq(av[i], bv[i]) {
				return false
			}
		}
		return true
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

func truncJSON(b []byte) string {
	const max = 4000
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

func parseLogFile(name string) (*Log, error) {
	f, err := os.Open(filepath.Join(logsDir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// normalizeGoldenOverrides rewrites deployment-config artifacts in a golden
// upstream body back to pure-translation semantics:
//   - max_tokens may have been overridden by the deployment's payload rules
//   - model may have had a provider prefix stripped (e.g. "friday/glm-5.2")
func normalizeGoldenOverrides(golden []byte, in *translate.AnthropicRequest) []byte {
	var m map[string]any
	if err := json.Unmarshal(golden, &m); err != nil {
		return golden
	}
	changed := false
	if mt, ok := m["max_tokens"].(float64); ok && in.MaxTokens > 0 && int(mt) != in.MaxTokens {
		m["max_tokens"] = in.MaxTokens
		changed = true
	}
	if gm, ok := m["model"].(string); ok && in.Model != "" && gm != in.Model &&
		strings.HasSuffix(in.Model, "/"+gm) {
		// The deployment stripped a provider prefix ("friday/glm-5.2" ->
		// "glm-5.2"); our translation passes the client name through, so
		// compare against the client's name.
		m["model"] = in.Model
		changed = true
	}
	if !changed {
		return golden
	}
	b, err := json.Marshal(m)
	if err != nil {
		return golden
	}
	return b
}
