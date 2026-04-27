import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { motion, AnimatePresence } from 'motion/react'
import { toast } from './Toast/toastStore'

export interface TakeoverInfo {
  takeover_path: string
  session_id: string
  resume_command: string
}

interface Props {
  info: TakeoverInfo | null
  onClose: () => void
}

// Centered modal that surfaces the takeover destination + resume command.
// Built inline because the codebase's existing overlays (RunDrawer is a
// side drawer, RepoPickerModal is bespoke) don't fit a centered card.
// Closes on Escape and on backdrop click. Copy buttons use the
// Clipboard API (navigator.clipboard.writeText) — fine because the app
// only runs on localhost, which is a secure context everywhere the
// API is supported. A failure surfaces as a toast and the user can
// still select the field text manually.
//
// Rendered via a portal to document.body. The trigger lives inside
// AgentCard, whose root has `backdrop-blur-xl`; that creates a
// containing block for `fixed` descendants, which would otherwise pin
// the "fixed inset-0" overlay to the card's bounds rather than the
// viewport. Same trick StationDetailOverlay's WaitingPill uses.
export default function TakeoverModal({ info, onClose }: Props) {
  const open = info !== null

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  return createPortal(
    <AnimatePresence>
      {open && info && (
        <>
          <motion.div
            className="fixed inset-0 bg-black/15 backdrop-blur-sm z-40"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />
          <motion.div
            className="fixed inset-0 z-50 flex items-center justify-center p-4 pointer-events-none"
            initial={{ opacity: 0, y: 8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 8 }}
            transition={{ type: 'spring', damping: 28, stiffness: 320 }}
          >
            <div
              role="dialog"
              aria-modal="true"
              aria-labelledby="takeover-modal-title"
              aria-describedby="takeover-modal-desc"
              className="w-full max-w-lg pointer-events-auto bg-surface-raised border border-border-glass rounded-2xl shadow-xl shadow-black/10 overflow-hidden"
              onClick={(e) => e.stopPropagation()}
            >
              <div className="px-5 pt-4 pb-3 border-b border-border-subtle flex items-start justify-between">
                <div>
                  <div className="text-[11px] font-semibold uppercase tracking-wider text-text-tertiary">
                    Run taken over
                  </div>
                  <div
                    id="takeover-modal-title"
                    className="text-[15px] font-semibold text-text-primary mt-0.5"
                  >
                    Resume in your terminal
                  </div>
                </div>
                <button
                  onClick={onClose}
                  className="ml-3 w-7 h-7 rounded-full text-text-tertiary hover:text-text-primary hover:bg-black/[0.04] flex items-center justify-center text-[16px]"
                  aria-label="Close"
                >
                  ×
                </button>
              </div>

              <div className="px-5 py-4 flex flex-col gap-4">
                <p
                  id="takeover-modal-desc"
                  className="text-[12px] text-text-secondary leading-relaxed"
                >
                  The headless run has been stopped and a working copy was cloned to your takeover
                  directory. Paste the command below into a terminal to resume the Claude Code
                  session interactively.
                </p>

                <Field label="Resume command" value={info.resume_command} monospace primary />
                <Field label="Working directory" value={info.takeover_path} monospace />
                <Field label="Session ID" value={info.session_id} monospace />
              </div>

              <div className="px-5 py-3 border-t border-border-subtle flex justify-end">
                <button
                  onClick={onClose}
                  className="text-[12px] font-medium text-text-secondary hover:text-text-primary px-3 py-1.5 rounded-lg hover:bg-black/[0.04] transition-colors"
                >
                  Done
                </button>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>,
    document.body,
  )
}

interface FieldProps {
  label: string
  value: string
  monospace?: boolean
  primary?: boolean
}

function Field({ label, value, monospace, primary }: FieldProps) {
  const [copied, setCopied] = useState(false)
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch (err) {
      toast.error(`Failed to copy: ${(err as Error).message}`)
    }
  }
  return (
    <div>
      <div className="text-[10px] font-semibold uppercase tracking-wider text-text-tertiary mb-1">
        {label}
      </div>
      <div
        className={`flex items-stretch rounded-lg border ${primary ? 'border-accent/30 bg-accent/[0.04]' : 'border-border-subtle bg-black/[0.02]'}`}
      >
        <div
          className={`flex-1 min-w-0 px-3 py-2 text-[12px] ${monospace ? 'font-mono' : ''} text-text-primary break-all`}
        >
          {value}
        </div>
        <button
          onClick={onCopy}
          className="shrink-0 px-3 text-[11px] font-medium text-text-secondary hover:text-text-primary border-l border-border-subtle hover:bg-black/[0.04] transition-colors"
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
    </div>
  )
}
