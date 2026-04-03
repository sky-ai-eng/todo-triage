import { useState, useEffect } from 'react'

interface SettingsData {
  github: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
  }
  jira: {
    enabled: boolean
    base_url: string
    has_token: boolean
    poll_interval: string
    projects: string[]
  }
  server: { port: number }
  ai: {
    model: string
    reprioritize_threshold: number
    preference_update_interval: number
  }
}

export default function Settings() {
  const [data, setData] = useState<SettingsData | null>(null)
  const [form, setForm] = useState({
    github_enabled: false,
    github_url: '',
    github_pat: '',
    jira_enabled: false,
    jira_url: '',
    jira_pat: '',
    github_poll_interval: '60s',
    jira_poll_interval: '60s',
    jira_projects: '',
    ai_model: 'sonnet',
    server_port: 3000,
  })
  const [saving, setSaving] = useState(false)
  const [message, setMessage] = useState<{ type: 'success' | 'error'; text: string } | null>(null)

  useEffect(() => {
    fetch('/api/settings')
      .then((r) => r.json())
      .then((d: SettingsData) => {
        setData(d)
        setForm({
          github_enabled: d.github.enabled,
          github_url: d.github.base_url || '',
          github_pat: '',
          jira_enabled: d.jira.enabled,
          jira_url: d.jira.base_url || '',
          jira_pat: '',
          github_poll_interval: d.github.poll_interval,
          jira_poll_interval: d.jira.poll_interval,
          jira_projects: (d.jira.projects || []).join(', '),
          ai_model: d.ai.model,
          server_port: d.server.port,
        })
      })
  }, [])

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setMessage(null)

    const projects = form.jira_projects
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)

    try {
      const res = await fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          github_enabled: form.github_enabled,
          github_url: form.github_url,
          github_pat: form.github_pat || undefined,
          jira_enabled: form.jira_enabled,
          jira_url: form.jira_url,
          jira_pat: form.jira_pat || undefined,
          github_poll_interval: form.github_poll_interval,
          jira_poll_interval: form.jira_poll_interval,
          jira_projects: projects,
          ai_model: form.ai_model,
          server_port: form.server_port,
        }),
      })
      const body = await res.json()
      if (!res.ok) {
        setMessage({ type: 'error', text: body.error || 'Save failed' })
      } else {
        setMessage({ type: 'success', text: 'Settings saved.' })
        setForm((f) => ({ ...f, github_pat: '', jira_pat: '' }))
      }
    } catch {
      setMessage({ type: 'error', text: 'Could not connect to server' })
    } finally {
      setSaving(false)
    }
  }

  if (!data) {
    return (
      <div className="flex items-center justify-center min-h-[50vh]">
        <p className="text-text-tertiary text-[13px]">Loading settings...</p>
      </div>
    )
  }

  return (
    <div className="max-w-2xl mx-auto">
      <h1 className="text-[22px] font-semibold text-text-primary tracking-tight mb-6">Settings</h1>
      <form onSubmit={save} className="space-y-5">

        {/* GitHub */}
        <Section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-[13px] font-medium text-text-secondary">GitHub</h2>
            <Toggle
              enabled={form.github_enabled}
              onChange={(v) => setForm((f) => ({ ...f, github_enabled: v }))}
            />
          </div>
          {form.github_enabled && (
            <div className="space-y-3">
              <Field label="Base URL">
                <input
                  type="url"
                  placeholder="https://github.com"
                  value={form.github_url}
                  onChange={update('github_url')}
                  className={inputClass}
                />
              </Field>
              <Field label={`Token${data.github.has_token ? ' (leave blank to keep current)' : ''}`}>
                <input
                  type="password"
                  placeholder={data.github.has_token ? '••••••••' : 'GitHub Personal Access Token'}
                  value={form.github_pat}
                  onChange={update('github_pat')}
                  className={inputClass}
                />
              </Field>
              <Field label="Poll interval">
                <select
                  value={form.github_poll_interval}
                  onChange={update('github_poll_interval')}
                  className={inputClass}
                >
                  <option value="30s">30 seconds</option>
                  <option value="1m0s">1 minute</option>
                  <option value="2m0s">2 minutes</option>
                  <option value="5m0s">5 minutes</option>
                </select>
              </Field>
            </div>
          )}
        </Section>

        {/* Jira */}
        <Section>
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-[13px] font-medium text-text-secondary">Jira</h2>
            <Toggle
              enabled={form.jira_enabled}
              onChange={(v) => setForm((f) => ({ ...f, jira_enabled: v }))}
            />
          </div>
          {form.jira_enabled && (
            <div className="space-y-3">
              <Field label="Base URL">
                <input
                  type="url"
                  placeholder="https://jira.yourcompany.com"
                  value={form.jira_url}
                  onChange={update('jira_url')}
                  className={inputClass}
                />
              </Field>
              <Field label={`Token${data.jira.has_token ? ' (leave blank to keep current)' : ''}`}>
                <input
                  type="password"
                  placeholder={data.jira.has_token ? '••••••••' : 'Jira Personal Access Token'}
                  value={form.jira_pat}
                  onChange={update('jira_pat')}
                  className={inputClass}
                />
              </Field>
              <Field label="Poll interval">
                <select
                  value={form.jira_poll_interval}
                  onChange={update('jira_poll_interval')}
                  className={inputClass}
                >
                  <option value="30s">30 seconds</option>
                  <option value="1m0s">1 minute</option>
                  <option value="2m0s">2 minutes</option>
                  <option value="5m0s">5 minutes</option>
                </select>
              </Field>
              <Field label="Projects (comma-separated)">
                <input
                  type="text"
                  placeholder="PROJ, INFRA"
                  value={form.jira_projects}
                  onChange={update('jira_projects')}
                  className={inputClass}
                />
              </Field>
            </div>
          )}
        </Section>

        {/* AI */}
        <Section>
          <h2 className="text-[13px] font-medium text-text-secondary mb-4">AI</h2>
          <div className="space-y-3">
            <Field label="Delegation model">
              <select
                value={form.ai_model}
                onChange={update('ai_model')}
                className={inputClass}
              >
                <option value="haiku">Haiku (fast, cheap)</option>
                <option value="sonnet">Sonnet (balanced)</option>
                <option value="opus">Opus (most capable)</option>
              </select>
            </Field>
          </div>
        </Section>

        {/* Message */}
        {message && (
          <div className={`rounded-xl px-4 py-2.5 text-[13px] ${
            message.type === 'success'
              ? 'bg-claim/[0.08] border border-claim/20 text-claim'
              : 'bg-dismiss/[0.08] border border-dismiss/20 text-dismiss'
          }`}>
            {message.text}
          </div>
        )}

        <button
          type="submit"
          disabled={saving}
          className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          {saving ? 'Saving...' : 'Save Settings'}
        </button>

        {/* Danger zone */}
        <Section danger>
          <h2 className="text-[13px] font-medium text-dismiss mb-3">Danger Zone</h2>
          <button
            type="button"
            onClick={async () => {
              if (!confirm('Clear all stored tokens? You will need to re-authenticate.')) return
              await fetch('/api/auth', { method: 'DELETE' })
              window.location.href = '/setup'
            }}
            className="text-[13px] text-dismiss hover:text-dismiss/80 border border-dismiss/20 hover:border-dismiss/30 rounded-xl px-4 py-2 transition-colors"
          >
            Clear All Tokens
          </button>
        </Section>
      </form>
    </div>
  )
}

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

function Section({ children, danger }: { children: React.ReactNode; danger?: boolean }) {
  return (
    <section className={`backdrop-blur-xl bg-surface-raised border rounded-2xl p-6 shadow-sm shadow-black/[0.03] ${
      danger ? 'border-dismiss/15' : 'border-border-glass'
    }`}>
      {children}
    </section>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="text-[11px] text-text-tertiary mb-1.5 block">{label}</span>
      {children}
    </label>
  )
}

function Toggle({ enabled, onChange }: { enabled: boolean; onChange: (v: boolean) => void }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={enabled}
      onClick={() => onChange(!enabled)}
      className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors ${
        enabled ? 'bg-accent' : 'bg-black/[0.08]'
      }`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow-sm transform transition-transform ${
          enabled ? 'translate-x-4' : 'translate-x-0'
        }`}
      />
    </button>
  )
}
