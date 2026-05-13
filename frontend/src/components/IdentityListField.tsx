import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { X } from 'lucide-react'
import { useDeploymentConfig, useTeamMembers } from '../hooks/useDeploymentConfig'
import type { TeamMember } from '../types'

/** IdentityListField is the editor for `author_in` / `reviewer_in` /
 *  `commenter_in` predicate fields (SKY-264). Two variants based on
 *  the active user's team size:
 *
 *  - Variant A (team_size===1, i.e. local mode or solo team): a single
 *    "Match my <actor>" toggle. ON → ["<current_user.github_username>"].
 *    OFF → field omitted (no filter). Disabled when github_username is
 *    null with a configure-GitHub hint.
 *
 *  - Variant B (team_size>1): chips + searchable dropdown of team
 *    members. Free-text entry supports external handles (bots,
 *    contractors not on the team). Implements the ARIA combobox
 *    pattern + keyboard navigation (Arrow keys, Enter, Escape).
 *
 *  Both serialize to the same wire shape: `string[] | undefined`. The
 *  matcher does case-insensitive compare server-side, so client casing
 *  is preserved verbatim for round-trip rendering. */
export default function IdentityListField({
  fieldName,
  value,
  onChange,
}: {
  fieldName: string
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
}) {
  const { config, loading } = useDeploymentConfig()
  const labels = labelsForField(fieldName)

  if (loading || !config) {
    return <div className="h-10 rounded-lg bg-black/[0.03] animate-pulse" />
  }

  if (config.team_size <= 1) {
    return <VariantA value={value} onChange={onChange} config={config} labels={labels} />
  }
  return <VariantB value={value} onChange={onChange} config={config} labels={labels} />
}

// --- Actor-specific copy ----------------------------------------------------

interface ActorLabels {
  /** "author" / "reviewer" / "commenter" */
  actor: string
  /** Variant A on-state label, e.g. "Match my PRs". */
  toggleOnLabel: string
  /** Variant A off-state label, e.g. "Match any author". */
  toggleOffLabel: string
  /** Variant A descriptive hint when ON, takes the captured login as a placeholder. */
  scopedHint: (login: string) => string
  /** Variant A descriptive hint when OFF. */
  unscopedHint: string
  /** Variant B placeholder shown in the search input. */
  searchPlaceholder: string
  /** Variant B caption shown beneath the chip area. */
  emptyHint: string
}

function labelsForField(fieldName: string): ActorLabels {
  // Strip the canonical "_in" suffix to derive the actor name. Falls
  // back to the field name itself if the suffix is missing — future
  // non-identity string_list predicates would hit that path and get
  // generic copy, which is still less wrong than mislabeling everything
  // as "author."
  const actor = fieldName.endsWith('_in') ? fieldName.slice(0, -3) : fieldName
  switch (actor) {
    case 'author':
      return {
        actor,
        toggleOnLabel: 'Match my PRs',
        toggleOffLabel: 'Match any author',
        scopedHint: (login) => `Scoped to PRs you authored as ${login}.`,
        unscopedHint: 'Fires on every author’s events.',
        searchPlaceholder: 'search team or type a handle…',
        emptyHint: 'Empty = match any author. Case-insensitive.',
      }
    case 'reviewer':
      return {
        actor,
        toggleOnLabel: 'Match my reviews',
        toggleOffLabel: 'Match any reviewer',
        scopedHint: (login) => `Scoped to reviews you submitted as ${login}.`,
        unscopedHint: 'Fires on every reviewer’s reviews.',
        searchPlaceholder: 'search team or type a reviewer handle…',
        emptyHint: 'Empty = match any reviewer. Case-insensitive.',
      }
    case 'commenter':
      return {
        actor,
        toggleOnLabel: 'Match my comments',
        toggleOffLabel: 'Match any commenter',
        scopedHint: (login) => `Scoped to comments left by ${login}.`,
        unscopedHint: 'Fires on every commenter’s comments.',
        searchPlaceholder: 'search team or type a commenter handle…',
        emptyHint: 'Empty = match any commenter. Case-insensitive.',
      }
    default:
      return {
        actor: actor || 'actor',
        toggleOnLabel: 'Match my events',
        toggleOffLabel: 'Match anyone',
        scopedHint: (login) => `Scoped to ${login}.`,
        unscopedHint: 'Fires on every actor.',
        searchPlaceholder: 'search team or type a handle…',
        emptyHint: 'Empty = match anyone. Case-insensitive.',
      }
  }
}

