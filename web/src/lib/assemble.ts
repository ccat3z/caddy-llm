// assemble.ts — merge an SSE event stream back into a single logical
// message for pretty display (like CPA's "assembled" view). Supports the
// Claude Messages stream (message_start / content_block_* / message_delta),
// OpenAI chat.completion.chunk streams, and OpenAI Responses streams
// (response.* events).

export interface Assembled {
  format: 'claude' | 'openai' | 'openai-responses'
  events: number
  message: Record<string, unknown>
}

interface SSEPayload {
  data: string
}

function payloads(body: string): SSEPayload[] {
  const out: SSEPayload[] = []
  for (const line of body.split('\n')) {
    const l = line.replace(/\r$/, '')
    if (l.startsWith('data: ')) {
      const data = l.slice(6)
      if (data !== '[DONE]') out.push({ data })
    }
  }
  return out
}

// assembleSSE returns null when the stream isn't a recognizable LLM stream
// (caller falls back to raw display).
export function assembleSSE(body: string): Assembled | null {
  const events = payloads(body)
  if (events.length === 0) return null
  const parsed: any[] = []
  for (const e of events) {
    try {
      parsed.push(JSON.parse(e.data))
    } catch {
      return null // glued/corrupt payloads — show raw
    }
  }
  if (parsed.some((p) => p?.object === 'chat.completion.chunk')) {
    return assembleOpenAI(parsed)
  }
  if (parsed.some((p) => typeof p?.type === 'string' && p.type.startsWith('response.'))) {
    return assembleResponses(parsed)
  }
  if (parsed.some((p) => p?.type === 'message_start' || p?.type === 'content_block_start')) {
    return assembleClaude(parsed)
  }
  return null
}

function assembleClaude(chunks: any[]): Assembled | null {
  const msg: Record<string, unknown> = { role: 'assistant' }
  const blocks: any[] = []
  const texts: Record<number, string> = {}
  let stopReason: string | undefined
  let usage: Record<string, unknown> | undefined

  for (const c of chunks) {
    switch (c.type) {
      case 'message_start': {
        const m = c.message ?? {}
        msg.id = m.id
        msg.type = m.type
        msg.model = m.model
        if (m.usage) usage = { ...m.usage }
        break
      }
      case 'content_block_start': {
        const i = c.index ?? blocks.length
        const b = c.content_block ?? {}
        blocks[i] = { type: b.type }
        if (b.type === 'tool_use') {
          blocks[i].id = b.id
          blocks[i].name = b.name
        }
        texts[i] = ''
        break
      }
      case 'content_block_delta': {
        const i = c.index ?? 0
        const d = c.delta ?? {}
        if (d.type === 'text_delta' || d.type === 'thinking_delta') {
          texts[i] = (texts[i] ?? '') + (d.text ?? d.thinking ?? '')
        } else if (d.type === 'input_json_delta') {
          texts[i] = (texts[i] ?? '') + (d.partial_json ?? '')
        }
        break
      }
      case 'message_delta': {
        if (c.delta?.stop_reason) stopReason = c.delta.stop_reason
        if (c.usage) usage = { ...usage, ...c.usage }
        break
      }
    }
  }

  const content = blocks
    .map((b, i) => {
      if (!b) return null
      const text = texts[i] ?? ''
      switch (b.type) {
        case 'text':
          return { ...b, text }
        case 'thinking':
          return { ...b, thinking: text }
        case 'tool_use': {
          let input: unknown = text
          try {
            input = JSON.parse(text)
          } catch {
            /* leave as raw string */
          }
          return { ...b, input }
        }
        default:
          return b
      }
    })
    .filter(Boolean)
  if (content.length === 0 && !usage) return null

  msg.content = content
  if (stopReason) msg.stop_reason = stopReason
  if (usage) msg.usage = usage
  return { format: 'claude', events: chunks.length, message: msg }
}

