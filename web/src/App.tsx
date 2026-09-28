import { useEffect, useState } from 'react'
import { fetchTraces } from '@/api'
import { RangeButtons, UsageChart } from '@/components/UsageChart'
import { RequestTable } from '@/components/RequestTable'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

function App() {
  const [traceName, setTraceName] = useState('')
  const [names, setNames] = useState<string[]>([])
  const [interval, setInterval_] = useState('5m')
  const [rangeHours, setRangeHours] = useState(1)
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

  return (
    <div className="mx-auto max-w-6xl space-y-4 p-6">
      <header className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">caddy-llm traces</h1>
        <div className="flex items-center gap-3">
          <RangeButtons rangeHours={rangeHours} onRange={setRangeHours} />
          <Select
            value={traceName || '__all__'}
            onValueChange={(v) => setTraceName(!v || v === '__all__' ? '' : v)}
          >
            <SelectTrigger className="w-32">
              <SelectValue>{traceName === '' ? 'all names' : traceName}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__all__">all names</SelectItem>
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
        onInterval={setInterval_}
      />

      <RequestTable traceName={traceName} refreshKey={refreshKey} />
    </div>
  )
}

export default App
