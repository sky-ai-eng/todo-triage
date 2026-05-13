import { useState, useEffect } from 'react'
import type { DeploymentConfig, TeamMember, TeamMembersResponse } from '../types'

/** In-flight Promise dedup for /api/config. When multiple
 *  IdentityListField components mount in the same render (e.g. a review
 *  event predicate with both author_in and reviewer_in fields), they
 *  share one round-trip rather than fanning out to identical fetches.
 *  Cleared once the request settles so the next mount re-fetches —
 *  there's deliberately no persistent result cache, because
 *  current_user.github_username can change mid-session (user opens
 *  editor → connects GitHub via Settings → returns), and a cached
 *  value would keep the editor stuck in the "identity not captured"
 *  state until a page reload. The endpoint is one cheap SELECT, so
 *  re-fetching per editor mount is correct and cheap. */
let configInFlight: Promise<DeploymentConfig> | null = null

function loadConfig(): Promise<DeploymentConfig> {
  if (configInFlight) return configInFlight
  configInFlight = fetch('/api/config')
    .then((r) => {
      if (!r.ok) throw new Error(`/api/config: ${r.status}`)
      return r.json() as Promise<DeploymentConfig>
    })
    .finally(() => {
      configInFlight = null
    })
  return configInFlight
}

/** useDeploymentConfig fetches /api/config on every mount with in-flight
 *  dedup. No persistent cache — the response carries
 *  current_user.github_username which is mutable during a session, and
 *  caching it would shadow real changes until a page reload. */
export function useDeploymentConfig(): {
  config: DeploymentConfig | null
  loading: boolean
  error: string | null
} {
  const [config, setConfig] = useState<DeploymentConfig | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadConfig()
      .then((data) => {
        if (!cancelled) {
          setConfig(data)
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { config, loading, error }
}

/** useTeamMembers fetches the roster for the active user's team. Used
 *  by Variant B (multi-select) of the identity-allowlist field. Fetched
 *  fresh on each component mount — the roster is mutable during a
 *  session but cache invalidation isn't worth the websocket plumbing
 *  for v1. The list is usually small (single digits to low tens). */
export function useTeamMembers(): {
  members: TeamMember[]
  loading: boolean
  error: string | null
} {
  const [members, setMembers] = useState<TeamMember[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    fetch('/api/team/members')
      .then((r) => {
        if (!r.ok) throw new Error(`/api/team/members: ${r.status}`)
        return r.json() as Promise<TeamMembersResponse>
      })
      .then((data) => {
        if (!cancelled) {
          setMembers(data.members || [])
          setLoading(false)
        }
      })
      .catch((err) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      })
    return () => {
      cancelled = true
    }
  }, [])

  return { members, loading, error }
}
