import { useEffect, useState } from 'react'
import { fetchTraces } from '@/api'
import logo from '@/assets/logo.svg'
import { RangeButtons, UsageChart } from '@/components/UsageChart'
import { RequestTable } from '@/components/RequestTable'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

// Interval defaults to duration / 12 (seconds) and resets whenever the
// duration changes.
const defaultInterval = (rangeHours: number) => (rangeHours * 3600) / 12

function App() {
  const [traceName, setTraceName] = useState('')
  const [names, setNames] = useState<string[]>([])
  const [rangeHours, setRangeHours] = useState(24)
  const [interval, setInterval_] = useState(defaultInterval(24))
  const [refreshKey, setRefreshKey] = useState(0)

  // Discover trace names from the unfiltered trace list.
  useEffect(() => {
    fetchTraces('', 500)
      .then((entries) => {
        const found = [...new Set((entries ?? []).map((e) => e.trace_name))].sort()
        setNames(found)
      })
      .catch(() => {})
  }, [refreshKey])

  // Light auto-refresh while the page is open.
  useEffect(() => {
    const t = setInterval(() => setRefreshKey((k) => k + 1), 15_000)
    return () => clearInterval(t)
  }, [])

  const onRange = (h: number) => {
    setRangeHours(h)
    setInterval_(defaultInterval(h))
  }

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-6">
      <header className="flex items-center justify-between">
        <h1 className="flex items-center gap-2 text-lg font-semibold">
          <img src={logo} alt="" className="h-7 w-7 rounded" />
          caddy-llm traces
        </h1>
        <div className="flex items-center gap-3">
          <RangeButtons rangeHours={rangeHours} onRange={onRange} />
          <Select
            value={traceName || '__all__'}
            onValueChange={(v) => setTraceName(!v || v === '__all__' ? '' : v)}
          >
            <SelectTrigger className="w-32">
              <SelectValue>{traceName === '' ? '*' : traceName}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__all__">*</SelectItem>
              {names.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </header>

      <UsageChart
        traceName={traceName}
        interval={interval}
        rangeHours={rangeHours}
        refreshKey={refreshKey}
        onInterval={setInterval_}
      />

      <RequestTable traceName={traceName} refreshKey={refreshKey} />
    </div>
  )
}

export default App
