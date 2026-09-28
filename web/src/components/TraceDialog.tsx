import { useEffect, useState } from 'react'
import { decodeBase64, fetchTrace, type RequestDetail } from '@/api'
import { assembleSSE } from '@/lib/assemble'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'

// ---------- pretty body rendering ----------

function PrettyBody({ body }: { body: string }) {
  const trimmed = body.trim()
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      return <pre className="p-3 text-xs whitespace-pre-wrap">{JSON.stringify(JSON.parse(trimmed), null, 2)}</pre>
    } catch {
      /* not JSON after all */
    }
  }
  return <pre className="p-3 text-xs whitespace-pre-wrap">{body}</pre>
}

// SSEBody shows an assembled view of the event stream by default, with a
// raw toggle (like CPA's request log).
function SSEBody({ body }: { body: string }) {
  const assembled = assembleSSE(body)
  const [raw, setRaw] = useState(false)
  if (!assembled) {
    return <pre className="p-3 text-xs whitespace-pre-wrap">{body}</pre>
  }
  return (
    <div>
      <div className="flex items-center gap-2 border-b bg-muted/40 px-3 py-1.5">
        <Badge variant="secondary" className="text-xs">
          {assembled.events} events
        </Badge>
        <Badge variant="outline" className="text-xs">
          {assembled.format}
        </Badge>
        <Button size="sm" variant="ghost" className="ml-auto h-6 px-2 text-xs" onClick={() => setRaw(!raw)}>
          {raw ? 'assembled' : 'raw'}
        </Button>
      </div>
      {raw ? (
        <pre className="p-3 text-xs whitespace-pre-wrap">{body}</pre>
      ) : (
        <pre className="p-3 text-xs whitespace-pre-wrap">{JSON.stringify(assembled.message, null, 2)}</pre>
      )}
    </div>
  )
}

// HttpSection is one direction of the exchange: headers verbatim, body
// pretty (JSON / SSE-assembled).
function HttpSection({ title, raw }: { title: string; raw: string }) {
  const [open, setOpen] = useState(true)
  const sep = raw.indexOf('\r\n\r\n')
  const head = sep >= 0 ? raw.slice(0, sep) : raw
  const body = sep >= 0 ? raw.slice(sep + 4) : ''
  const isSSE = body.trimStart().startsWith('event:') || body.trimStart().startsWith('data:')

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
        <div className="max-h-[50vh] overflow-auto border-t">
          <pre className="border-b bg-muted/60 p-3 text-xs whitespace-pre-wrap">{head}</pre>
          {body && (isSSE ? <SSEBody body={body} /> : <PrettyBody body={body} />)}
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
      <DialogContent className="sm:max-w-3xl">
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
