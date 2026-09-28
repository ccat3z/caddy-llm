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

interface Props {
  traceName: string
  interval: number // seconds
  rangeHours: number
  refreshKey: number
  onInterval: (sec: number) => void
}

// Base interval options (seconds); the duration-derived default is merged
// in when it isn't one of these.
const INTERVALS: number[] = [60, 300, 900, 1800, 3600, 7200, 21600, 43200, 86400]

const fmtInterval = (sec: number) => {
  if (sec < 3600) return `${Math.round(sec / 60)} min`
  if (sec < 86400) return `${Math.round(sec / 3600)} hours`
  return `${Math.round(sec / 86400)} day`
}

const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${(n / 1_000_000).toFixed(1)}M` : n >= 1000 ? `${(n / 1000).toFixed(1)}k` : `${n}`

const pad = (n: number) => String(n).padStart(2, '0')
const hm = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`
const md = (d: Date) => `${d.getMonth() + 1}/${d.getDate()}`

// Bucket label: start–end time of the bucket.
function bucketLabel(startMs: number, intervalSec: number): string {
  const s = new Date(startMs)
  const e = new Date(startMs + intervalSec * 1000)
  if (intervalSec >= 86400) return `${md(s)} ${hm(s)} – ${md(e)} ${hm(e)}`
  if (s.getDate() !== e.getDate()) return `${md(s)} ${hm(s)} – ${md(e)} ${hm(e)}`
  return `${hm(s)} – ${hm(e)}`
}

// BucketTick renders the start–end range on two lines so ticks stay
// horizontal (no rotation, no clipping) even when dense. recharts passes
// the tick's x/y and the category value in payload.
function BucketTick(props: { x: number; y: number; payload: { value: number }; interval: number }) {
  const { x, y, payload, interval } = props
  const s = new Date(payload.value)
  const e = new Date(payload.value + interval * 1000)
  const sameDay = s.getDate() === e.getDate() && interval < 86400
  const [l1, l2] = sameDay
    ? [hm(s), `– ${hm(e)}`]
    : [`${md(s)} ${hm(s)}`, `– ${md(e)} ${hm(e)}`]
  return (
    <g transform={`translate(${x},${y})`}>
      <text textAnchor="middle" fontSize={10} fill="currentColor">
        <tspan x="0" dy="10">
          {l1}
        </tspan>
        <tspan x="0" dy="13">
          {l2}
        </tspan>
      </text>
    </g>
  )
}

export function UsageChart({ traceName, interval, rangeHours, refreshKey, onInterval }: Props) {
  const [buckets, setBuckets] = useState<UsageBucket[]>([])
  const [error, setError] = useState<string>('')
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    fetchUsage({
      interval,
      traceName: traceName || undefined,
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
  }, [traceName, interval, rangeHours, refreshKey])

  // Fill empty buckets: the API omits them, the chart shows every slot in
  // the range (bucket edges are interval-aligned to the unix epoch, same as
  // the SQL bucketing). The category key is the bucket epoch — NOT the
  // formatted label: labels repeat across days ("20:00 – 22:00" twice in a
  // 24h view), and recharts then mixes up bar/tooltip data.
  const data = useMemo(() => {
    const ivMs = interval * 1000
    const to = Date.now()
    const from = to - rangeHours * 3600_000
    const byStart = new Map(buckets.map((b) => [new Date(b.timestamp).getTime(), b]))
    const out = []
    for (let t = Math.floor(from / ivMs) * ivMs; t < to; t += ivMs) {
      const b = byStart.get(t)
      out.push({
        t,
        input_tokens: b?.input_tokens ?? 0,
        cache_tokens: b?.cache_tokens ?? 0,
        output_tokens: b?.output_tokens ?? 0,
      })
    }
    return out
  }, [buckets, interval, rangeHours])

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

  const options = useMemo(
    () => [...new Set([...INTERVALS, interval])].sort((a, b) => a - b),
    [interval],
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
        <Select value={String(interval)} onValueChange={(v) => v && onInterval(Number(v))}>
          <SelectTrigger className="w-28">
            <SelectValue>{fmtInterval(interval)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {options.map((sec) => (
              <SelectItem key={sec} value={String(sec)}>
                {fmtInterval(sec)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </CardHeader>
      <CardContent>
        {error && <p className="text-sm text-destructive">{error}</p>}
        {!error && buckets.length === 0 && (
          <p className="py-16 text-center text-sm text-muted-foreground">
            {loading ? 'Loading…' : 'No usage in this range'}
          </p>
        )}
        {!error && buckets.length > 0 && (
          <ResponsiveContainer width="100%" height={260}>
            <BarChart data={data} margin={{ top: 8, right: 8, left: 8, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" vertical={false} />
              {/* interval=0: never auto-hide ticks; the two-line custom
                  tick keeps even dense series horizontal. */}
              <XAxis
                dataKey="t"
                interval={0}
                tickLine={false}
                height={44}
                tick={(props) => (
                  <BucketTick
                    x={props.x as number}
                    y={props.y as number}
                    payload={props.payload as { value: number }}
                    interval={interval}
                  />
                )}
              />
              <YAxis tickFormatter={fmtTokens} tickLine={false} axisLine={false} fontSize={11} width={48} />
              <Tooltip
                formatter={(value, name) => [fmtTokens(Number(value ?? 0)), String(name)]}
                labelFormatter={(t) => bucketLabel(Number(t), interval)}
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
