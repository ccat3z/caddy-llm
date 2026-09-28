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
  const [stage, setStage] = useState('')
  const [stages, setStages] = useState<string[]>([])
  const [interval, setInterval_] = useState('5m')
  const [rangeHours, setRangeHours] = useState(1)
  const [refreshKey, setRefreshKey] = useState(0)

  // Discover stage names from the unfiltered trace list.
  useEffect(() => {
    fetchTraces('', 500)
      .then((entries) => {
        const names = [...new Set((entries ?? []).map((e) => e.trace_name))].sort()
        setStages(names)
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
          <Select value={stage || '__all__'} onValueChange={(v) => setStage(!v || v === '__all__' ? '' : v)}>
            <SelectTrigger className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__all__">all stages</SelectItem>
              {stages.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </header>

      <UsageChart
        stage={stage}
        interval={interval}
        rangeHours={rangeHours}
        onInterval={setInterval_}
      />

      <RequestTable stage={stage} refreshKey={refreshKey} />
    </div>
  )
}

export default App
