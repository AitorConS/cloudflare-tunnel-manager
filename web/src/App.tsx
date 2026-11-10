import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { api, setUnauthorizedHandler, type Status } from './api'
import { Field, ToastProvider, useToast } from './ui'
import { HostsPage } from './Hosts'
import { TunnelsPage } from './Tunnels'
import { SettingsPage } from './Settings'

type Page = 'hosts' | 'tunnels' | 'settings'
const pages: { id: Page; label: string }[] = [
  { id: 'hosts', label: 'Hosts' },
  { id: 'tunnels', label: 'Tunnels' },
  { id: 'settings', label: 'Settings' },
]

function pageFromHash(): Page {
  const id = location.hash.replace(/^#\/?/, '')
  return pages.some(p => p.id === id) ? (id as Page) : 'hosts'
}

export function Logo() {
  return (
    <svg className="logo" viewBox="0 0 32 32" aria-hidden="true">
      <path d="M5 26v-8a11 11 0 0 1 22 0v8" fill="none" stroke="currentColor" strokeWidth="4" strokeLinecap="round" />
      <circle cx="16" cy="23" r="3.2" fill="var(--signal)" />
    </svg>
  )
}

export default function App() {
  return <ToastProvider><Root /></ToastProvider>
}

function Root() {
  const [status, setStatus] = useState<Status | null>(null)
  const [error, setError] = useState('')
  const [page, setPage] = useState<Page>(pageFromHash)

  const reload = useCallback(async () => {
    try {
      setStatus(await api.status())
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }, [])

  useEffect(() => {
    reload()
    setUnauthorizedHandler(() => setStatus(s => (s ? { ...s, user: null } : s)))
    const onHash = () => setPage(pageFromHash())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [reload])

  if (!status) {
    return (
      <main className="auth">
        {error
          ? <div className="auth-card"><p className="notice notice-error">{error}</p><button className="btn" onClick={reload}>Retry</button></div>
          : <p className="muted">Loading…</p>}
      </main>
    )
  }
  if (!status.user) return <AuthScreen setup={status.setupRequired} onDone={reload} />

  const needsCloudflare = !status.cloudflare?.configured
  const current: Page = needsCloudflare ? 'settings' : page

  return (
    <>
      <header className="topbar">
        <div className="topbar-in">
          <a className="brand" href="#/hosts"><Logo /><span>Tunnel Manager</span></a>
          <nav aria-label="Sections">
            {pages.map(p => (
              <a key={p.id} href={`#/${p.id}`} aria-current={current === p.id ? 'page' : undefined}
                className={needsCloudflare && p.id !== 'settings' ? 'disabled' : ''}>{p.label}</a>
            ))}
          </nav>
          <div className="topbar-user">
            <span className="muted">{status.user.username}</span>
            <button className="btn btn-small" onClick={async () => { await api.logout().catch(() => {}); reload() }}>Sign out</button>
          </div>
        </div>
      </header>
      <main className="page">
        {status.cloudflared && !status.cloudflared.available && (
          <p className="notice notice-warn">
            The <code>cloudflared</code> program was not found on this server, so tunnels that run here cannot
            start. The Docker image already includes it; outside Docker, install it or set its path in <code>CTM_CLOUDFLARED_BIN</code>.
          </p>
        )}
        {current === 'hosts' && <HostsPage />}
        {current === 'tunnels' && <TunnelsPage />}
        {current === 'settings' && <SettingsPage status={status} onChange={reload} />}
      </main>
    </>
  )
}

function AuthScreen({ setup, onDone }: { setup: boolean; onDone: () => void }) {
  const toast = useToast()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [repeat, setRepeat] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (setup && password !== repeat) return setError('Passwords do not match')
    setBusy(true)
    setError('')
    try {
      if (setup) {
        await api.setup(username, password)
        toast('Admin account created')
      } else {
        await api.login(username, password)
      }
      onDone()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="auth">
      <form className="auth-card" onSubmit={submit}>
        <div className="brand brand-big"><Logo /><span>Tunnel Manager</span></div>
        <p className="muted">
          {setup
            ? 'First-time setup: create the admin account you will use to sign in to the panel.'
            : 'Sign in to manage your Cloudflare tunnels.'}
        </p>
        <Field label="Username">
          <input value={username} onChange={e => setUsername(e.target.value)} autoComplete="username" autoFocus required />
        </Field>
        <Field label="Password" hint={setup ? 'At least 10 characters.' : undefined}>
          <input type="password" value={password} onChange={e => setPassword(e.target.value)}
            autoComplete={setup ? 'new-password' : 'current-password'} required minLength={setup ? 10 : undefined} />
        </Field>
        {setup && (
          <Field label="Repeat password">
            <input type="password" value={repeat} onChange={e => setRepeat(e.target.value)} autoComplete="new-password" required />
          </Field>
        )}
        {error && <p className="notice notice-error">{error}</p>}
        <button className="btn btn-primary btn-block" disabled={busy}>{setup ? 'Create account' : 'Sign in'}</button>
      </form>
    </main>
  )
}
