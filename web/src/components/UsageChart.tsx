import { useEffect, useMemo, useState } from 'react'
import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { fetchUsage, type UsageBucket } from '@/api'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Button } from '@/components/ui/button'

export interface ChartControls {
  stage: string
  interval: string
  rangeHours: number
}

interface Props {
  stage: string
  interval: string
  rangeHours: number
  onInterval: (v: string) => void
}

const INTERVALS: { value: string; label: string }[] = [
  { value: '1m', label: '1 min' },
  { value: '5m', label: '5 min' },
  { value: '15m', label: '15 min' },
  { value: '30m', label: '30 min' },
  { value: '1h', label: '1 hour' },
  { value: '6h', label: '6 hours' },
  { value: '24h', label: '1 day' },
]

const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(1)}M` : n >= 1000 ? `${(n / 1000).toFixed(1)}k` : `${n}`

const bucketLabel = (ts: string, interval: string) => {
  const d = new Date(ts)
  const day = `${d.getMonth() + 1}/${d.getDate()}`
  const hm = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  // Long intervals get a day label; short ones just the time.
  return interval.endsWith('h') || interval === '24h' ? `${day} ${hm}` : hm
}

export function UsageChart({ stage, interval, rangeHours, onInterval }: Props) {
  const [buckets, setBuckets] = useState<UsageBucket[]>([])
  const [error, setError] = useState<string>('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    fetchUsage({
      interval,
      stage: stage || undefined,
      from: new Date(Date.now() - rangeHours * 3600_000),
    })
      .then((b) => {
        if (!cancelled) {
          setBuckets(b ?? [])
          setError('')
        }
      })
      .catch((e) => !cancelled && setError(String(e)))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
  }, [stage, interval, rangeHours])

  const data = useMemo(
    () =>
      buckets.map((b) => ({
        ...b,
        label: bucketLabel(b.timestamp, interval),
      })),
    [buckets, interval],
  )

  const totals = useMemo(
    () =>
      buckets.reduce(
        (acc, b) => ({
          input: acc.input + b.input_tokens,
          cache: acc.cache + b.cache_tokens,
          output: acc.output + b.output_tokens,
        }),
        { input: 0, cache: 0, output: 0 },
      ),
    [buckets],
  )

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <CardTitle className="text-base font-medium">
          Token usage
          <span className="ml-3 text-sm font-normal text-muted-foreground">
            in {fmtTokens(totals.input)} · cache {fmtTokens(totals.cache)} · out{' '}
            {fmtTokens(totals.output)}
          </span>
        </CardTitle>
        <Select value={interval} onValueChange={(v) => v && onInterval(v)}>
          <SelectTrigger className="w-28">
            <SelectValue>{INTERVALS.find((i) => i.value === interval)?.label}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {INTERVALS.map((i) => (
              <SelectItem key={i.value} value={i.value}>
                {i.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </CardHeader>
      <CardContent>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {!error && data.length === 0 && (
          <p className="py-16 text-center text-sm text-muted-foreground">
            {loading ? 'Loading…' : 'No usage in this range'}
          </p>
        )}
        {data.length > 0 && (
          <ResponsiveContainer width="100%" height={260}>
            <BarChart data={data} margin={{ top: 8, right: 8, left: 8, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" vertical={false} />
              <XAxis dataKey="label" tickLine={false} fontSize={11} />
              <YAxis tickFormatter={fmtTokens} tickLine={false} axisLine={false} fontSize={11} width={48} />
              <Tooltip
                formatter={(value, name) => [fmtTokens(Number(value ?? 0)), String(name)]}
                labelFormatter={(label) => `bucket: ${label}`}
              />
              <Bar dataKey="input_tokens" name="input" stackId="t" fill="#0ea5e9" />
              <Bar dataKey="cache_tokens" name="cache" stackId="t" fill="#f59e0b" />
              <Bar dataKey="output_tokens" name="output" stackId="t" fill="#10b981" radius={[3, 3, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  )
}

export function RangeButtons({
  rangeHours,
  onRange,
}: {
  rangeHours: number
  onRange: (h: number) => void
}) {
  const ranges = [
    { h: 1, label: '1h' },
    { h: 6, label: '6h' },
    { h: 24, label: '24h' },
    { h: 24 * 7, label: '7d' },
  ]
  return (
    <div className="flex gap-1">
      {ranges.map((r) => (
        <Button
          key={r.h}
          size="sm"
          variant={rangeHours === r.h ? 'default' : 'outline'}
          onClick={() => onRange(r.h)}
        >
          {r.label}
        </Button>
      ))}
    </div>
  )
}
