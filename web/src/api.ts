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

const BASE = '/llm/traces'

export async function fetchTraces(stage: string, limit = 100): Promise<RequestSummary[]> {
  const q = new URLSearchParams({ limit: String(limit) })
  if (stage) q.set('stage', stage)
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
  interval: string // Go duration ("5m", "1h") or bare seconds
  stage?: string
  from?: Date
  to?: Date
}

export async function fetchUsage(p: UsageParams): Promise<UsageBucket[]> {
  const q = new URLSearchParams({ interval: p.interval })
  if (p.stage) q.set('stage', p.stage)
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
