import { useEffect, useRef, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { ChevronDown, Check } from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import { useOrgContext } from '../contexts/OrgContext'

/**
 * Topbar dropdown for switching between orgs the user belongs to.
 * Mounted only in multi mode (Shell decides). When the user picks a
 * new org, we both update OrgContext and rewrite the URL so deep
 * links to the same sub-path inside the new org work — e.g. switching
 * from /orgs/A/triage to /orgs/B/triage preserves the /triage tail.
 */
export default function OrgPicker() {
  const auth = useAuth()
  const { activeOrgId, setActiveOrgId } = useOrgContext()
  const navigate = useNavigate()
  const location = useLocation()
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    function onClick(e: MouseEvent) {
      if (!rootRef.current?.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  if (auth.orgs.length === 0) return null

  const active = auth.orgs.find((o) => o.id === activeOrgId) ?? auth.orgs[0]

  const handlePick = (newOrgId: string) => {
    setOpen(false)
    if (newOrgId === activeOrgId) return
    setActiveOrgId(newOrgId)
    // Rewrite the URL: swap the org_id segment, preserve everything
    // after it. Routes that don't start with /orgs/<id> fall back to
    // the new org's root.
    const swapped = location.pathname.match(/^\/orgs\/[^/]+(\/.*)?$/)
    const tail = swapped?.[1] ?? ''
    navigate('/orgs/' + newOrgId + tail, { replace: false })
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 text-[13px] font-medium px-3 py-1.5 rounded-full text-text-secondary hover:text-text-primary hover:bg-black/[0.03] transition-colors"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span className="truncate max-w-[160px]">{active.name}</span>
        <ChevronDown size={14} className="text-text-tertiary" />
      </button>

      {open && (
        <div
          role="listbox"
          className="absolute left-0 top-full mt-1.5 min-w-[200px] backdrop-blur-xl bg-surface-raised border border-border-glass rounded-xl shadow-lg shadow-black/[0.08] py-1 z-50"
        >
          {auth.orgs.map((org) => {
            const isActive = org.id === active.id
            return (
              <button
                key={org.id}
                type="button"
                role="option"
                aria-selected={isActive}
                onClick={() => handlePick(org.id)}
                className="w-full flex items-center justify-between gap-3 px-3 py-2 text-left text-[13px] text-text-primary hover:bg-black/[0.03] transition-colors"
              >
                <span className="flex flex-col">
                  <span className="font-medium">{org.name}</span>
                  <span className="text-[11px] text-text-tertiary capitalize">{org.role}</span>
                </span>
                {isActive && <Check size={14} className="text-accent shrink-0" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
