import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import type { PromptBinding } from '../types'

interface Props {
  promptId: string | null
  isNew?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

const TEMPLATE_VARS = [
  { name: '{{OWNER}}', desc: 'Repository owner' },
  { name: '{{REPO}}', desc: 'Repository name' },
  { name: '{{PR_NUMBER}}', desc: 'Pull request number' },
]

const MIN_WIDTH = 380
const MAX_WIDTH = 900
const STORAGE_KEY = 'prompt-drawer-width'

function loadWidth(): number {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored) {
      const w = parseInt(stored, 10)
      if (w >= MIN_WIDTH && w <= MAX_WIDTH) return w
    }
  } catch {}
  return 520
}

export default function PromptDrawer({ promptId, isNew, onClose, onSaved, onDeleted }: Props) {
  const [name, setName] = useState('')
  const [body, setBody] = useState('')
  const [source, setSource] = useState('user')
  const [bindings, setBindings] = useState<PromptBinding[]>([])
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [width, setWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startWidth = useRef(0)

  const open = promptId !== null || isNew

  useEffect(() => {
    if (isNew) {
      setName('')
      setBody('')
      setSource('user')
      setBindings([])
      setError('')
      return
    }
    if (!promptId) return
    fetch(`/api/prompts/${promptId}`)
      .then(res => res.json())
      .then(data => {
        setName(data.prompt.name)
        setBody(data.prompt.body)
        setSource(data.prompt.source)
        setBindings(data.bindings || [])
        setError('')
      })
      .catch(() => setError('Failed to load prompt'))
  }, [promptId, isNew])

  // Resize drag handlers
  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    dragging.current = true
    startX.current = e.clientX
    startWidth.current = width
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
  }, [width])

  useEffect(() => {
    const onMouseMove = (e: MouseEvent) => {
      if (!dragging.current) return
      const delta = startX.current - e.clientX
      const newWidth = Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, startWidth.current + delta))
      setWidth(newWidth)
    }
    const onMouseUp = () => {
      if (!dragging.current) return
      dragging.current = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      // Persist on release
      localStorage.setItem(STORAGE_KEY, String(width))
    }
    window.addEventListener('mousemove', onMouseMove)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      window.removeEventListener('mousemove', onMouseMove)
      window.removeEventListener('mouseup', onMouseUp)
    }
  }, [width])

  const save = async () => {
    if (!name.trim() || !body.trim()) {
      setError('Name and body are required')
      return
    }
    setSaving(true)
    setError('')

    try {
      if (isNew) {
        const res = await fetch('/api/prompts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, body, bindings }),
        })
        if (!res.ok) throw new Error('Failed to create')
      } else {
        const res = await fetch(`/api/prompts/${promptId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, body }),
        })
        if (!res.ok) throw new Error('Failed to save')
      }
      onSaved()
    } catch {
      setError('Failed to save prompt')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!promptId || source === 'system') return
    setDeleting(true)
    try {
      const res = await fetch(`/api/prompts/${promptId}`, { method: 'DELETE' })
      if (!res.ok) throw new Error('Failed to delete')
      onDeleted?.()
    } catch {
      setError('Failed to delete prompt')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 bg-black/10 backdrop-blur-sm z-40"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Drawer */}
          <motion.div
            className="fixed top-0 right-0 bottom-0 z-50 bg-surface-raised border-l border-border-glass shadow-2xl shadow-black/10 flex flex-col"
            style={{ width: Math.min(width, window.innerWidth * 0.9) }}
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={{ type: 'spring', damping: 30, stiffness: 300 }}
          >
            {/* Resize handle */}
            <div
              onMouseDown={onMouseDown}
              className="absolute left-0 top-0 bottom-0 w-1.5 cursor-col-resize hover:bg-accent/20 active:bg-accent/30 transition-colors z-10"
            />

            {/* Header */}
            <div className="px-6 py-5 border-b border-border-subtle flex items-center justify-between shrink-0">
              <h2 className="text-[15px] font-semibold text-text-primary">
                {isNew ? 'New Prompt' : 'Edit Prompt'}
              </h2>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
              >
                &times;
              </button>
            </div>

            {/* Body — scrollable */}
            <div className="flex-1 overflow-y-auto px-6 py-5 space-y-5">
              {/* Name */}
              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">Name</label>
                <input
                  type="text"
                  value={name}
                  onChange={e => setName(e.target.value)}
                  placeholder="e.g. Thorough Code Review"
                  className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                />
              </div>

              {/* Body */}
              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">Prompt Body</label>
                <textarea
                  value={body}
                  onChange={e => setBody(e.target.value)}
                  placeholder="Describe what the agent should do..."
                  rows={16}
                  className="w-full px-3 py-2.5 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary font-mono leading-relaxed placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors resize-y"
                />
              </div>

              {/* Template variables reference */}
              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">Template Variables</label>
                <div className="bg-black/[0.02] rounded-lg border border-border-subtle p-3 space-y-1.5">
                  {TEMPLATE_VARS.map(v => (
                    <div key={v.name} className="flex items-center gap-3">
                      <code className="text-[11px] font-mono text-accent bg-accent/[0.06] px-1.5 py-0.5 rounded">{v.name}</code>
                      <span className="text-[11px] text-text-tertiary">{v.desc}</span>
                    </div>
                  ))}
                  <p className="text-[10px] text-text-tertiary mt-2 pt-2 border-t border-border-subtle">
                    Tool guidance and completion format are injected automatically. You only need to write the mission.
                  </p>
                </div>
              </div>

              {/* Bindings info */}
              {bindings.length > 0 && (
                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">Event Bindings</label>
                  <div className="space-y-1">
                    {bindings.map(b => (
                      <div key={b.event_type} className="flex items-center gap-2 text-[12px]">
                        <code className="text-text-secondary font-mono bg-black/[0.03] px-1.5 py-0.5 rounded">{b.event_type}</code>
                        {b.is_default && (
                          <span className="text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-emerald-500/10 text-emerald-700">Default</span>
                        )}
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* Metadata */}
              {!isNew && source && (
                <div className="text-[11px] text-text-tertiary space-y-0.5">
                  <p>Source: <span className="font-medium">{source}</span></p>
                </div>
              )}
            </div>

            {/* Footer */}
            <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between shrink-0">
              <div>
                {!isNew && source !== 'system' && (
                  <button
                    onClick={handleDelete}
                    disabled={deleting}
                    className="text-[12px] text-red-500 hover:text-red-600 font-medium transition-colors disabled:opacity-50"
                  >
                    {deleting ? 'Deleting...' : 'Delete'}
                  </button>
                )}
              </div>

              <div className="flex items-center gap-3">
                {error && <span className="text-[12px] text-red-500">{error}</span>}
                <button
                  onClick={onClose}
                  className="text-[12px] text-text-tertiary hover:text-text-secondary font-medium transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={save}
                  disabled={saving}
                  className="text-[12px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-50"
                >
                  {saving ? 'Saving...' : isNew ? 'Create' : 'Save'}
                </button>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
