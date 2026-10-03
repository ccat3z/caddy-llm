import { useCallback, useEffect, useState } from 'react'
import { fetchTraces, type RequestSummary } from '@/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { TraceDialog } from '@/components/TraceDialog'

const fmtNum = (n?: number) => (n == null ? '—' : n.toLocaleString())

function StatusBadge({ status, state }: { status?: number; state?: string }) {
  if (state === 'in_progress') {
    return <Badge className="border-sky-500/50 bg-sky-500/10 text-sky-600">in progress</Badge>
  }
  if (state === 'crashed') {
    return <Badge variant="outline" className="border-red-500/50 text-red-600">crashed</Badge>
  }
  if (!status) return <Badge variant="outline">—</Badge>
  const cls =
    status < 300
      ? 'border-emerald-500/50 text-emerald-600'
      : status < 500
        ? 'border-amber-500/50 text-amber-600'
        : 'border-red-500/50 text-red-600'
  return (
    <Badge variant="outline" className={cls}>
      {status}
    </Badge>
  )
}

// Adaptive duration: ms under a second, fractional seconds under a minute,
// m+s beyond — so hour-long streams don't render as a wall of milliseconds.
const fmtMS = (ms: number) => {
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`
  const m = Math.floor(s / 60)
  return `${m}m${Math.round(s - m * 60)}s`
}

const fmtDuration = (e: RequestSummary) => {
  if (e.state === 'in_progress') {
    // In-flight: show live elapsed instead of the (unset) final duration.
    return fmtMS(Date.now() - new Date(e.timestamp).getTime())
  }
  return e.duration_ms != null ? fmtMS(e.duration_ms) : '—'
}

const fmtTime = (ts: string) => {
  const d = new Date(ts)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(
    d.getSeconds(),
  ).padStart(2, '0')}`
}

const PAGE_SIZE = 20

export function RequestTable({ traceName, refreshKey }: { traceName: string; refreshKey: number }) {
  const [entries, setEntries] = useState<RequestSummary[]>([])
  const [hasMore, setHasMore] = useState(false)
  const [page, setPage] = useState(0)
  const [error, setError] = useState('')
  const [selected, setSelected] = useState<{ traceID: string; name: string } | null>(null)

  // Back to page 1 when the filter changes.
  useEffect(() => setPage(0), [traceName])

  const load = useCallback(() => {
    // Fetch one extra row to know whether a next page exists (the API has
    // no total count).
    fetchTraces(traceName, PAGE_SIZE + 1, page * PAGE_SIZE)
      .then((rows) => {
        const e = rows ?? []
        setHasMore(e.length > PAGE_SIZE)
        setEntries(e.slice(0, PAGE_SIZE))
        setError('')
      })
      .catch((e) => setError(String(e)))
  }, [traceName, page])

  useEffect(() => {
    load()
  }, [load, refreshKey])

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <CardTitle className="text-base font-medium">Requests</CardTitle>
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Button size="sm" variant="outline" disabled={page === 0} onClick={() => setPage(page - 1)}>
            Prev
          </Button>
          <span className="min-w-16 text-center">Page {page + 1}</span>
          <Button size="sm" variant="outline" disabled={!hasMore} onClick={() => setPage(page + 1)}>
            Next
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {error && <p className="text-sm text-destructive">{error}</p>}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-24">Time</TableHead>
              <TableHead className="w-28">Trace Name</TableHead>
              <TableHead>Model</TableHead>
              <TableHead className="w-20">Status</TableHead>
              <TableHead className="w-20 text-right">Duration</TableHead>
              <TableHead className="text-right">Input Tokens</TableHead>
              <TableHead className="text-right">Cache Tokens</TableHead>
              <TableHead className="text-right">Output Tokens</TableHead>
              <TableHead className="text-right">Req/Resp</TableHead>
              <TableHead className="w-32">Trace</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((e) => {
              const key = `${e.trace_id}/${e.trace_name}`
              return (
                <TableRow
                  key={key}
                  className="cursor-pointer"
                  onClick={() => setSelected({ traceID: e.trace_id, name: e.trace_name })}
                >
                  <TableCell className="font-mono text-xs">{fmtTime(e.timestamp)}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">{e.trace_name}</Badge>
                  </TableCell>
                  <TableCell className="max-w-48 truncate font-mono text-xs" title={e.model}>
                    {e.model || '—'}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={e.status} state={e.state} />
                  </TableCell>
                  <TableCell className="text-right text-xs">{fmtDuration(e)}</TableCell>
                  <TableCell className="text-right font-mono text-xs">{fmtNum(e.input_tokens)}</TableCell>
                  <TableCell className="text-right font-mono text-xs">{fmtNum(e.cache_tokens)}</TableCell>
                  <TableCell className="text-right font-mono text-xs">{fmtNum(e.output_tokens)}</TableCell>
                  <TableCell className="text-right font-mono text-xs">
                    {e.req_bytes}/{e.resp_bytes}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {e.trace_id.slice(0, 12)}
                  </TableCell>
                </TableRow>
              )
            })}
            {entries.length === 0 && !error && (
              <TableRow>
                <TableCell colSpan={10} className="py-10 text-center text-sm text-muted-foreground">
                  No requests recorded yet
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </CardContent>
      <TraceDialog detail={selected} onClose={() => setSelected(null)} />
    </Card>
  )
}
