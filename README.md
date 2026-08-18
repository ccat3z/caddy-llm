# caddy-llm

Caddy plugin for proxying LLM APIs — a clean rewrite of
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Translates the
Anthropic Messages API (`/v1/messages`) to OpenAI chat-completions upstreams,
with request tracing, using only stock Caddy mechanics (`reverse_proxy` does
the actual forwarding).

## Modules

| Caddyfile directive | Module ID | What it does |
|---|---|---|
| `claude2openai` | `http.handlers.claude2openai` | Translates Anthropic request/response bodies to OpenAI chat-completions and back. Path routing and upstream-path rewriting are left to route matchers and the `rewrite` handler; forwarding to `reverse_proxy`. |
| `trace <stage>` | `http.handlers.trace` | Captures the request/response passing through it (both sides of a translation when chained) and records them to the trace store. |
| `llm_route` | `http.handlers.llm_route` | Model-based upstream routing with fallthrough: each block matches the request's model (literal or anchored regex with rewrite), runs its own subchain on a cloned request, and falls through to the next block on 429/404/5xx or subchain errors. |
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
	# Only POST /v1/messages enters the translation chain; everything else
	# (count_tokens, /v1/models, ...) never reaches it.
	@claude path /v1/messages
	handle @claude {
		route {
			trace claude
			# Upstream path is the stock rewrite handler's job — set the full
			# path when the upstream base URL carries a prefix.
			rewrite * /v1/chat/completions
			claude2openai
			trace openai
			reverse_proxy https://api.openai.com {
				header_up Authorization "Bearer {$UPSTREAM_KEY}"
			}
		}
	}

	handle /llm/traces* {
		llm_tracer_api
	}
	respond 404
}
```

Point any Anthropic client (Claude Code, Anthropic SDK) at the server. Requests
to `POST /v1/messages` are translated and forwarded; sub-resources like
`/v1/messages/count_tokens` pass through untouched.

## JSON configuration

Everything is configurable via raw JSON too (`caddy-llm run --config caddy.json`),
with the same handler chain:

```json
{
  "apps": {
    "llm_tracer": { "dir": "/var/lib/caddy/llm-traces" },
    "http": {
      "servers": {
        "srv0": {
          "listen": [":443"],
          "routes": [
            {
              "match": [{ "path": ["/llm/traces*"] }],
              "handle": [{ "handler": "llm_tracer_api" }]
            },
            {
              "handle": [
                { "handler": "trace", "stage": "claude" },
                { "handler": "claude2openai" },
                { "handler": "trace", "stage": "openai" },
                {
                  "handler": "reverse_proxy",
                  "upstreams": [{ "dial": "open.bigmodel.cn:443" }]
                }
              ]
            }
          ]
        }
      }
    }
  }
}
```

Note: unlike the Caddyfile adapter, raw JSON routes match strictly in order —
keep the `/llm/traces*` route (and any other matched routes) above the
catch-all proxy route.

## Multi-upstream routing (llm_route)

Each upstream is one `llm_route` block; block order is the priority order:

```caddyfile
@claude path /v1/messages
route @claude {
	llm_route {
		model mc/(.*) $1            # regex: strip the mc/ prefix
		model glm-5.2               # literal: also serve the bare short name
		route {
			rewrite * /v2/chat
			trace mc
			reverse_proxy https://mcli.sankuai.com {
				header_up Authorization "Bearer {$MC_KEY}"
			}
		}
	}
	llm_route {
		model glm/(.*) $1
		model glm-5.2
		route {
			rewrite * /api/coding/paas/v4/chat/completions
			claude2openai            # this upstream speaks OpenAI
			trace glm
			reverse_proxy https://open.bigmodel.cn {
				header_up Authorization "Bearer {$GLM_KEY}"
			}
		}
	}
	respond "model not found or all upstreams failed" 404
}
```

Behavior:
- The client's original model name is extracted once (first llm_route) and
  reused, so fallbacks always match what the client asked for.
- A matching rule may rewrite the model (regex with `$1` template) before the
  subchain runs; the request is cloned, so nothing leaks between blocks.
- Fallthrough happens when no rule matches, or the subchain answers 429/404/
  5xx, or the subchain errors (dial failure). The next llm_route then sees
  the pristine original request.
- Put `trace <upstream-id>` inside each block: failed attempts keep their
  response bodies in the trace store, so fallbacks are distinguishable.

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

1. **CLIProxyAPI regression tests** (`integration/cpa`) replay the real log
   directory — every recorded exchange becomes a regression case.
   from `CPA_LOG_DIR` (default: `../CLIproxyAPI/data/logs`); tests skip
   when absent. Controls:
   - `CPA_REGRESSION_SAMPLE=N` — number of files to test (default 300)
   - `CPA_REGRESSION_ALL=1` — every file (~20k verified pairs, ~1 min)
2. **Committed sanitized cases** (`integration/cpa/testdata`) run
   everywhere without the log directory: five representative exchanges (text stream,
   tool-call stream, thinking stream, non-streaming tool response, mid-stream
   error) extracted from real logs with credentials, cookies, session IDs,
   hostnames, and user paths redacted — guarded by a leak test.

Package layout:

```
translate/                pure translation library (no Caddy deps)
claudetoopenai/           claude2openai handler
trace/                    trace handler + llm_tracer app + query API
integration/              full-chain integration tests (Caddyfile and JSON)
integration/cpa/          CLIProxyAPI regression tests, sanitized cases,
                          and the log-format parser (logparse.go)
cmd/caddy-llm/            custom binary entry
all.go                    side-effect import of every module
```

## Status / limitations

- Claude → OpenAI only (no Gemini, no OpenAI Responses dialect).
- `count_tokens` is passed through, not answered locally.
- No credential rotation — set the upstream `Authorization` header yourself
  (e.g. via `header_up` on `reverse_proxy`).
