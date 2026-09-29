// API client for the caddy-llm traces API (mounted at /llm/traces with the
// prefix stripped; see examples/).

export interface RequestSummary {
  trace_id: string
  trace_name: string
  timestamp: string
  duration_ms?: number
  status?: number
  req_bytes: number
  resp_bytes: number
  state?: 'in_progress' | 'crashed'
  input_tokens?: number
  cache_tokens?: number
  output_tokens?: number
  has_usage?: boolean
}

export interface RequestDetail extends RequestSummary {
  request_raw?: string // base64
  response_raw?: string // base64
}

export interface UsageBucket {
  timestamp: string
  input_tokens: number
  cache_tokens: number
  output_tokens: number
  requests: number
}

// The UI is served by llm_tracer_api at {api-prefix}/ui/; derive the API
// base from the page path so any mount prefix works. In the vite dev
// server (page at /) fall back to the proxied /llm/traces.
const BASE = (() => {
  const m = window.location.pathname.match(/^(.*)\/ui\/?$/)
  return m && m[1] ? m[1] : '/llm/traces'
})()

export async function fetchTraces(traceName: string, limit = 100, offset = 0): Promise<RequestSummary[]> {
  const q = new URLSearchParams({ limit: String(limit), offset: String(offset) })
  if (traceName) q.set('trace_name', traceName)
  const r = await fetch(`${BASE}?${q}`)
  if (!r.ok) throw new Error(`list traces: ${r.status}`)
  return r.json()
}

export async function fetchTrace(traceID: string, name: string): Promise<RequestDetail> {
  const r = await fetch(`${BASE}/${traceID}/${name}`)
  if (!r.ok) throw new Error(`get trace: ${r.status}`)
  return r.json()
}

export interface UsageParams {
  interval: number // seconds
  traceName?: string
  from?: Date
  to?: Date
}

export async function fetchUsage(p: UsageParams): Promise<UsageBucket[]> {
  const q = new URLSearchParams({ interval: String(p.interval) })
  if (p.traceName) q.set('trace_name', p.traceName)
  if (p.from) q.set('from', p.from.toISOString())
  if (p.to) q.set('to', p.to.toISOString())
  const r = await fetch(`${BASE}/usage?${q}`)
  if (!r.ok) throw new Error(`usage: ${r.status}`)
  return r.json()
}

export function decodeBase64(b64?: string): string {
  if (!b64) return ''
  const bin = atob(b64)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return new TextDecoder().decode(bytes)
}

// tracePartURL is the direct download URL for one direction's raw message
// (linkable — the UI anchors it so right-click "copy link" works).
export function tracePartURL(traceID: string, name: string, part: 'request' | 'response'): string {
  return `${BASE}/${traceID}/${name}?part=${part}`
}
