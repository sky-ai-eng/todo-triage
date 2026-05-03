import { useState, useEffect, useCallback, useRef } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import * as Popover from '@radix-ui/react-popover'
import { ArrowLeft, Trash2, Pencil, Check, X, Plus } from 'lucide-react'
import Markdown from 'react-markdown'
import type { Project, KnowledgeFile } from '../types'
import { readError } from '../lib/api'
import { toast } from '../components/Toast/toastStore'
import TrackerProjectPickers from '../components/TrackerProjectPickers'

// ProjectDetail is the per-project workspace. Top-to-bottom on the
// left:
//   1. Header — name + description (inline-editable), pinned repos
//      surfaced as interactive chips alongside tracker chips.
//   2. Integrations — Jira / Linear pickers. Pinned repos lived here
//      in an earlier draft but moved into the header so the user
//      doesn't see two surfaces showing the same data.
//   3. Knowledge base — markdown files under the project's
//      knowledge-base directory, rendered read-only.
//
// The chat panel slot is the right column at a true 50/50 split. SKY-226
// grafts in the streaming chat with renderers, queueing, and cancellation;
// the placeholder reserves the column so SKY-226 doesn't trigger a
// re-layout when it lands.
//
// Edits across the page are auto-saved — there's no explicit Save button.
// The patch helper handles error toasts; on success the page resyncs from
// the freshly-returned project row.
export default function ProjectDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [project, setProject] = useState<Project | null>(null)
  const [loading, setLoading] = useState(true)
  const [missing, setMissing] = useState(false)

  const refresh = useCallback(async () => {
    if (!id) return
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}`)
      if (res.status === 404) {
        setMissing(true)
        return
      }
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to load project'))
        return
      }
      const data: Project = await res.json()
      setProject(data)
    } catch (err) {
      toast.error(`Failed to load project: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setLoading(false)
    }
  }, [id])

  useEffect(() => {
    refresh()
  }, [refresh])

  const patch = useCallback(
    async (body: Record<string, unknown>) => {
      if (!id) return false
      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(id)}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to update project'))
          return false
        }
        const fresh: Project = await res.json()
        setProject(fresh)
        return true
      } catch (err) {
        toast.error(`Failed to update project: ${err instanceof Error ? err.message : String(err)}`)
        return false
      }
    },
    [id],
  )

  const handleDelete = useCallback(async () => {
    if (!id || !project) return
    if (!confirm(`Delete project "${project.name}"? This can't be undone.`)) return
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' })
      if (!res.ok && res.status !== 204) {
        toast.error(await readError(res, 'Failed to delete project'))
        return
      }
      const cleanupWarning = res.headers.get('X-Cleanup-Warning')
      if (cleanupWarning) {
        toast.warning(cleanupWarning)
      } else {
        toast.success(`Deleted project "${project.name}"`)
      }
      navigate('/projects')
    } catch (err) {
      toast.error(`Failed to delete project: ${err instanceof Error ? err.message : String(err)}`)
    }
  }, [id, project, navigate])

  if (loading) {
    return (
      <div className="max-w-7xl mx-auto">
        <div className="text-text-tertiary text-[13px]">Loading project…</div>
      </div>
    )
  }

  if (missing || !project) {
    return (
      <div className="max-w-7xl mx-auto">
        <Link
          to="/projects"
          className="inline-flex items-center gap-1 text-[13px] text-text-secondary hover:text-text-primary mb-6"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <div className="text-text-secondary text-[13px]">
          Project not found. It may have been deleted.
        </div>
      </div>
    )
  }

  return (
    <div className="max-w-7xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <Link
          to="/projects"
          className="inline-flex items-center gap-1 text-[13px] text-text-secondary hover:text-text-primary"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <button
          type="button"
          onClick={handleDelete}
          className="
            inline-flex items-center gap-1.5 rounded-full
            px-3 py-1.5 text-[12px]
            text-dismiss/80 hover:text-dismiss hover:bg-dismiss/[0.08]
            transition-all
          "
        >
          <Trash2 size={12} />
          Delete project
        </button>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
        <div className="space-y-6">
          <ProjectHeader
            project={project}
            onPatchName={(name) => patch({ name })}
            onPatchDescription={(description) => patch({ description })}
            onPatchPinnedRepos={(pinned_repos) => patch({ pinned_repos })}
          />

          <IntegrationsPanel project={project} onPatch={patch} />

          <KnowledgePanel projectId={project.id} />
        </div>

        <ChatSlotPlaceholder />
      </div>
    </div>
  )
}