// --- Variant A: solo / local toggle -----------------------------------------

function VariantA({
  value,
  onChange,
  config,
  labels,
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  config: { current_user: { github_username: string | null } }
  labels: ActorLabels
}) {
  const myLogin = config.current_user.github_username
  const hasIdentity = !!myLogin
  // The toggle is ON whenever the allowlist contains the current user's
  // login. If the user has manually added external handles too, the
  // toggle still represents "am I in the list" — clicking off removes
  // the user but leaves other entries alone. v1 doesn't surface those
  // other entries in Variant A; if you have external handles you should
  // probably be on Variant B anyway.
  const isOn = !!myLogin && (value ?? []).some((h) => h.toLowerCase() === myLogin.toLowerCase())

  const handleToggle = () => {
    if (!hasIdentity) return
    if (isOn) {
      const next = (value ?? []).filter((h) => h.toLowerCase() !== myLogin!.toLowerCase())
      onChange(next.length === 0 ? undefined : next)
    } else {
      onChange([...(value ?? []), myLogin!])
    }
  }

  return (
    <div>
      <button
        type="button"
        onClick={handleToggle}
        disabled={!hasIdentity}
        className={`inline-flex items-center gap-2 px-3 py-1.5 rounded-full text-[12px] font-medium border transition-colors ${
          isOn
            ? 'bg-accent/10 text-accent border-accent/25'
            : 'text-text-tertiary border-border-subtle hover:text-text-secondary'
        } ${!hasIdentity ? 'opacity-50 cursor-not-allowed' : ''}`}
        aria-pressed={isOn}
      >
        <span
          className={`inline-block w-2 h-2 rounded-full ${
            isOn ? 'bg-accent' : 'bg-text-tertiary/40'
          }`}
        />
        {isOn ? labels.toggleOnLabel : labels.toggleOffLabel}
      </button>
      {hasIdentity ? (
        <p className="mt-1.5 text-[11px] text-text-tertiary">
          {isOn ? labels.scopedHint(myLogin!) : labels.unscopedHint}
        </p>
      ) : (
        <p className="mt-1.5 text-[11px] text-amber-600">
          GitHub identity not yet captured.{' '}
          <a href="/settings" className="underline">
            Configure GitHub on Settings
          </a>{' '}
          to enable this filter.
        </p>
      )}
    </div>
  )
}

// --- Variant B: team-aware multi-select (ARIA combobox + listbox) ----------
//
// The ARIA 1.2 combobox pattern: a single input element carries
// role="combobox" + aria-expanded + aria-controls + aria-activedescendant.
// A sibling element with role="listbox" holds option rows
// (role="option", aria-selected). The "active descendant" pattern is
// used (rather than DOM focus) so the input never loses focus while the
// user arrow-keys through suggestions — this is the standard pattern
// when typing and choosing must coexist.
//
// Keyboard:
//   ArrowDown    → move active to next item; open if closed
//   ArrowUp      → move active to previous item
//   Enter        → add the active item (or commit free-text)
//   Escape       → close dropdown without changing selection
//   Backspace    → if input is empty, remove last chip (lifted from native)

/** A flat "interactive item" projected from the dropdown's contents.
 *  Combines team suggestions with the "add external handle" affordance
 *  so the active-descendant index can address them with one shape. */
type DropdownItem = { kind: 'team'; member: TeamMember } | { kind: 'external'; handle: string }

