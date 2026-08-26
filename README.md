# caddy-llm

Caddy plugin for proxying LLM APIs — a clean rewrite of
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Translates the
Anthropic Messages API (`/v1/messages`) to OpenAI chat-completions upstreams,
with request tracing, using only stock Caddy mechanics (`reverse_proxy` does
the actual forwarding).

**JSON configuration only** — no Caddyfile directives are registered. Run
with `caddy-llm run --config caddy.json`; see `examples/`.

## Modules

| Module ID | What it does |
|---|---|
| `http.handlers.claude2openai` | Translates Anthropic request/response bodies to OpenAI chat-completions and back. Path routing and upstream-path rewriting are left to route matchers and the `rewrite` handler; forwarding to `reverse_proxy`. |
| `http.handlers.llm_route` | Model-based upstream routing with fallthrough: each block matches the request's model (literal or anchored regex with rewrite), runs its own subchain on a cloned request, and falls through to the next block on 429/404/5xx or subchain errors. |
| `http.handlers.trace` | Captures the request/response passing through it (both sides of a translation when chained) and records them to the trace store. |
| `http.handlers.llm_tracer_api` | HTTP query API for recorded traces. |
| `llm_tracer` (app) | Trace persistence: append-only `traces.jsonl` + in-memory index. |

## Examples

Runnable configurations in [`examples/`](examples/), all validated:

- [`claude2openai.json`](examples/claude2openai.json) — minimal translation
  chain: one route, `rewrite` → `claude2openai` → `reverse_proxy`.
- [`traced-translation.json`](examples/traced-translation.json) — the same
  chain with `trace` on both sides of the translation and the traces API.
- [`multi-upstream-fallback.json`](examples/multi-upstream-fallback.json) —
  two `llm_route` blocks (Claude-native upstream first, OpenAI upstream with
  translation as fallback) plus the tracer app.

```
caddy-llm run --config examples/traced-translation.json
```

Point any Anthropic client (Claude Code, Anthropic SDK) at the server.
Requests to `POST /v1/messages` are translated and forwarded; sub-resources
like `/v1/messages/count_tokens` pass through untouched (keep their routes
separate, as in the examples).

## Multi-upstream routing (llm_route)

Each upstream is one `llm_route` handler; handler order is the priority
order. The shape of one block (from
[`examples/multi-upstream-fallback.json`](examples/multi-upstream-fallback.json)):

```json
{
  "handler": "llm_route",
  "models": [
    { "pattern": "glm/(.*)", "replace": "$1" },
    { "pattern": "glm-5.2" }
  ],
  "sub": {
    "handler": "subroute",
    "routes": [
      {
        "handle": [
          { "handler": "rewrite", "uri": "/api/coding/paas/v4/chat/completions" },
          { "handler": "claude2openai" },
          { "handler": "trace", "stage": "glm" },
          { "handler": "reverse_proxy", "upstreams": [{ "dial": "open.bigmodel.cn:443" }] }
        ]
      }
    ]
  }
}
```

Behavior:

- A matching rule rewrites the model name (regex with `$1` template) on a
  deep copy of the parsed object. The request is cloned, so nothing leaks
  between blocks — a fallthrough hands the next block the pristine original.
- Fallthrough happens when no rule matches, or the subchain answers 429/404/
  5xx, or the subchain errors (dial failure). The next llm_route then sees
  the pristine original request.
- Put `trace` handlers inside each block: failed attempts keep their
  response bodies in the trace store, so fallbacks are distinguishable.

### Programmatic body access

After the first llm_route (or `claude2openai` — it converts if needed), the
request body is a `*llmroute.Body`: the parsed JSON object (`Body.Obj`, a
`map[string]any`) is the authoritative body. Handlers in the chain can
type-assert it and read or mutate the object directly — no byte reads, no
re-parsing (`claude2openai` and `trace` both use this). Regular
`io.ReadCloser` consumption keeps working: the bytes are marshaled from the
object once, on first read. After that the bytes are frozen — check
`Body.Readonly()` before mutating `Obj`. Since the length is only known at
marshal time, requests are sent chunked (`ContentLength` -1) with `GetBody`
serving retries from the frozen bytes.

## How it works

```
client ──Claude──▶ trace(claude) ──▶ claude2openai ──OpenAI──▶ trace(openai) ──▶ reverse_proxy ──▶ upstream
                                         translates the parsed body in place,
                                         strips anthropic-* headers
```

Responses flow back through the same chain: non-streaming JSON is buffered
and translated in one shot; SSE streams are converted incrementally
(line-buffered, flushed per event). Tool-call arguments are buffered per
OpenAI index and flushed as a single `input_json_delta`. `[DONE]` is never
forwarded to the client. Errors are mapped to the Anthropic error envelope
(`{"type":"error","error":{"type":...,"message":...}}`) with upstream JSON
error bodies overriding the status-derived type.

Both tracers of one request share an `X-LLM-Trace-ID` (also returned to the
client as a response header); each stage records its own entry
(`<trace-id>/<stage>`), so a Claude-format and an OpenAI-format capture of
the same exchange are correlated. Captured `Authorization`/`X-Api-Key`
headers are redacted before persistence.

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

Run packages sequentially (`for p in translate llmroute claudetoopenai trace
integration integration/cpa; do go test ./$p/; done`) — the integration tests
bind fixed ports and conflict when run in parallel.

Two layers of translation tests, both driven by CLIProxyAPI request logs
(original Claude request → forwarded OpenAI request, upstream response →
client-facing response):

1. **CLIProxyAPI regression tests** (`integration/cpa`) replay the real log
   directory — every recorded exchange becomes a regression case, in both
   the struct path (`TranslateRequest`) and the map path
   (`TranslateRequestMap`) — from `CPA_LOG_DIR` (default:
   `../CLIproxyAPI/data/logs`); tests skip when absent. Controls:
   - `CPA_REGRESSION_SAMPLE=N` — number of files to test (default 300)
   - `CPA_REGRESSION_ALL=1` — every file (~20k verified pairs, ~1 min)
2. **Committed sanitized cases** (`integration/cpa/testdata`) run
   everywhere without the log directory: five representative exchanges (text
   stream, tool-call stream, thinking stream, non-streaming tool response,
   mid-stream error) extracted from real logs with credentials, cookies,
   session IDs, hostnames, and user paths redacted — guarded by a leak test.

Package layout:

```
translate/                pure translation library (no Caddy deps)
                          struct + map-native entry points
claudetoopenai/           claude2openai handler
llmroute/                 llm_route handler + the in-memory JSON body type
trace/                    trace handler + llm_tracer app + query API
integration/              full-chain integration tests (JSON configs)
integration/cpa/          CLIProxyAPI regression tests, sanitized cases,
                          and the log-format parser (logparse.go)
examples/                 validated JSON config examples
cmd/caddy-llm/            custom binary entry
all.go                    side-effect import of every module
```

## Status / limitations

- Claude → OpenAI only (no Gemini, no OpenAI Responses dialect).
- `count_tokens` is passed through, not answered locally.
- No credential rotation — set the upstream `Authorization` header yourself
  (e.g. on `reverse_proxy`'s `headers.request.set`).
- JSON config only; no Caddyfile directives.