// ProjectHeader handles inline edit for name + description and embeds
// the pinned-repos editor + tracker chips in one cohesive block. The
// pinned-repos chips are interactive: hover surfaces an X to remove,
// and a "+" affordance opens a popover of remaining configured repos
// to add. Auto-saves on change.
function ProjectHeader({
  project,
  onPatchName,
  onPatchDescription,
  onPatchPinnedRepos,
}: {
  project: Project
  onPatchName: (name: string) => Promise<boolean | undefined>
  onPatchDescription: (description: string) => Promise<boolean | undefined>
  onPatchPinnedRepos: (pinned: string[]) => Promise<boolean | undefined>
}) {
  const [editingName, setEditingName] = useState(false)
  const [editingDesc, setEditingDesc] = useState(false)
  const [draftName, setDraftName] = useState(project.name)
  const [draftDesc, setDraftDesc] = useState(project.description)

  const beginEditName = () => {
    setDraftName(project.name)
    setEditingName(true)
  }

  const beginEditDesc = () => {
    setDraftDesc(project.description)
    setEditingDesc(true)
  }

  const saveName = async () => {
    if (!draftName.trim() || draftName.trim() === project.name) {
      setEditingName(false)
      return
    }
    const ok = await onPatchName(draftName.trim())
    if (ok) setEditingName(false)
  }

  const saveDesc = async () => {
    if (draftDesc === project.description) {
      setEditingDesc(false)
      return
    }
    const ok = await onPatchDescription(draftDesc)
    if (ok) setEditingDesc(false)
  }

  return (
    <Card>
      <div className="flex items-start justify-between gap-3">
        {editingName ? (
          <div className="flex-1 flex items-center gap-2">
            <input
              type="text"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') saveName()
                if (e.key === 'Escape') {
                  setDraftName(project.name)
                  setEditingName(false)
                }
              }}
              autoFocus
              className="
                flex-1 rounded-lg border border-border-subtle
                bg-white/80 px-3 py-1.5 text-lg font-semibold tracking-tight
                text-text-primary
                focus:outline-none focus:border-accent
              "
            />
            <button
              type="button"
              onClick={saveName}
              className="text-claim hover:bg-claim/10 p-1.5 rounded-full"
            >
              <Check size={14} />
            </button>
            <button
              type="button"
              onClick={() => {
                setDraftName(project.name)
                setEditingName(false)
              }}
              className="text-text-tertiary hover:bg-black/[0.03] p-1.5 rounded-full"
            >
              <X size={14} />
            </button>
          </div>
        ) : (
          <h1 className="text-2xl font-semibold tracking-tight text-text-primary">
            <button
              type="button"
              onClick={beginEditName}
              className="group inline-flex items-center gap-2 text-inherit cursor-pointer"
            >
              {project.name}
              <Pencil size={12} className="text-text-tertiary opacity-0 group-hover:opacity-100" />
            </button>
          </h1>
        )}
      </div>

      <div className="mt-3">
        {editingDesc ? (
          <div className="space-y-2">
            <textarea
              value={draftDesc}
              onChange={(e) => setDraftDesc(e.target.value)}
              autoFocus
              rows={3}
              className="
                w-full rounded-lg border border-border-subtle
                bg-white/80 px-3 py-2 text-[13px] text-text-primary
                resize-none focus:outline-none focus:border-accent
              "
            />
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setDraftDesc(project.description)
                  setEditingDesc(false)
                }}
                className="text-[12px] text-text-secondary hover:text-text-primary px-2 py-1 rounded-full"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={saveDesc}
                className="text-[12px] bg-accent text-white px-3 py-1 rounded-full hover:opacity-90"
              >
                Save
              </button>
            </div>
          </div>
        ) : (
          <p
            className="text-[13px] text-text-secondary leading-relaxed cursor-pointer group inline-flex items-start gap-2 hover:text-text-primary"
            onClick={beginEditDesc}
          >
            {project.description ? (
              project.description
            ) : (
              <span className="italic text-text-tertiary">Add a description…</span>
            )}
            <Pencil
              size={12}
              className="text-text-tertiary opacity-0 group-hover:opacity-100 mt-1 shrink-0"
            />
          </p>
        )}
      </div>

      <div className="mt-4">
        <PinnedReposInline
          pinned={project.pinned_repos}
          onChange={onPatchPinnedRepos}
          jiraKey={project.jira_project_key}
          linearKey={project.linear_project_key}
        />
      </div>
    </Card>
  )
}

