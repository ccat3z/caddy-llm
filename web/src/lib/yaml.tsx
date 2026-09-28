// yaml.tsx — YAML-ish pretty renderer for JSON values, ported from
// CLIProxyAPI's management UI (RequestEventsDetailsCard.tsx). Not a YAML
// serializer: a hand-rolled tokenizer + renderer tuned for LLM payloads —
// `system`/`tools` sections fold by default, multiline strings render as
// literal blocks (|) with a 5-line preview, and top-level keys are ordered
// so system/tools/messages come last.
import { useCallback, useMemo, useState, type ReactNode } from 'react'

type YamlToken =
  | { type: 'indent'; depth: number }
  | { type: 'key'; value: string }
  | { type: 'colon' }
  | { type: 'string'; value: string }
  | { type: 'number'; value: string }
  | { type: 'bool'; value: string }
  | { type: 'null' }
  | { type: 'dash' }
  | { type: 'pipe' }
  | { type: 'literal-line'; value: string }
  | { type: 'fold-marker'; collapsed: boolean; count: number }

const TOP_LEVEL_KEY_ORDER = ['system', 'tools', 'messages']
const COLLAPSED_BY_DEFAULT_KEYS = new Set(['system', 'tools'])
const MULTILINE_PREVIEW_LINES = 5

function jsonToYamlTokens(value: unknown, depth = 0, collapsedKeys?: Set<string>): YamlToken[][] {
  const ind = (d = depth) => [{ type: 'indent' as const, depth: d }]
  const lines: YamlToken[][] = []

  if (value === null || value === undefined) {
    lines.push([...ind(), { type: 'null' }])
    return lines
  }
  if (typeof value === 'boolean') {
    lines.push([...ind(), { type: 'bool', value: String(value) }])
    return lines
  }
  if (typeof value === 'number') {
    lines.push([...ind(), { type: 'number', value: String(value) }])
    return lines
  }
  if (typeof value === 'string') {
    if (value.includes('\n')) {
      lines.push([...ind(), { type: 'pipe' }])
      for (const line of value.split('\n')) {
        lines.push([{ type: 'indent', depth: depth + 1 }, { type: 'literal-line', value: line }])
      }
    } else {
      lines.push([...ind(), { type: 'string', value }])
    }
    return lines
  }
  if (Array.isArray(value)) {
    if (value.length === 0) {
      lines.push([...ind(), { type: 'string', value: '[]' }])
      return lines
    }
    for (const item of value) {
      const inner = jsonToYamlTokens(item, depth)
      if (inner.length > 0) {
        const first = inner[0]
        lines.push([
          ...ind(depth > 0 ? depth - 1 : 0),
          { type: 'dash' },
          ...(first[0]?.type === 'indent' ? first.slice(1) : first),
        ])
        lines.push(...inner.slice(1))
      }
    }
    return lines
  }
  if (typeof value === 'object') {
    let entries = Object.entries(value as Record<string, unknown>)
    if (entries.length === 0) {
      lines.push([...ind(), { type: 'string', value: '{}' }])
      return lines
    }
    if (depth === 0) {
      const ordered: [string, unknown][] = []
      const tail: [string, unknown][] = []
      for (const entry of entries) {
        if (TOP_LEVEL_KEY_ORDER.includes(entry[0])) {
          tail.push(entry)
        } else {
          ordered.push(entry)
        }
      }
      tail.sort((a, b) => TOP_LEVEL_KEY_ORDER.indexOf(a[0]) - TOP_LEVEL_KEY_ORDER.indexOf(b[0]))
      entries = [...ordered, ...tail]
    }
    for (const [k, v] of entries) {
      const isCollapsed = collapsedKeys?.has(k) ?? false
      if (typeof v === 'object' && v !== null) {
        lines.push([...ind(), { type: 'key', value: k }, { type: 'colon' }])
        const inner = jsonToYamlTokens(v, depth + 1, collapsedKeys)
        if (isCollapsed) {
          lines.push([{ type: 'fold-marker', collapsed: true, count: inner.length }])
        } else {
          lines.push(...inner)
        }
      } else if (typeof v === 'string' && v.includes('\n')) {
        lines.push([...ind(), { type: 'key', value: k }, { type: 'colon' }, { type: 'pipe' }])
        if (isCollapsed) {
          lines.push([{ type: 'fold-marker', collapsed: true, count: v.split('\n').length }])
        } else {
          for (const line of v.split('\n')) {
            lines.push([...ind(depth + 1), { type: 'literal-line', value: line }])
          }
        }
      } else {
        const valTokens = jsonToYamlTokens(v, 0)
        const firstLine = valTokens[0]
        if (firstLine) {
          const withoutIndent = firstLine[0]?.type === 'indent' ? firstLine.slice(1) : firstLine
          lines.push([...ind(), { type: 'key', value: k }, { type: 'colon' }, ...withoutIndent])
          lines.push(...valTokens.slice(1))
        }
      }
    }
    return lines
  }
  lines.push([...ind(), { type: 'string', value: String(value) }])
  return lines
}

