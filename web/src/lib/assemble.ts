// assemble.ts — merge an SSE event stream back into a single logical
// message for pretty display (like CPA's "assembled" view). Supports the
// Claude Messages stream (message_start / content_block_* / message_delta)
// and OpenAI chat.completion.chunk streams.

export interface Assembled {
  format: 'claude' | 'openai'
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