// PinnedReposInline renders the pinned-repo chips alongside tracker
// chips. Pinned chips are interactive: hovering surfaces an X to
// remove the pin (auto-saved), and a trailing "+" button opens a
// popover that lists remaining configured repos to add.
//
// The tracker chips render inline but aren't editable here — that's
// the IntegrationsPanel's job. Co-locating them visually keeps the
// "this project is X plus these things" narrative tight.
function PinnedReposInline({
  pinned,
  onChange,
  jiraKey,
  linearKey,
}: {
  pinned: string[]
  onChange: (next: string[]) => Promise<boolean | undefined>
  jiraKey: string
  linearKey: string
}) {
  const [available, setAvailable] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [adderOpen, setAdderOpen] = useState(false)
  const [search, setSearch] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch('/api/repos')
        if (!res.ok) {
          await readError(res, 'load repos')
          return
        }
        const data: Array<{ id: string }> = await res.json()
        if (!cancelled) setAvailable(data.map((r) => r.id))
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [])

  const remove = async (slug: string) => {
    const next = pinned.filter((s) => s !== slug)
    await onChange(next)
  }

  const add = async (slug: string) => {
    if (pinned.includes(slug)) return
    const next = [...pinned, slug].sort()
    const ok = await onChange(next)
    if (ok) {
      setAdderOpen(false)
      setSearch('')
    }
  }

  const addable = available.filter(
    (slug) =>
      !pinned.includes(slug) &&
      (!search.trim() || slug.toLowerCase().includes(search.trim().toLowerCase())),
  )

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {jiraKey && <Chip label={`Jira: ${jiraKey}`} tone="accent" />}
      {linearKey && <Chip label={`Linear: ${linearKey}`} tone="accent" />}
      {pinned.map((slug) => (
        <RepoChip key={slug} slug={slug} onRemove={() => remove(slug)} />
      ))}
      <Popover.Root open={adderOpen} onOpenChange={setAdderOpen}>
        <Popover.Trigger asChild>
          <button
            type="button"
            className="
              inline-flex items-center gap-1 rounded-full
              border border-dashed border-border-subtle
              px-2 py-0.5 text-[11px] text-text-tertiary
              hover:border-accent hover:text-accent hover:bg-accent-soft/40
              transition-colors
            "
          >
            <Plus size={10} />
            Add repo
          </button>
        </Popover.Trigger>
        <Popover.Portal>
          <Popover.Content
            sideOffset={6}
            align="start"
            className="
              z-50 w-72 rounded-xl border border-border-subtle
              bg-white shadow-lg shadow-black/[0.08] p-2
            "
          >
            <input
              type="text"
              autoFocus
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search configured repos…"
              className="
                w-full rounded-lg border border-border-subtle
                bg-white px-2.5 py-1.5 text-[12px] text-text-primary
                placeholder:text-text-tertiary mb-1.5
                focus:outline-none focus:border-accent
              "
            />
            <div className="max-h-60 overflow-y-auto">
              {loading ? (
                <div className="text-[12px] text-text-tertiary px-2 py-1">Loading…</div>
              ) : available.length === 0 ? (
                <div className="text-[12px] text-text-tertiary px-2 py-1">
                  No repos configured.{' '}
                  <Link to="/repos" className="text-accent hover:underline">
                    Add some
                  </Link>
                  .
                </div>
              ) : addable.length === 0 ? (
                <div className="text-[12px] text-text-tertiary px-2 py-1 italic">
                  {pinned.length === available.length
                    ? 'All configured repos are pinned.'
                    : 'No matches.'}
                </div>
              ) : (
                addable.map((slug) => (
                  <button
                    key={slug}
                    type="button"
                    onClick={() => add(slug)}
                    className="
                      w-full text-left px-2 py-1.5 rounded-md
                      text-[12px] text-text-primary
                      hover:bg-black/[0.04] transition-colors
                    "
                  >
                    {slug}
                  </button>
                ))
              )}
            </div>
          </Popover.Content>
        </Popover.Portal>
      </Popover.Root>
    </div>
  )
}

function RepoChip({ slug, onRemove }: { slug: string; onRemove: () => void }) {
  return (
    <span
      className="
        group inline-flex items-center rounded-full
        bg-black/[0.03] text-text-secondary border border-border-subtle
        pl-2 pr-1 py-0.5 text-[11px]
        hover:border-dismiss/40 hover:bg-dismiss/[0.04] transition-colors
      "
    >
      {slug}
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation()
          onRemove()
        }}
        aria-label={`Remove ${slug}`}
        className="
          ml-1 inline-flex items-center justify-center
          h-3.5 w-3.5 rounded-full
          opacity-0 group-hover:opacity-100
          text-text-tertiary hover:text-dismiss hover:bg-dismiss/10
          transition-[opacity,color]
        "
      >
        <X size={10} />
      </button>
    </span>
  )
}

