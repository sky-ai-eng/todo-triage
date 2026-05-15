import { Navigate, useLocation } from 'react-router-dom'
import { useAuthStatus } from './hooks/useAuthStatus'
import { useDeploymentConfig } from './hooks/useDeploymentConfig'
import { useAuth } from './contexts/AuthContext'
import { useActiveOrgId } from './contexts/OrgContext'

/**
 * AuthGate routes between the existing local-mode keychain setup
 * flow and the multi-mode cookie-session flow.
 *
 *   Local mode → LocalAuthGate (existing /api/integrations/status)
 *   Multi mode → MultiAuthGate (uses AuthContext)
 *
 * Multi-mode states:
 *   loading → spinner
 *   error   → spinner (transient; AuthContext keeps retrying via refresh())
 *   unauth  → redirect to /login?return_to=<current>
 *   authed + 0 orgs   → redirect to /no-orgs
 *   authed + N orgs, URL not under /orgs/:id → redirect to /orgs/<active>
 *   authed + N orgs, URL under /orgs/:id → render children
 *
 * Multi mode also blocks the /setup local-mode keychain wizard —
 * keychain creds aren't a thing in multi mode (they live in Vault
 * per-org via D5).
 */

function Loading() {
  return (
    <div className="min-h-screen bg-surface flex items-center justify-center">
      <p className="text-text-tertiary text-sm">Loading...</p>
    </div>
  )
}

function LocalAuthGate({ children }: { children: React.ReactNode }) {
  const { configured, loading } = useAuthStatus()
  if (loading) return <Loading />
  if (!configured) return <Navigate to="/setup" replace />
  return <>{children}</>
}

function MultiAuthGate({ children }: { children: React.ReactNode }) {
  const auth = useAuth()
  const activeOrgId = useActiveOrgId()
  const location = useLocation()

  if (auth.status === 'loading' || auth.status === 'error') {
    return <Loading />
  }
  if (auth.status === 'unauth') {
    const target = location.pathname + location.search
    const params = target !== '/' ? '?return_to=' + encodeURIComponent(target) : ''
    return <Navigate to={'/login' + params} replace />
  }
  if (auth.orgs.length === 0) {
    return <Navigate to="/no-orgs" replace />
  }
  // Authed + has orgs. The router only mounts MultiAuthGate under
  // /orgs/:org_id/* so by definition we're on an org-scoped path.
  // The activeOrgId might still be null briefly between auth
  // resolution and OrgContext picking — render loading until both
  // line up.
  if (!activeOrgId) {
    return <Loading />
  }
  return <>{children}</>
}

export default function AuthGate({ children }: { children: React.ReactNode }) {
  const { config, loading } = useDeploymentConfig()

  // Boot: we don't know which mode we're in yet. Spinner until
  // /api/config resolves. This is the only request that blocks the
  // app from rendering anything.
  if (loading || !config) return <Loading />

  if (config.deployment_mode === 'multi') {
    return <MultiAuthGate>{children}</MultiAuthGate>
  }
  return <LocalAuthGate>{children}</LocalAuthGate>
}
