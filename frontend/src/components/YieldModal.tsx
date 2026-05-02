import { useEffect, useState } from 'react'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'

/** YieldRequest mirrors domain.YieldRequest in Go. The agent emits one
 *  of three shapes (confirmation / choice / prompt) when it pauses a
 *  run for user input — see internal/ai/prompts/envelope.txt. */
export interface YieldRequest {
  type: 'confirmation' | 'choice' | 'prompt'
  message: string
  // Confirmation
  accept_label?: string
  reject_label?: string
  // Choice
  options?: Array<{ id: string; label: string }>
  multi?: boolean
  // Prompt
  placeholder?: string
}

interface Props {
  runID: string
  request: YieldRequest
  open: boolean
  onClose: () => void
  onSubmitted?: () => void
}

/** Modal that renders the right input for a yield_request and POSTs
 *  the response back to the agent. Sized 480x80vh max with internal
 *  scroll so a long message or option list doesn't overflow the
 *  viewport. */
export default function YieldModal({ runID, request, open, onClose, onSubmitted }: Props) {
  // Internal state initializes fresh on every mount. Callers should
  // pass a `key` derived from the yield_request message id so a new
  // open against a different request gives us a new component
  // instance — that way text/selected/submitting reset without a
  // useEffect-driven setState (banned by the project lint config).
  const [submitting, setSubmitting] = useState(false)
  const [text, setText] = useState('')
  const [selected, setSelected] = useState<string[]>([])

  // Escape closes the modal.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose, submitting])

  if (!open) return null

  const submit = async (body: Record<string, unknown>) => {
    setSubmitting(true)
    try {
      const res = await fetch(`/api/agent/runs/${runID}/respond`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to submit response'))
        setSubmitting(false)
        return
      }
      onSubmitted?.()
      onClose()
    } catch (err) {
      toast.error(`Failed to submit response: ${(err as Error).message}`)
      setSubmitting(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={() => {
        if (!submitting) onClose()
      }}
    >
      <div
        className="w-[480px] max-w-[92vw] max-h-[80vh] overflow-y-auto rounded-2xl bg-surface-raised border border-border-glass shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-5 pt-4 pb-2 border-b border-border-subtle">
          <div className="text-[10px] font-semibold uppercase tracking-wider text-text-tertiary">
            Agent waiting for response
          </div>
        </div>
        <div className="px-5 py-4">
          <p className="text-[13px] text-text-primary leading-relaxed whitespace-pre-wrap mb-4">
            {request.message}
          </p>

          {request.type === 'confirmation' && (
            <ConfirmationBody
              request={request}
              submitting={submitting}
              onAccept={() => submit({ type: 'confirmation', accepted: true })}
              onReject={() => submit({ type: 'confirmation', accepted: false })}
            />
          )}

          {request.type === 'choice' && (
            <ChoiceBody
              request={request}
              selected={selected}
              setSelected={setSelected}
              submitting={submitting}
              onSubmit={() => submit({ type: 'choice', selected })}
            />
          )}

          {request.type === 'prompt' && (
            <PromptBody
              request={request}
              text={text}
              setText={setText}
              submitting={submitting}
              onSubmit={() => submit({ type: 'prompt', value: text })}
            />
          )}
        </div>
      </div>
    </div>
  )
}

function ConfirmationBody({
  request,
  submitting,
  onAccept,
  onReject,
}: {
  request: YieldRequest
  submitting: boolean
  onAccept: () => void
  onReject: () => void
}) {
  return (
    <div className="flex items-center gap-2 justify-end">
      <button
        disabled={submitting}
        onClick={onReject}
        className="text-[12px] font-medium px-3 py-1.5 rounded-lg text-text-secondary hover:bg-black/[0.04] disabled:opacity-50 transition-colors"
      >
        {request.reject_label || 'Cancel'}
      </button>
      <button
        disabled={submitting}
        onClick={onAccept}
        className="text-[12px] font-semibold px-3 py-1.5 rounded-lg text-white bg-accent hover:bg-accent/90 disabled:opacity-50 transition-colors"
      >
        {request.accept_label || 'Confirm'}
      </button>
    </div>
  )
}

function ChoiceBody({
  request,
  selected,
  setSelected,
  submitting,
  onSubmit,
}: {
  request: YieldRequest
  selected: string[]
  setSelected: (v: string[]) => void
  submitting: boolean
  onSubmit: () => void
}) {
  const multi = !!request.multi
  const options = request.options || []

  const toggle = (id: string) => {
    if (multi) {
      setSelected(selected.includes(id) ? selected.filter((s) => s !== id) : [...selected, id])
    } else {
      setSelected([id])
    }
  }

  const canSubmit = multi ? true : selected.length === 1

  return (
    <>
      <div className="flex flex-col gap-1.5 mb-4">
        {options.map((opt) => {
          const checked = selected.includes(opt.id)
          return (
            <button
              key={opt.id}
              disabled={submitting}
              onClick={() => toggle(opt.id)}
              className={`flex items-center gap-3 px-3 py-2 rounded-lg border text-left text-[12px] transition-colors ${
                checked
                  ? 'border-accent/60 bg-accent/10 text-text-primary'
                  : 'border-border-subtle hover:bg-black/[0.03] text-text-secondary'
              } disabled:opacity-50`}
            >
              <span
                className={`shrink-0 inline-flex items-center justify-center w-4 h-4 rounded-${
                  multi ? 'sm' : 'full'
                } border ${checked ? 'border-accent bg-accent text-white' : 'border-border-subtle'}`}
                aria-hidden
              >
                {checked && (
                  <svg width="10" height="10" viewBox="0 0 16 16" fill="none">
                    <path
                      d="M3 8l3 3 7-7"
                      stroke="currentColor"
                      strokeWidth="2.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                )}
              </span>
              <span className="leading-snug">{opt.label}</span>
            </button>
          )
        })}
      </div>
      <div className="flex items-center justify-end">
        <button
          disabled={submitting || !canSubmit}
          onClick={onSubmit}
          className="text-[12px] font-semibold px-3 py-1.5 rounded-lg text-white bg-accent hover:bg-accent/90 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Submit
        </button>
      </div>
    </>
  )
}

function PromptBody({
  request,
  text,
  setText,
  submitting,
  onSubmit,
}: {
  request: YieldRequest
  text: string
  setText: (v: string) => void
  submitting: boolean
  onSubmit: () => void
}) {
  return (
    <>
      <textarea
        autoFocus
        disabled={submitting}
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={request.placeholder || ''}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && text.trim() !== '') {
            onSubmit()
          }
        }}
        className="w-full min-h-[100px] max-h-[40vh] px-3 py-2 rounded-lg border border-border-subtle bg-surface-raised text-[13px] text-text-primary placeholder:text-text-tertiary/60 outline-none focus:border-accent/60 transition-colors resize-y mb-3"
      />
      <div className="flex items-center justify-between gap-2">
        <span className="text-[10px] text-text-tertiary">⌘↩ to submit</span>
        <button
          disabled={submitting || text.trim() === ''}
          onClick={onSubmit}
          className="text-[12px] font-semibold px-3 py-1.5 rounded-lg text-white bg-accent hover:bg-accent/90 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Submit
        </button>
      </div>
    </>
  )
}
