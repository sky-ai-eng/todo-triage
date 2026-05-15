import { useCallback } from 'react'
import { useDeploymentConfig } from './useDeploymentConfig'
import { useActiveOrgId } from '../contexts/OrgContext'

/**
 * useOrgHref returns a stable function that resolves a relative client
 * path against the deployment mode:
 *
 *   local → relPath unchanged                   '/triage' → '/triage'
 *   multi → '/orgs/<active_org>' + relPath      '/triage' → '/orgs/abc/triage'
 *
 * The helper keeps page-level code mode-agnostic. NavLinks and
 * navigate() calls stay readable:
 *
 *   const orgHref = useOrgHref()
 *   <Link to={orgHref('/triage')}>Triage</Link>
 *
 * Active-org state lives in OrgContext (URL-driven in multi mode,
 * fallback to localStorage). Until both config and an active-org
 * resolve, the helper returns the raw path — fine because routes that
 * use it aren't reachable before AuthGate clears anyway.
 */
export function useOrgHref(): (relPath: string) => string {
  const { config } = useDeploymentConfig()
  const activeOrgId = useActiveOrgId()

  return useCallback(
    (relPath: string): string => {
      if (!config || config.deployment_mode !== 'multi') {
        return relPath
      }
      if (!activeOrgId) {
        return relPath
      }
      const cleaned = relPath.startsWith('/') ? relPath : '/' + relPath
      // The root path '/' maps to '/orgs/<id>' (no trailing slash) so
      // NavLink end-matching still highlights "Factory" at the org
      // root URL.
      if (cleaned === '/') {
        return '/orgs/' + activeOrgId
      }
      return '/orgs/' + activeOrgId + cleaned
    },
    [config, activeOrgId],
  )
}