// IntegrationsPanel is now just the tracker-projects section. Pinned
// repos live in the header. Auto-saves: each picker change triggers
// an immediate PATCH; the upstream project state is the source of
// truth and the panel re-renders from it on success.
function IntegrationsPanel({
  project,
  onPatch,
}: {
  project: Project
  onPatch: (body: Record<string, unknown>) => Promise<boolean | undefined>
}) {
  // We track whichever side the user is mid-changing in a ref so
  // we can avoid clobbering the picker while the PATCH is in flight.
  // The UI reads from the project prop on render — there's no local
  // mirror state — so a slow network won't desync the dropdown.
  const inflight = useRef(false)

  const handleJiraChange = async (key: string) => {
    if (inflight.current) return
    inflight.current = true
    try {
      await onPatch({ jira_project_key: key })
    } finally {
      inflight.current = false
    }
  }

  const handleLinearChange = async (key: string) => {
    if (inflight.current) return
    inflight.current = true
    try {
      await onPatch({ linear_project_key: key })
    } finally {
      inflight.current = false
    }
  }

  return (
    <Card>
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-4">
        Integrations
      </h2>
      <TrackerProjectPickers
        jiraKey={project.jira_project_key}
        linearKey={project.linear_project_key}
        onJiraChange={handleJiraChange}
        onLinearChange={handleLinearChange}
      />
    </Card>
  )
}

function KnowledgePanel({ projectId }: { projectId: string }) {
  const [files, setFiles] = useState<KnowledgeFile[]>([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/knowledge`)
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to load knowledge base'))
          return
        }
        const data: KnowledgeFile[] = await res.json()
        if (!cancelled) setFiles(data)
      } catch (err) {
        if (!cancelled) {
          toast.error(
            `Failed to load knowledge base: ${err instanceof Error ? err.message : String(err)}`,
          )
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [projectId])

  return (
    <Card>
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-4">
        Knowledge base
      </h2>

      {loading ? (
        <div className="text-[12px] text-text-tertiary">Loading…</div>
      ) : files.length === 0 ? (
        <div className="text-[12px] text-text-tertiary italic">
          No knowledge files yet. The Curator will populate this as you chat.
        </div>
      ) : (
        <div className="space-y-2">
          {files.map((file) => {
            const isExpanded = expanded === file.path
            return (
              <div
                key={file.path}
                className="rounded-lg border border-border-subtle bg-white/40 overflow-hidden"
              >
                <button
                  type="button"
                  onClick={() => setExpanded(isExpanded ? null : file.path)}
                  className="
                    w-full flex items-center justify-between gap-3
                    px-3 py-2 text-left
                    hover:bg-black/[0.02] transition-colors
                  "
                >
                  <span className="text-[12px] font-medium text-text-primary truncate">
                    {file.path}
                  </span>
                  <span className="text-[10px] text-text-tertiary tabular-nums shrink-0">
                    {formatBytes(file.size_bytes)}
                  </span>
                </button>
                {isExpanded && (
                  <div className="border-t border-border-subtle px-4 py-3 prose prose-sm max-w-none text-[12px] text-text-secondary leading-relaxed">
                    <Markdown>{file.content}</Markdown>
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}
    </Card>
  )
}

function ChatSlotPlaceholder() {
  return (
    <Card className="lg:sticky lg:top-24 lg:h-[calc(100vh-12rem)] flex flex-col">
      <h2 className="text-[13px] font-semibold tracking-tight text-text-primary uppercase mb-2">
        Curator chat
      </h2>
      <div className="flex-1 flex items-center justify-center text-center px-6">
        <div className="text-[12px] text-text-tertiary leading-relaxed italic">
          Chat panel arrives in a follow-up ticket.
          <br />
          The Curator runtime is already running — you can hit it via the API in the meantime.
        </div>
      </div>
    </Card>
  )
}

function Card({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return (
    <section
      className={`
        relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        ${className}
      `}
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-white/30 blur-2xl"
      />
      <div className="relative">{children}</div>
    </section>
  )
}

function Chip({ label, tone }: { label: string; tone: 'accent' | 'muted' }) {
  const cls =
    tone === 'accent'
      ? 'bg-accent-soft text-accent'
      : 'bg-black/[0.03] text-text-secondary border border-border-subtle'
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-[11px] ${cls}`}>
      {label}
    </span>
  )
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}
