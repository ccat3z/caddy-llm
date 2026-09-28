import { useEffect, useState } from 'react'
import { decodeBase64, fetchTrace, type RequestDetail } from '@/api'
import { assembleSSE } from '@/lib/assemble'
import { YamlBlock } from '@/lib/yaml'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// ---------- pretty body rendering ----------

// BodyView renders a message body pretty (YAML-ish) by default with a
// pretty/raw toggle: SSE streams are first assembled into one message,
// JSON bodies rendered directly. Bodies that are neither show raw only.
function BodyView({ body }: { body: string }) {
  const trimmed = body.trim()
  const isSSE = trimmed.startsWith('event:') || trimmed.startsWith('data:')
  const assembled = isSSE ? assembleSSE(trimmed) : null

  let pretty: unknown = null
  if (assembled) {
    pretty = assembled.message
  } else if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      pretty = JSON.parse(trimmed)
    } catch {
      /* not JSON after all */
    }
  }

  const [raw, setRaw] = useState(false)
  if (pretty === null) {
    return <pre className="p-3 text-xs whitespace-pre-wrap">{body}</pre>
  }
  const seg = (active: boolean) =>
    `px-2 py-0.5 ${active ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted'}`
  return (
    <div>
      <div className="flex items-center gap-2 border-b bg-muted/40 px-3 py-1.5">
        {assembled && (
          <>
            <Badge variant="secondary" className="text-xs">
              {assembled.events} events
            </Badge>
            <Badge variant="outline" className="text-xs">
              {assembled.format}
            </Badge>
          </>
        )}
        <div className="ml-auto flex overflow-hidden rounded-md border text-xs">
          <button className={seg(!raw)} onClick={() => setRaw(false)}>
            pretty
          </button>
          <button className={seg(raw)} onClick={() => setRaw(true)}>
            raw
          </button>
        </div>
      </div>
      {raw ? (
        <pre className="p-3 text-xs whitespace-pre-wrap">{body}</pre>
      ) : (
        <YamlBlock value={pretty} />
      )}
    </div>
  )
}

// HttpSection is one direction of the exchange: headers verbatim, body
// pretty-rendered (YAML-ish) with a raw toggle.
function HttpSection({ title, raw }: { title: string; raw: string }) {
  const [open, setOpen] = useState(true)
  const sep = raw.indexOf('\r\n\r\n')
  const head = sep >= 0 ? raw.slice(0, sep) : raw
  const body = sep >= 0 ? raw.slice(sep + 4) : ''

  return (
    <div className="rounded-md border">
      <button
        className="flex w-full items-center justify-between px-3 py-2 text-left text-sm font-medium hover:bg-muted/50"
        onClick={() => setOpen(!open)}
      >
        <span>
          {open ? '▾' : '▸'} {title}
        </span>
        <span className="text-xs font-normal text-muted-foreground">{raw.length} B</span>
      </button>
      {open && (
        <div className="border-t">
          {/* Head and body scroll independently: long header blocks must
              not squeeze the body out of the section. */}
          <pre className="max-h-40 overflow-auto border-b bg-muted/60 p-3 text-xs whitespace-pre-wrap">{head}</pre>
          <div className="max-h-[45vh] overflow-auto">{body && <BodyView body={body} />}</div>
        </div>
      )}
    </div>
  )
}

// ---------- the dialog ----------

export function TraceDialog({
  detail,
  onClose,
}: {
  detail: { traceID: string; name: string } | null
  onClose: () => void
}) {
  const [data, setData] = useState<RequestDetail | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    setData(null)
    setError('')
    if (detail) {
      fetchTrace(detail.traceID, detail.name)
        .then(setData)
        .catch((e) => setError(String(e)))
    }
  }, [detail])

  return (
    <Dialog open={detail !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle className="font-mono text-sm">
            {detail ? `${detail.traceID} / ${detail.name}` : ''}
          </DialogTitle>
        </DialogHeader>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {!data && !error && <p className="text-sm text-muted-foreground">Loading…</p>}
        {data && (
          <div className="space-y-2 overflow-auto">
            <HttpSection title="REQUEST" raw={decodeBase64(data.request_raw)} />
            <HttpSection title="RESPONSE" raw={decodeBase64(data.response_raw)} />
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
