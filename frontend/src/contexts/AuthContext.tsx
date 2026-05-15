/* eslint-disable react-refresh/only-export-components --
   This file is a context+hooks pair. The Provider component and the
   useAuth / useOptionalAuth hooks belong together — splitting them
   into separate files would just trade one set of imports for
   another. The HMR boundary trade-off is acceptable for stable
   plumbing. */
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { apiFetch, apiJSON, HttpError, setUnauthHandler } from '../lib/apiClient'

/**
 * AuthContext owns the multi-mode session state machine.
 *
 * State transitions:
 *   loading  → authed   (GET /api/me returned 200 with body)
 *   loading  → unauth   (GET /api/me returned 401)
 *   loading  → error    (network failure or non-401 5xx)
 *   authed   → unauth   (401 from any subsequent API call via setUnauthHandler)
 *   authed   → unauth   (logout completed)
 *
 * Only mounted in multi mode (see main.tsx). Local mode never
 * constructs this provider — the existing keychain AuthGate handles
 * its own flow.
 *
 * 401 funneling: AuthContext registers a global handler on apiClient
 * so any 401 from any endpoint flips state to 'unauth' and lets the
 * router redirect to /login. Avoids each consumer having to wire its
 * own 401 → reauth logic.
 */

export interface AuthUser {
  id: string
  email: string
  display_name?: string
  avatar_url?: string
  github_username?: string
}

export interface AuthOrg {
  id: string
  name: string
  role: string
}

export interface MeResponse extends AuthUser {
  orgs: AuthOrg[]
}

export type AuthStatus = 'loading' | 'authed' | 'unauth' | 'error'

interface AuthContextValue {
  status: AuthStatus
  user: AuthUser | null
  orgs: AuthOrg[]
  error: string | null
  refresh: () => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>('loading')
  const [user, setUser] = useState<AuthUser | null>(null)
  const [orgs, setOrgs] = useState<AuthOrg[]>([])
  const [error, setError] = useState<string | null>(null)

  // statusRef tracks the latest status so the 401 handler (a closure
  // captured at mount) can branch on it without re-binding every
  // render. Without this, an unauth flip-while-already-unauth would
  // log a redundant transition.
  const statusRef = useRef<AuthStatus>('loading')
  useEffect(() => {
    statusRef.current = status
  }, [status])

  const refresh = useCallback(async () => {
    try {
      const data = await apiJSON<MeResponse>('/api/me')
      const { orgs: orgList, ...userFields } = data
      setUser(userFields as AuthUser)
      setOrgs(orgList ?? [])
      setError(null)
      setStatus('authed')
    } catch (err) {
      if (err instanceof HttpError && err.status === 401) {
        setUser(null)
        setOrgs([])
        setError(null)
        setStatus('unauth')
        return
      }
      // Network failure or unexpected status — surface as 'error'
      // rather than 'unauth' so the UI can distinguish transient
      // server issues from genuine not-logged-in state.
      setUser(null)
      setOrgs([])
      setError(err instanceof Error ? err.message : String(err))
      setStatus('error')
    }
  }, [])

  const logout = useCallback(async () => {
    try {
      await apiFetch('/api/auth/logout', { method: 'POST' })
    } catch {
      // Best-effort. Even if the server call fails, the user clicked
      // logout — we should reflect that locally. The cookie may
      // persist; the next request will surface the failure.
    }
    setUser(null)
    setOrgs([])
    setError(null)
    setStatus('unauth')
  }, [])

  // Initial /api/me fetch on mount. refresh() handles its own
  // setState calls; the effect just kicks the first call. The lint
  // rule is meant to catch render-loop setState; an event-driven
  // fetch-on-mount is the canonical safe pattern.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh()
  }, [refresh])

  // Register the apiClient 401 hook. A 401 from any API call flips
  // us to 'unauth' so the router can redirect. statusRef.current
  // guards against redundant transitions when we're already unauth.
  useEffect(() => {
    setUnauthHandler(() => {
      if (statusRef.current === 'unauth') return
      setUser(null)
      setOrgs([])
      setStatus('unauth')
    })
    return () => setUnauthHandler(null)
  }, [])

  const value = useMemo<AuthContextValue>(
    () => ({ status, user, orgs, error, refresh, logout }),
    [status, user, orgs, error, refresh, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const v = useContext(AuthContext)
  if (!v) {
    throw new Error('useAuth called outside AuthProvider (local mode does not mount it)')
  }
  return v
}

/** useOptionalAuth returns null when no AuthProvider is mounted.
 *  Components rendered in both modes (e.g. Shell) use this to branch
 *  on whether multi-mode auth state is available. */
export function useOptionalAuth(): AuthContextValue | null {
  return useContext(AuthContext)
}
