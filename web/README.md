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

Serve `dist/` from the same caddy-llm instance as the API so the UI and
`/llm/traces` share an origin. `handle` blocks are mutually exclusive and
sorted by matcher specificity, so list API routes before the static
catch-all:

```caddyfile
handle /llm/traces* {
	uri strip_prefix /llm/traces
	llm_tracer_api
}

handle /v1/* {
	route {
		trace mcli
		reverse_proxy https://upstream.example.com
	}
}

handle {
	root * /path/to/web/dist
	try_files {path} /index.html
	file_server
}
```

Note: a catch-all `handle { file_server }` placed before `route` blocks will
shadow them (directive order puts `handle` ahead of `route`) — always give
the API routes explicit matchers.
