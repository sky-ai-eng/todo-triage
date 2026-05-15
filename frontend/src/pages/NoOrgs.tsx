import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../contexts/AuthContext'

/**
 * Displayed when a user successfully authenticates but isn't a
 * member of any organization. Self-serve org creation isn't in v1
 * scope — orgs are admin-provisioned via the D14 admin UI — so the
 * only action available here is to log out and try a different
 * account.
 */
export default function NoOrgs() {
  const auth = useAuth()
  const navigate = useNavigate()

  // Redirect to /login once logout flips auth to unauth. NoOrgs is
  // outside AuthGate so nothing else observes this state transition.
  useEffect(() => {
    if (auth.status === 'unauth') {
      navigate('/login', { replace: true })
    }
  }, [auth.status, navigate])

  // When an admin adds this user to an org after they landed here,
  // auth.refresh() will update the orgs list and this effect redirects
  // them in-place without a full page reload.
  useEffect(() => {
    if (auth.status === 'authed' && auth.orgs.length > 0) {
      navigate('/orgs/' + auth.orgs[0].id, { replace: true })
    }
  }, [auth.status, auth.orgs, navigate])

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <div className="w-full max-w-md backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Awaiting invitation
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            You&apos;re signed in as{' '}
            <span className="text-text-secondary font-medium">
              {auth.user?.display_name || auth.user?.email || 'this account'}
            </span>
            , but you&apos;re not yet a member of any organization on this deployment.
          </p>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            Ask your organization admin to invite this account, then refresh.
          </p>
        </div>

        <div className="flex gap-3">
          <button
            type="button"
            onClick={() => void auth.refresh()}
            className="flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Refresh
          </button>
          <button
            type="button"
            onClick={() => void auth.logout()}
            className="flex-1 bg-accent hover:bg-accent/90 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            Log out
          </button>
        </div>
      </div>
    </div>
  )
}