function needsQuote(s: string): boolean {
  if (s === '') return true
  if (/[:\{\}\[\],&\*#\?|\-<>=!%@\\]/.test(s)) return true
  if (s === 'true' || s === 'false' || s === 'null') return true
  if (/^\d/.test(s)) return true
  if (s.includes('\n')) return true
  return false
}

const foldCls = 'ml-2 cursor-pointer select-none text-sky-600 hover:underline'

function YamlLine({ tokens, suffix }: { tokens: YamlToken[]; suffix?: ReactNode }) {
  return (
    <div className="whitespace-pre-wrap break-all">
      {tokens.map((tok, i) => {
        switch (tok.type) {
          case 'indent':
            return <span key={i}>{'  '.repeat(tok.depth)}</span>
          case 'key':
            return (
              <span key={i} className="text-sky-700">
                {tok.value}
              </span>
            )
          case 'colon':
            return <span key={i}>: </span>
          case 'string':
            return (
              <span key={i} className="text-emerald-700">
                {needsQuote(tok.value) ? JSON.stringify(tok.value) : tok.value}
              </span>
            )
          case 'number':
            return (
              <span key={i} className="text-amber-600">
                {tok.value}
              </span>
            )
          case 'bool':
            return (
              <span key={i} className="text-purple-600">
                {tok.value}
              </span>
            )
          case 'null':
            return (
              <span key={i} className="italic text-muted-foreground">
                null
              </span>
            )
          case 'dash':
            return <span key={i}>- </span>
          case 'pipe':
            return (
              <span key={i} className="text-muted-foreground">
                |
              </span>
            )
          case 'literal-line':
            return (
              <span key={i} className="text-emerald-700">
                {tok.value}
              </span>
            )
          case 'fold-marker':
            return null
        }
      })}
      {suffix}
    </div>
  )
}

export function YamlBlock({ value }: { value: unknown }) {
  const [collapsedKeys, setCollapsedKeys] = useState<Set<string>>(
    () => new Set(COLLAPSED_BY_DEFAULT_KEYS),
  )
  const [expandedLiterals, setExpandedLiterals] = useState<Set<number>>(new Set())
  const allTokens = useMemo(() => jsonToYamlTokens(value, 0, collapsedKeys), [value, collapsedKeys])

  const toggleKey = useCallback((key: string) => {
    setCollapsedKeys((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }, [])

  const toggleLiteral = useCallback((idx: number) => {
    setExpandedLiterals((prev) => {
      const next = new Set(prev)
      if (next.has(idx)) next.delete(idx)
      else next.add(idx)
      return next
    })
  }, [])

  const rendered: ReactNode[] = []
  let i = 0
  while (i < allTokens.length) {
    const line = allTokens[i]
    const marker = line.find((t) => t.type === 'fold-marker') as
      | { type: 'fold-marker'; collapsed: boolean; count: number }
      | undefined
    if (marker?.collapsed) {
      const lastIdx = rendered.length - 1
      if (lastIdx >= 0) {
        const prevLine = allTokens[i - 1]
        const keyToken = prevLine.find((t) => t.type === 'key')
        const keyName = keyToken && 'value' in keyToken ? keyToken.value : ''
        rendered[lastIdx] = (
          <YamlLine
            key={`foldkey-${i}`}
            tokens={prevLine}
            suffix={
              <span className={foldCls} onClick={() => keyName && toggleKey(keyName)}>
                ... {marker.count} more lines
              </span>
            }
          />
        )
      }
      i++
      continue
    }

    if (line.some((t) => t.type === 'pipe')) {
      const runStart = i + 1
      let runEnd = runStart
      while (runEnd < allTokens.length && allTokens[runEnd].some((t) => t.type === 'literal-line')) {
        runEnd++
      }
      const literalCount = runEnd - runStart
      const isExpanded = expandedLiterals.has(runStart)
      const canFold = literalCount > MULTILINE_PREVIEW_LINES
      const pipeSuffix = canFold ? (
        isExpanded ? (
          <span className={foldCls} onClick={() => toggleLiteral(runStart)}>
            collapse
          </span>
        ) : (
          <span className={foldCls} onClick={() => toggleLiteral(runStart)}>
            ... {literalCount - MULTILINE_PREVIEW_LINES} more lines
          </span>
        )
      ) : undefined
      rendered.push(<YamlLine key={i} tokens={line} suffix={pipeSuffix} />)
      const stop = canFold && !isExpanded ? runStart + MULTILINE_PREVIEW_LINES : runEnd
      for (let j = runStart; j < stop; j++) {
        rendered.push(<YamlLine key={j} tokens={allTokens[j]} />)
      }
      i = runEnd
      continue
    }

    const keyToken = line.find((t) => t.type === 'key')
    const keyName = keyToken && 'value' in keyToken ? keyToken.value : ''
    const isCollapsibleKey = COLLAPSED_BY_DEFAULT_KEYS.has(keyName) && !collapsedKeys.has(keyName)
    rendered.push(
      <YamlLine
        key={i}
        tokens={line}
        suffix={
          isCollapsibleKey ? (
            <span className={foldCls} onClick={() => toggleKey(keyName)}>
              collapse
            </span>
          ) : undefined
        }
      />,
    )
    i++
  }

  return <div className="p-3 font-mono text-xs">{rendered}</div>
}
