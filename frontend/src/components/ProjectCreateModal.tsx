import { useState, useEffect, useCallback } from 'react'
import { X } from 'lucide-react'
import type { Project } from '../types'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'
import RepoMultiSelect from './RepoMultiSelect'
import TrackerProjectPickers from './TrackerProjectPickers'

// ProjectCreateModal is the only way to create a project from the UI.
// Required: name. Optional: description, pinned repos, tracker
// projects (Jira / Linear). The pinned-repos picker filters to the
// configured-repos list (server validates anyway, but pre-filtering
// keeps the user from picking a slug that's about to be rejected).
interface Props {
  onClose: () => void
  onCreated: (project: Project) => void
}

export default function ProjectCreateModal({ onClose, onCreated }: Props) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [pinnedRepos, setPinnedRepos] = useState<string[]>([])
  const [jiraKey, setJiraKey] = useState('')
  const [linearKey, setLinearKey] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Escape closes unless a create request is in flight.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, submitting])

  const handleSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault()
      if (!name.trim()) {
        toast.error('Name is required')
        return
      }
      setSubmitting(true)
      try {
        const res = await fetch('/api/projects', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: name.trim(),
            description: description.trim(),
            pinned_repos: pinnedRepos,
            jira_project_key: jiraKey,
            linear_project_key: linearKey,
          }),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to create project'))
          return
        }
        const created: Project = await res.json()
        toast.success(`Created project "${created.name}"`)
        onCreated(created)
      } catch (err) {
        toast.error(`Failed to create project: ${err instanceof Error ? err.message : String(err)}`)
      } finally {
        setSubmitting(false)
      }
    },
    [name, description, pinnedRepos, jiraKey, linearKey, onCreated],
  )

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="
          relative w-full max-w-lg max-h-[90vh] overflow-y-auto
          rounded-2xl border border-border-glass
          bg-gradient-to-br from-white/95 via-white/90 to-white/85
          shadow-xl shadow-black/[0.08] backdrop-blur-xl
          p-6
        "
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-create-modal-title"
        aria-describedby="project-create-modal-description"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex items-start justify-between mb-5">
          <div>
            <h2
              id="project-create-modal-title"
              className="text-lg font-semibold tracking-tight text-text-primary"
            >
              New project
            </h2>
            <p
              id="project-create-modal-description"
              className="text-[12px] text-text-tertiary mt-0.5"
            >
              You can add or change everything later.
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="text-text-tertiary hover:text-text-secondary p-1 rounded-full hover:bg-black/[0.03]"
            aria-label="Close"
          >
            <X size={16} />
          </button>
        </header>

        <form onSubmit={handleSubmit} className="space-y-4">
          <Field label="Name" required>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoFocus
              required
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/60 px-3 py-2 text-[13px] text-text-primary
                placeholder:text-text-tertiary
                focus:outline-none focus:border-accent focus:bg-white
              "
              placeholder="e.g. Triage Factory"
            />
          </Field>

          <Field label="Description">
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/60 px-3 py-2 text-[13px] text-text-primary
                placeholder:text-text-tertiary resize-none
                focus:outline-none focus:border-accent focus:bg-white
              "
              placeholder="What this project is about (optional)"
            />
          </Field>

          <Field label="Pinned repos">
            <RepoMultiSelect value={pinnedRepos} onChange={setPinnedRepos} />
          </Field>

          <Field label="Tracker projects">
            <TrackerProjectPickers
              jiraKey={jiraKey}
              linearKey={linearKey}
              onJiraChange={setJiraKey}
              onLinearChange={setLinearKey}
            />
          </Field>

          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={onClose}
              disabled={submitting}
              className="
                rounded-full px-4 py-2 text-[13px]
                text-text-secondary hover:text-text-primary hover:bg-black/[0.03]
                transition-all disabled:opacity-50
              "
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting || !name.trim()}
              className="
                rounded-full px-4 py-2 text-[13px] font-medium
                bg-accent text-white hover:opacity-90
                disabled:opacity-50 transition-all
              "
            >
              {submitting ? 'Creating…' : 'Create project'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function Field({
  label,
  required,
  children,
}: {
  label: string
  required?: boolean
  children: React.ReactNode
}) {
  return (
    <label className="block">
      <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
        {label}
        {required && <span className="text-accent ml-0.5">*</span>}
      </span>
      {children}
    </label>
  )
}