function VariantB({
  value,
  onChange,
  config: _config,
  labels,
}: {
  value: string[] | undefined
  onChange: (val: string[] | undefined) => void
  config: { current_user: { github_username: string | null } }
  labels: ActorLabels
}) {
  const { members, loading } = useTeamMembers()
  const [search, setSearch] = useState('')
  const [open, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(0)
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const listboxId = `identity-listbox-${useId()}`
  const optionIdFor = (i: number) => `${listboxId}-opt-${i}`
  // Memoize `selected` so the useMemo below has a stable dep when the
  // parent passes the same value array shape. (`value ?? []` returns
  // a fresh empty array every render when value is undefined, which
  // would invalidate every downstream memo.)
  const selected = useMemo(() => value ?? [], [value])

  // Close the dropdown when clicking outside the container.
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (!containerRef.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onClick)
    return () => document.removeEventListener('mousedown', onClick)
  }, [])

  const memberByLogin = new Map<string, { display: string; isSelf: boolean }>()
  for (const m of members) {
    if (m.github_username) {
      memberByLogin.set(m.github_username.toLowerCase(), {
        display: m.display_name || m.github_username,
        isSelf: m.is_current_user,
      })
    }
  }

  // The flat dropdown-items list. Filtering and the external-handle
  // affordance combine into one indexable sequence so keyboard nav has
  // a single source of truth.
  const dropdownItems = useMemo<DropdownItem[]>(() => {
    const q = search.trim().toLowerCase()
    const teamMatches = members.filter((m) => {
      if (!m.github_username) return false
      if (selected.some((h) => h.toLowerCase() === m.github_username!.toLowerCase())) return false
      return (
        q === '' ||
        m.github_username.toLowerCase().includes(q) ||
        (m.display_name || '').toLowerCase().includes(q)
      )
    })
    const items: DropdownItem[] = teamMatches.map((m) => ({ kind: 'team', member: m }))
    const exactMatchExists = members.some(
      (m) => m.github_username && m.github_username.toLowerCase() === q,
    )
    const alreadySelected = selected.some((h) => h.toLowerCase() === q)
    if (q !== '' && !exactMatchExists && !alreadySelected) {
      items.push({ kind: 'external', handle: search.trim() })
    }
    return items
    // Recompute when search query or selection set changes; `members`
    // is identity-stable per fetch so it's safe in deps.
  }, [members, search, selected])

  // Clamp the active-index during render rather than via an effect:
  // storing the raw setState value and reading a clamped derivation
  // avoids a cascading-render cycle when the items list shrinks
  // (e.g. user typed more characters and fewer suggestions match).
  // The stored `activeIndex` may temporarily exceed `dropdownItems.length`
  // but every read goes through `safeActiveIndex`.
  const safeActiveIndex =
    dropdownItems.length === 0 ? 0 : Math.min(activeIndex, dropdownItems.length - 1)

  const addHandle = (handle: string) => {
    const trimmed = handle.trim()
    if (!trimmed) return
    if (selected.some((h) => h.toLowerCase() === trimmed.toLowerCase())) return
    onChange([...selected, trimmed])
    setSearch('')
    setActiveIndex(0)
  }
  const removeHandle = (handle: string) => {
    const next = selected.filter((h) => h.toLowerCase() !== handle.toLowerCase())
    onChange(next.length === 0 ? undefined : next)
  }

  const commitActive = () => {
    if (dropdownItems.length === 0) {
      // Nothing in the dropdown but the user has typed something —
      // commit the free-text as a literal external handle.
      if (search.trim()) addHandle(search)
      return
    }
    const item = dropdownItems[safeActiveIndex]
    if (!item) return
    if (item.kind === 'team' && item.member.github_username) {
      addHandle(item.member.github_username)
    } else if (item.kind === 'external') {
      addHandle(item.handle)
    }
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      commitActive()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setOpen(true)
      if (dropdownItems.length > 0) {
        setActiveIndex((i) => (i + 1) % dropdownItems.length)
      }
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setOpen(true)
      if (dropdownItems.length > 0) {
        setActiveIndex((i) => (i - 1 + dropdownItems.length) % dropdownItems.length)
      }
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      setOpen(false)
      return
    }
    if (e.key === 'Backspace' && search === '' && selected.length > 0) {
      removeHandle(selected[selected.length - 1])
    }
  }

  const showDropdown = open && (dropdownItems.length > 0 || loading)
  const ariaExpanded = showDropdown

  return (
    <div className="relative" ref={containerRef}>
      <div
        className="min-h-[40px] w-full px-2 py-1.5 rounded-lg border border-border-subtle bg-white/50 flex flex-wrap items-center gap-1.5 cursor-text focus-within:border-accent/40 focus-within:ring-1 focus-within:ring-accent/20"
        onClick={() => {
          inputRef.current?.focus()
          setOpen(true)
        }}
      >
        {selected.map((h) => {
          const m = memberByLogin.get(h.toLowerCase())
          return (
            <span
              key={h}
              className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-[11px] border ${
                m
                  ? 'bg-accent/10 text-accent border-accent/25'
                  : 'bg-violet-100 text-violet-700 border-violet-200'
              }`}
              title={m ? `${m.display}${m.isSelf ? ' (you)' : ''}` : `External handle: ${h}`}
            >
              {m ? `${m.display} · ${h}` : h}
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation()
                  removeHandle(h)
                }}
                aria-label={`Remove ${h}`}
                className="hover:text-text-primary"
              >
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          )
        })}
        <input
          ref={inputRef}
          type="text"
          role="combobox"
          aria-expanded={ariaExpanded}
          aria-controls={listboxId}
          aria-autocomplete="list"
          aria-activedescendant={
            showDropdown && dropdownItems.length > 0 ? optionIdFor(safeActiveIndex) : undefined
          }
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setOpen(true)
            setActiveIndex(0)
          }}
          onKeyDown={onKeyDown}
          onFocus={() => setOpen(true)}
          placeholder={selected.length === 0 ? labels.searchPlaceholder : ''}
          className="flex-1 min-w-[140px] bg-transparent text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none"
        />
      </div>

      {showDropdown && (
        <ul
          id={listboxId}
          role="listbox"
          className="absolute z-20 mt-1 w-full max-h-[260px] overflow-y-auto rounded-lg border border-border-subtle bg-white shadow-lg list-none p-0 m-0"
        >
          {loading && (
            <li className="px-3 py-2 text-[12px] text-text-tertiary" aria-disabled="true">
              Loading team…
            </li>
          )}
          {dropdownItems.map((item, i) => {
            const isActive = i === safeActiveIndex
            const baseCls = `w-full flex items-center justify-between px-3 py-2 text-left cursor-pointer ${
              isActive ? 'bg-accent/10' : 'hover:bg-accent/5'
            }`
            if (item.kind === 'team') {
              const m = item.member
              return (
                <li
                  id={optionIdFor(i)}
                  key={m.user_id}
                  role="option"
                  aria-selected={isActive}
                  onMouseDown={(e) => {
                    // mousedown rather than click so the input doesn't blur
                    // and close the dropdown before the selection registers.
                    e.preventDefault()
                    addHandle(m.github_username!)
                  }}
                  onMouseEnter={() => setActiveIndex(i)}
                  className={baseCls}
                >
                  <span className="text-[13px] text-text-primary">
                    {m.display_name || m.github_username}
                    {m.is_current_user && (
                      <span className="ml-1 text-[10px] text-text-tertiary">(you)</span>
                    )}
                  </span>
                  <span className="text-[11px] font-mono text-text-tertiary">
                    {m.github_username}
                  </span>
                </li>
              )
            }
            return (
              <li
                id={optionIdFor(i)}
                key={`external-${item.handle}`}
                role="option"
                aria-selected={isActive}
                onMouseDown={(e) => {
                  e.preventDefault()
                  addHandle(item.handle)
                }}
                onMouseEnter={() => setActiveIndex(i)}
                className={`${baseCls} text-violet-700 border-t border-border-subtle`}
              >
                <span className="text-[12px]">
                  + Add &ldquo;{item.handle}&rdquo; as external handle
                </span>
              </li>
            )
          })}
          {/* Members without captured identity render outside the option
              flow so they're not navigable and don't enter the
              active-descendant rotation. Surfaces the gap honestly. */}
          {members
            .filter((m) => !m.github_username)
            .map((m) => (
              <li
                key={m.user_id}
                role="presentation"
                aria-hidden="true"
                className="px-3 py-2 text-[12px] text-text-tertiary opacity-60"
                title="No GitHub identity captured — ask this user to configure GitHub on Settings"
              >
                {m.display_name || '(no name)'}{' '}
                <span className="text-[10px]">— no GitHub identity</span>
              </li>
            ))}
        </ul>
      )}

      <p className="mt-1.5 text-[11px] text-text-tertiary">{labels.emptyHint}</p>
    </div>
  )
}
