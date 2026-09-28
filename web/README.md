# caddy-llm traces UI

Web dashboard for the `llm_tracer` API: token-usage time series (input /
cache / output stacked, adjustable interval) and the request event table
with expandable raw HTTP exchanges.

Stack: React + TypeScript + Vite, Tailwind CSS, shadcn/ui components,
Recharts.

## Develop

```
npm install
npm run dev          # vite dev server, proxies /llm -> http://127.0.0.1:8080
```

Point the proxy at wherever caddy-llm serves the traces API
(`vite.config.ts`, default `http://127.0.0.1:8080` — e.g.
`caddy-llm run --config examples/traced-translation.json`).

## Build & serve

```
npm run build        # outputs dist/
```

`web/dist` is **embedded into the Go binary** (`web/embed.go`) and served by
the `llm_tracer_api` handler at `{mount}/ui/` — e.g. with the API mounted at
`/llm/traces`, the dashboard is at `/llm/traces/ui/`. Build the UI before
`go build`; a fresh clone (dist has only a `.gitkeep` placeholder) builds
fine and serves a "not built" hint page at the UI route instead.

No static file server is needed:

```caddyfile
handle /llm/traces* {
	uri strip_prefix /llm/traces
	llm_tracer_api
}
```

(If you ever serve SPA files with `file_server` instead, give every API
route an explicit matcher: a catch-all `handle { file_server }` sorts
before `route` blocks and shadows them.)