function assembleOpenAI(chunks: any[]): Assembled | null {
  let id: string | undefined
  let model: string | undefined
  let role: string | undefined
  let content = ''
  let finishReason: string | undefined
  let usage: Record<string, unknown> | undefined
  const toolCalls: Record<number, { id?: string; name: string; arguments: string }> = {}

  for (const c of chunks) {
    if (c.id) id = c.id
    if (c.model) model = c.model
    if (c.usage) usage = c.usage
    const ch = c.choices?.[0]
    if (!ch) continue
    const d = ch.delta ?? {}
    if (d.role) role = d.role
    if (typeof d.content === 'string') content += d.content
    if (ch.finish_reason) finishReason = ch.finish_reason
    for (const tc of d.tool_calls ?? []) {
      const i = tc.index ?? 0
      toolCalls[i] = toolCalls[i] ?? { name: '', arguments: '' }
      if (tc.id) toolCalls[i].id = tc.id
      if (tc.function?.name) toolCalls[i].name += tc.function.name
      if (tc.function?.arguments) toolCalls[i].arguments += tc.function.arguments
    }
  }

  const tools = Object.values(toolCalls)
  if (!content && tools.length === 0 && !usage) return null

  const message: Record<string, unknown> = {}
  if (id) message.id = id
  message.object = 'chat.completion'
  if (model) message.model = model
  const msgContent: Record<string, unknown> = {}
  if (role) msgContent.role = role
  if (content) msgContent.content = content
  if (tools.length > 0) {
    msgContent.tool_calls = tools.map((t) => {
      let args: unknown = t.arguments
      try {
        args = JSON.parse(t.arguments)
      } catch {
        /* raw */
      }
      return { id: t.id, type: 'function', function: { name: t.name, arguments: args } }
    })
  }
  message.choices = [{ index: 0, message: msgContent, finish_reason: finishReason ?? null }]
  if (usage) message.usage = usage
  return { format: 'openai', events: chunks.length, message }
}

// assembleResponses rebuilds an OpenAI Responses object from its stream.
// The terminal event carries the authoritative response (output, usage,
// status); streamed item/delta events are the fallback for truncated
// streams. output_index preserves the upstream item order.
function assembleResponses(chunks: any[]): Assembled | null {
  let base: Record<string, any> | undefined
  const items = new Map<number, Record<string, any>>()
  const text = new Map<string, string>() // "<output_index>:<content_index>" -> text
  const summary = new Map<string, string>() // "<output_index>:<summary_index>" -> text
  const args = new Map<number, string>()

  const append = (m: Map<string, string>, key: string, delta: unknown) => {
    if (typeof delta === 'string' && delta) m.set(key, (m.get(key) ?? '') + delta)
  }

  for (const c of chunks) {
    if (c.response && typeof c.response === 'object') base = c.response
    const idx: number = typeof c.output_index === 'number' ? c.output_index : 0
    switch (c.type) {
      case 'response.output_item.added':
      case 'response.output_item.done':
        // done supersedes added: it carries the complete item.
        if (c.item && typeof c.item === 'object') items.set(idx, { ...c.item })
        break
      case 'response.output_text.delta':
      case 'response.refusal.delta':
      case 'response.reasoning_text.delta':
        append(text, `${idx}:${c.content_index ?? 0}`, c.delta)
        break
      case 'response.reasoning_summary_text.delta':
        append(summary, `${idx}:${c.summary_index ?? 0}`, c.delta)
        break
      case 'response.function_call_arguments.delta':
        if (typeof c.delta === 'string') args.set(idx, (args.get(idx) ?? '') + c.delta)
        break
    }
  }

  const parts = (m: Map<string, string>, idx: number, type: string) => {
    const out: Record<string, unknown>[] = []
    for (const [key, value] of m) {
      const [i, part] = key.split(':').map(Number)
      if (i === idx && value) out[part] = { type, text: value }
    }
    return out.filter(Boolean)
  }

  for (const [idx, item] of items) {
    if (item.type === 'reasoning') {
      if (!item.summary?.length) {
        const s = parts(summary, idx, 'summary_text')
        if (s.length > 0) item.summary = s
      }
      if (!item.content?.length) {
        const t = parts(text, idx, 'reasoning_text')
        if (t.length > 0) item.content = t
      }
    } else if (item.type === 'function_call') {
      if (!item.arguments && args.has(idx)) item.arguments = args.get(idx)
    } else if (!item.content?.length) {
      const t = parts(text, idx, 'output_text')
      if (t.length > 0) item.content = t
    }
  }

  const streamed = [...items.entries()].sort(([a], [b]) => a - b).map(([, item]) => item)
  if (!base && streamed.length === 0) return null

  const message: Record<string, unknown> = base ? { ...base } : { object: 'response' }
  message.output = Array.isArray(base?.output) && base.output.length > 0 ? base.output : streamed
  return { format: 'openai-responses', events: chunks.length, message }
}
