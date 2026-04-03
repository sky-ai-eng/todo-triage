import { useState } from 'react'
import { useNavigate } from 'react-router-dom'

export default function Setup() {
  const navigate = useNavigate()
  const [githubEnabled, setGithubEnabled] = useState(true)
  const [jiraEnabled, setJiraEnabled] = useState(false)
  const [form, setForm] = useState({
    github_url: '',
    github_pat: '',
    jira_url: '',
    jira_pat: '',
  })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const update = (field: string) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm((f) => ({ ...f, [field]: e.target.value }))

  const canSubmit = (githubEnabled && form.github_url && form.github_pat) ||
                    (jiraEnabled && form.jira_url && form.jira_pat)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')

    if (!canSubmit) {
      setError('Enable at least one service and fill in its fields')
      return
    }

    setLoading(true)
    try {
      const body = {
        github_url: githubEnabled ? form.github_url : '',
        github_pat: githubEnabled ? form.github_pat : '',
        jira_url: jiraEnabled ? form.jira_url : '',
        jira_pat: jiraEnabled ? form.jira_pat : '',
      }
      const res = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const data = await res.json()
        setError(data.error || 'Setup failed')
        return
      }
      navigate('/')
    } catch {
      setError('Could not connect to server')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <form
        onSubmit={submit}
        className="w-full max-w-lg backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 space-y-6 shadow-lg shadow-black/[0.04]"
      >
        <div>
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Todo Tinder Setup</h1>
          <p className="text-[13px] text-text-tertiary mt-1.5 leading-relaxed">
            Tokens are stored in your OS keychain and never leave your machine.
            Enable the services you use.
          </p>
        </div>

        {/* GitHub */}
        <fieldset className={`space-y-3 transition-opacity ${!githubEnabled ? 'opacity-40' : ''}`}>
          <div className="flex items-center justify-between">
            <legend className="text-[13px] font-medium text-text-secondary">GitHub</legend>
            <Toggle enabled={githubEnabled} onChange={setGithubEnabled} />
          </div>
          {githubEnabled && (
            <>
              <input
                type="url"
                placeholder="https://github.yourcompany.com"
                value={form.github_url}
                onChange={update('github_url')}
                className={inputClass}
              />
              <input
                type="password"
                placeholder="GitHub Personal Access Token"
                value={form.github_pat}
                onChange={update('github_pat')}
                className={inputClass}
              />
              <p className="text-[11px] text-text-tertiary">
                Requires <code className="text-text-secondary">repo</code> and{' '}
                <code className="text-text-secondary">read:org</code> scopes.
              </p>
            </>
          )}
        </fieldset>

        {/* Jira */}
        <fieldset className={`space-y-3 transition-opacity ${!jiraEnabled ? 'opacity-40' : ''}`}>
          <div className="flex items-center justify-between">
            <legend className="text-[13px] font-medium text-text-secondary">Jira</legend>
            <Toggle enabled={jiraEnabled} onChange={setJiraEnabled} />
          </div>
          {jiraEnabled && (
            <>
              <input
                type="url"
                placeholder="https://jira.yourcompany.com"
                value={form.jira_url}
                onChange={update('jira_url')}
                className={inputClass}
              />
              <input
                type="password"
                placeholder="Jira Personal Access Token"
                value={form.jira_pat}
                onChange={update('jira_pat')}
                className={inputClass}
              />
            </>
          )}
        </fieldset>

        {error && (
          <div className="rounded-xl bg-dismiss/[0.08] border border-dismiss/20 px-4 py-2.5 text-[13px] text-dismiss">
            {error}
          </div>
        )}

        <button
          type="submit"
          disabled={loading || !canSubmit}
          className="w-full bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
        >
          {loading ? 'Validating...' : 'Connect & Start'}
        </button>
      </form>
    </div>
  )
}

const inputClass =
  'w-full bg-white/50 border border-border-subtle rounded-xl px-4 py-2.5 text-[13px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors'

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
