# caddy-llm

Caddy plugin for proxying LLM APIs — a clean rewrite of
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Translates the
Anthropic Messages API (`/v1/messages`) to OpenAI chat-completions upstreams,
with request tracing, using only stock Caddy mechanics (`reverse_proxy` does
the actual forwarding).

## Modules

| Caddyfile directive | Module ID | What it does |
|---|---|---|
| `claude2openai` | `http.handlers.claude2openai` | Translates Anthropic `/v1/messages` requests to OpenAI chat-completions and translates responses (JSON and SSE) back. Forwarding is left to `reverse_proxy`. |
| `trace <stage>` | `http.handlers.trace` | Captures the request/response passing through it (both sides of a translation when chained) and records them to the trace store. |
| `llm_tracer_api` | `http.handlers.llm_tracer_api` | HTTP query API for recorded traces. |
| `llm_tracer` (global) | `llm_tracer` | Trace persistence: append-only `traces.jsonl` + in-memory index. |

## Quick start

```caddyfile
{
	llm_tracer {
		dir /var/lib/caddy/llm-traces
	}
}

api.example.com {
	route {
		trace claude              # capture client-facing (Claude) traffic
		claude2openai            # Claude -> OpenAI
		trace openai              # capture upstream (OpenAI) traffic
		reverse_proxy https://open.bigmodel.cn {
			header_up Authorization "Bearer {$UPSTREAM_KEY}"
		}
	}

	route /llm/traces* {
		llm_tracer_api
	}
}
```

Point any Anthropic client (Claude Code, Anthropic SDK) at the server. Requests
to `POST /v1/messages` are translated and forwarded; sub-resources like
`/v1/messages/count_tokens` pass through untouched.

## How it works

```
client ──Claude──▶ trace(claude) ──▶ claude2openai ──OpenAI──▶ trace(openai) ──▶ reverse_proxy ──▶ upstream
                                         translates req/body/path,
                                         strips anthropic-* headers,
                                         sets GetBody (retry-safe)
```

Responses flow back through the same chain: non-streaming JSON is buffered and
translated in one shot; SSE streams are converted incrementally (line-buffered,
flushed per event). Tool-call arguments are buffered per OpenAI index and
flushed as a single `input_json_delta`. `[DONE]` is never forwarded to the
client. Errors are mapped to the Anthropic error envelope
(`{"type":"error","error":{"type":...,"message":...}}`) with upstream JSON
error bodies overriding the status-derived type.

Both tracers of one request share an `X-LLM-Trace-ID` (also returned to the
client as a response header); each stage records its own entry
(`<trace-id>/<stage>`), so a Claude-format and an OpenAI-format capture of the
same exchange are correlated. Captured `Authorization`/`X-Api-Key` headers are
redacted before persistence.

### Trace API

- `GET /llm/traces?stage=claude&limit=50&offset=0` — newest-first summaries.
- `GET /llm/traces/{id}` — full entry including bodies (base64 in JSON).

## Build

```
go build ./cmd/caddy-llm
```

Or use [xcaddy](https://github.com/caddyserver/xcaddy):
`xcaddy build --with github.com/ccat3z/caddy-llm`.

## Testing

```
go test ./...
```

Two layers of translation tests, both driven by CLIProxyAPI request logs
(original Claude request → forwarded OpenAI request, upstream response →
client-facing response):

1. **Corpus integration tests** (`integration/cpa`) replay the real log
   directory. The directory comes from `CPA_LOG_DIR` (default:
   `../CLIProxyAPI/data/logs`); tests skip when it is absent. Controls:
   - `CORPUS_SAMPLE=N` — number of files to test (default 300)
   - `CORPUS_ALL=1` — every file (~20k verified pairs, ~1 min)
2. **Committed sanitized cases** (`integration/cpa/testdata`) run
   everywhere without the corpus: five representative exchanges (text stream,
   tool-call stream, thinking stream, non-streaming tool response, mid-stream
   error) extracted from real logs with credentials, cookies, session IDs,
   hostnames, and user paths redacted — guarded by a leak test.

Package layout:

```
translate/                pure translation library (no Caddy deps)
claudetoopenai/           claude2openai handler
trace/                    trace handler + llm_tracer app + query API
integration/              full-chain integration tests
integration/cpa/      corpus replay tests, sanitized cases, and the
                          CLIProxyAPI log-format parser (logparse.go)
cmd/caddy-llm/            custom binary entry
all.go                    side-effect import of every module
```

## Status / limitations

- Claude → OpenAI only (no Gemini, no OpenAI Responses dialect).
- `count_tokens` is passed through, not answered locally.
- No credential rotation — set the upstream `Authorization` header yourself
  (e.g. via `header_up` on `reverse_proxy`).
