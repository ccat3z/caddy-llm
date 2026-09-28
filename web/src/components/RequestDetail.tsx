import { useEffect, useState } from 'react'
import { decodeBase64, fetchTrace } from '@/api'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

// HttpMessageView renders one raw replayable HTTP message: the head
// (request/status line + headers) verbatim, the body pretty-printed when it
// is JSON and grouped by event when it is an SSE stream.
function HttpMessageView({ raw }: { raw: string }) {
  if (!raw) {
    return <p className="p-3 text-sm text-muted-foreground">(empty)</p>
  }
  const sep = raw.indexOf('\r\n\r\n')
  const head = sep >= 0 ? raw.slice(0, sep) : raw
  const body = sep >= 0 ? raw.slice(sep + 4) : ''

  return (
    <div className="max-h-96 overflow-auto rounded-md border bg-muted/40 text-xs">
      <pre className="border-b bg-muted/60 p-3 whitespace-pre-wrap">{head}</pre>
      {body && <BodyView body={body} />}
    </div>
  )
}

function BodyView({ body }: { body: string }) {
  // JSON body: pretty-print.
  const trimmed = body.trim()
  if (trimmed.startsWith('{')) {
    try {
      return (
        <pre className="p-3 whitespace-pre-wrap">{JSON.stringify(JSON.parse(trimmed), null, 2)}</pre>
      )
    } catch {
      /* fall through to raw */
    }
  }
  // SSE stream: one event per block, data payloads kept raw (they can be
  // huge) but each on its own lines.
  if (trimmed.startsWith('event:') || trimmed.startsWith('data:')) {
    return (
      <div className="p-3 font-mono">
        {trimmed.split(/\n\n+/).map((ev, i) => (
          <div key={i} className="mb-2 border-b pb-2 last:border-0 whitespace-pre-wrap">
            {ev}
          </div>
        ))}
      </div>
    )
  }
  return <pre className="p-3 whitespace-pre-wrap">{body}</pre>
}

export function RequestDetail({ traceID, name }: { traceID: string; name: string }) {
  const [req, setReq] = useState('')
  const [resp, setResp] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    fetchTrace(traceID, name)
      .then((d) => {
        setReq(decodeBase64(d.request_raw))
        setResp(decodeBase64(d.response_raw))
      })
      .catch((e) => setError(String(e)))
  }, [traceID, name])

  if (error) return <p className="p-3 text-sm text-destructive">{error}</p>

  return (
    <Tabs defaultValue="request" className="w-full">
      <TabsList>
        <TabsTrigger value="request">Request</TabsTrigger>
        <TabsTrigger value="response">Response</TabsTrigger>
      </TabsList>
      <TabsContent value="request">
        <HttpMessageView raw={req} />
      </TabsContent>
      <TabsContent value="response">
        <HttpMessageView raw={resp} />
      </TabsContent>
    </Tabs>
  )
}
