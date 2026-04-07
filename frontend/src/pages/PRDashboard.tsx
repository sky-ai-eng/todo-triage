import { useState, useEffect, useCallback } from 'react'
import PRCard from '../components/PRCard'

export interface PRSummary {
  number: number
  title: string
  repo: string
  author: string
  state: string
  draft: boolean
  labels: string[]
  created_at: string
  updated_at: string
  html_url: string
}

export default function PRDashboard() {
  const [prs, setPrs] = useState<PRSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [lastRefresh, setLastRefresh] = useState(Date.now())

  const fetchPRs = useCallback(async () => {
    setLoading(true)
    try {
      const res = await fetch('/api/dashboard/prs')
      if (res.ok) {
        setPrs(await res.json())
      }
    } finally {
      setLoading(false)
      setLastRefresh(Date.now())
    }
  }, [])

  useEffect(() => {
    fetchPRs()
  }, [fetchPRs])

  // Auto-refresh every 2 minutes while visible
  useEffect(() => {
    const interval = setInterval(fetchPRs, 120000)
    const handleVisibility = () => {
      if (document.visibilityState === 'visible') fetchPRs()
    }
    document.addEventListener('visibilitychange', handleVisibility)
    return () => {
      clearInterval(interval)
      document.removeEventListener('visibilitychange', handleVisibility)
    }
  }, [fetchPRs])

  const draftPRs = prs.filter((pr) => pr.draft)
  const readyPRs = prs.filter((pr) => !pr.draft)

  return (
    <div className="max-w-4xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-xl font-semibold text-text-primary">Your Pull Requests</h1>
        <div className="flex items-center gap-3">
          <span className="text-[11px] text-text-tertiary">
            {formatTimeSince(lastRefresh)}
          </span>
          <button
            onClick={fetchPRs}
            disabled={loading}
            className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors disabled:opacity-50"
          >
            {loading ? 'Refreshing...' : 'Refresh'}
          </button>
        </div>
      </div>

      {prs.length === 0 && !loading && (
        <div className="flex flex-col items-center justify-center py-20">
          <p className="text-text-tertiary text-sm">No open pull requests</p>
        </div>
      )}

      {readyPRs.length > 0 && (
        <section className="mb-8">
          <h2 className="text-[13px] font-medium text-text-secondary mb-3 px-1">
            Ready for review
            <span className="ml-2 text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5 text-[11px]">
              {readyPRs.length}
            </span>
          </h2>
          <div className="space-y-3">
            {readyPRs.map((pr) => (
              <PRCard key={`${pr.repo}-${pr.number}`} pr={pr} />
            ))}
          </div>
        </section>
      )}

      {draftPRs.length > 0 && (
        <section>
          <h2 className="text-[13px] font-medium text-text-secondary mb-3 px-1">
            Drafts
            <span className="ml-2 text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5 text-[11px]">
              {draftPRs.length}
            </span>
          </h2>
          <div className="space-y-3">
            {draftPRs.map((pr) => (
              <PRCard key={`${pr.repo}-${pr.number}`} pr={pr} />
            ))}
          </div>
        </section>
      )}
    </div>
  )
}

function formatTimeSince(ts: number): string {
  const diff = Math.floor((Date.now() - ts) / 1000)
  if (diff < 5) return 'just now'
  if (diff < 60) return `${diff}s ago`
  return `${Math.floor(diff / 60)}m ago`
}
