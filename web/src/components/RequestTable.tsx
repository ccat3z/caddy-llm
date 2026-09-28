import { useCallback, useEffect, useState } from 'react'
import { fetchTraces, type RequestSummary } from '@/api'
import { Badge } from '@/components/ui/badge'
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

function StatusBadge({ status }: { status?: number }) {
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

const fmtTime = (ts: string) => {
  const d = new Date(ts)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(
    d.getSeconds(),
  ).padStart(2, '0')}`
}

export function RequestTable({ traceName, refreshKey }: { traceName: string; refreshKey: number }) {
  const [entries, setEntries] = useState<RequestSummary[]>([])
  const [error, setError] = useState('')
  const [selected, setSelected] = useState<{ traceID: string; name: string } | null>(null)

  const load = useCallback(() => {
    fetchTraces(traceName)
      .then((e) => {
        setEntries(e ?? [])
        setError('')
      })
      .catch((e) => setError(String(e)))
  }, [traceName])

  useEffect(() => {
    load()
  }, [load, refreshKey])

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-base font-medium">Requests</CardTitle>
      </CardHeader>
      <CardContent>
        {error && <p className="text-sm text-destructive">{error}</p>}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-24">Time</TableHead>
              <TableHead className="w-28">Trace Name</TableHead>
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
                  <TableCell>
                    <StatusBadge status={e.status} />
                  </TableCell>
                  <TableCell className="text-right text-xs">
                    {e.duration_ms != null ? `${e.duration_ms}ms` : '—'}
                  </TableCell>
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
                <TableCell colSpan={9} className="py-10 text-center text-sm text-muted-foreground">
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
