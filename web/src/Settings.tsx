import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { api, type Account, type PermCheck, type Status, type UpdateInfo } from './api'
import { Confirm, Field, useToast } from './ui'

// The exact values to pick in Cloudflare's "Create Custom Token" form.
const permissions: { key: string; scope: string; name: string; level: string; why: string; optional?: boolean }[] = [
  { key: 'tunnel', scope: 'Account', name: 'Cloudflare Tunnel', level: 'Edit', why: 'Create tunnels and write their routes' },
  { key: 'zone', scope: 'Zone', name: 'Zone', level: 'Read', why: 'See your domains' },
  { key: 'dns', scope: 'Zone', name: 'DNS', level: 'Edit', why: 'Create the DNS record for each host' },
  { key: 'access', scope: 'Account', name: 'Access: Apps and Policies', level: 'Edit', why: 'Protect hosts with sign-in', optional: true },
]

export function SettingsPage({ status, onChange }: { status: Status; onChange: () => void }) {
  const cfg = status.cloudflare
  const [checks, setChecks] = useState<PermCheck[] | null>(null)
  const [checking, setChecking] = useState(false)
  const [replacing, setReplacing] = useState(false)

  const runChecks = useCallback(async () => {
    setChecking(true)
    try {
      setChecks(await api.checkCloudflare())
    } catch {
      setChecks(null)
    } finally {
      setChecking(false)
    }
  }, [])
  useEffect(() => { if (cfg?.configured) runChecks() }, [cfg?.configured, cfg?.accountId, runChecks])

  const showForm = !cfg?.configured || replacing
  return (
    <>
      <div className="page-head"><h1>Settings</h1></div>

      <section className="panel">
        <h2>Cloudflare account</h2>
        {cfg?.configured ? (
          <>
            <div className="connected">
              <span className="state state-ok">Connected</span>
              <strong>{cfg.accountName}</strong>
              <span className="push" />
              <button className="btn btn-small" onClick={runChecks} disabled={checking}>{checking ? 'Checking…' : 'Check permissions'}</button>
              <button className="btn btn-small" onClick={() => setReplacing(r => !r)}>{replacing ? 'Cancel' : 'Replace token'}</button>
            </div>
            {checks && <PermissionResults checks={checks} />}
          </>
        ) : (
          <p>
            Connect your account so the panel can create tunnels and DNS records for you. You need an API token with just
            the permissions it requires; the steps are below.
          </p>
        )}

        {showForm && (
          <>
            <TokenGuide />
            <CloudflareForm onSaved={() => { setReplacing(false); onChange() }} />
          </>
        )}
      </section>

      <section className="panel">
        <h2>How the token is stored</h2>
        <ul className="plain-list">
          <li>It is encrypted with AES-256-GCM before being written to the database (<code>data/ctm.db</code>). Tunnel tokens are stored the same way.</li>
          <li>
            The encryption key is in <code>data/secret.key</code>, or is derived from the <code>CTM_SECRET_KEY</code> variable if you set it.
            With the variable, a copy of the <code>data</code> folder is not enough to read the token.
          </li>
          <li>It is never sent back to the browser or written to the logs, which is why you cannot view it here, only replace it.</li>
          <li>To revoke it, delete it in Cloudflare (<em>My Profile → API Tokens</em>). Existing tunnels keep working; you just can no longer change them from here.</li>
        </ul>
      </section>

      <section className="panel">
        <h2>Password</h2>
        <PasswordForm />
      </section>

      <section className="panel">
        <h2>About and updates</h2>
        <dl className="kv">
          <dt>Tunnel Manager</dt><dd>{status.version}</dd>
          <dt>cloudflared</dt><dd>{status.cloudflared?.available ? status.cloudflared.version : 'not installed'}</dd>
        </dl>
        <UpdatePanel />
      </section>
    </>
  )
}

function UpdatePanel() {
  const [info, setInfo] = useState<UpdateInfo | null>(null)
  const [error, setError] = useState('')
  const [checking, setChecking] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [installing, setInstalling] = useState('')

  const check = useCallback(async (fresh: boolean) => {
    setChecking(true)
    setError('')
    try {
      setInfo(await api.checkUpdate(fresh))
    } catch (e: any) {
      setError(e.message)
    } finally {
      setChecking(false)
    }
  }, [])
  useEffect(() => { check(false) }, [check])

  async function install() {
    setConfirming(false)
    setError('')
    try {
      const { version } = await api.applyUpdate()
      setInstalling(version)
      // The server restarts on the new binary: wait for it, then reload the UI.
      for (let i = 0; i < 60; i++) {
        await new Promise(r => setTimeout(r, 1500))
        try {
          if ((await api.status()).version === version) return location.reload()
        } catch { /* still restarting */ }
      }
      setInstalling('')
      setError(`The server did not come back on ${version} within 90 seconds. Check the service log on the machine.`)
    } catch (e: any) {
      setError(e.message)
    }
  }

  return (
    <div className="update">
      {installing ? (
        <p className="notice notice-info">Installing {installing} and restarting. This page reloads when the server is back.</p>
      ) : (
        <>
          <div className="connected">
            {info && (info.updateAvailable
              ? <span className="state state-warn">Version {info.latest} is available</span>
              : <span className="state state-ok">Up to date{info.latest ? ` (latest release: ${info.latest})` : ''}</span>)}
            <span className="push" />
            {info?.url && <a className="btn btn-small" href={info.url} target="_blank" rel="noreferrer">Release notes</a>}
            <button className="btn btn-small" onClick={() => check(true)} disabled={checking}>{checking ? 'Checking…' : 'Check for updates'}</button>
            {info?.canSelfUpdate && <button className="btn btn-small btn-primary" onClick={() => setConfirming(true)}>Update now</button>}
          </div>
          {info?.why && <p className="field-hint">{info.updateAvailable ? `It cannot be installed from here: ${info.why}.` : `${info.why[0].toUpperCase()}${info.why.slice(1)}.`}</p>}
        </>
      )}
      {error && <p className="notice notice-error">{error}</p>}
      {confirming && info && (
        <Confirm title={`Update to ${info.latest}`} action="Update now" onClose={() => setConfirming(false)} onConfirm={install}
          body={<p>
            The panel downloads the release from GitHub, verifies its checksum, replaces itself and restarts. Tunnels that run
            on this server reconnect during the restart, so their hosts are unreachable for a few seconds.
          </p>} />
      )}
    </div>
  )
}

function PermissionResults({ checks }: { checks: PermCheck[] }) {
  const byKey = new Map(checks.map(c => [c.key, c]))
  return (
    <>
      <table className="perm-table perm-results">
        <thead><tr><th>Permission</th><th>Purpose</th><th>Status</th></tr></thead>
        <tbody>
          {permissions.map(p => {
            const c = byKey.get(p.key)
            return (
              <tr key={p.key}>
                <td><strong>{p.name}</strong> <span className="muted">({p.scope}, {p.level})</span></td>
                <td>{p.why}{p.key === 'zone' && c?.ok && c.detail && <span className="muted">: {c.detail}</span>}</td>
                <td>
                  {!c ? <span className="muted">not checked</span>
                    : c.ok ? <span className="state state-ok">Granted</span>
                    : <span className={`state state-${p.optional ? 'warn' : 'bad'}`}>{p.optional ? 'Not available (optional)' : 'Missing'}</span>}
                  {c && !c.ok && (
                    <div className="perm-error">
                      {c.detail.includes('not_enabled')
                        ? 'Zero Trust is not enabled on the account. Enable it in the Cloudflare dashboard (Zero Trust, free plan) to use Access.'
                        : <>Cloudflare denied access. Edit the token and add the row <strong>{p.scope} → {p.name} → {p.level}</strong>; there is no need to create a new one.</>}
                      <div className="muted">{c.detail}</div>
                    </div>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <p className="field-hint">
        The check only reads, so it confirms the token can reach each area; whether the level is "Edit" only shows when you
        create the first tunnel or host. Access also requires Zero Trust to be enabled on the account (the free plan is enough).
      </p>
    </>
  )
}

function TokenGuide() {
  return (
    <div className="guide">
      <h3>Create the API token</h3>
      <ol className="steps">
        <li>
          Open <a href="https://dash.cloudflare.com/profile/api-tokens" target="_blank" rel="noreferrer">dash.cloudflare.com/profile/api-tokens</a>,
          create a custom token and name it, for example <code>Tunnel Manager</code>.
        </li>
        <li>
          Add a policy for <strong>zones</strong>. The token must be scoped to zones, with <strong>all zones selected</strong> (or
          at least every domain you want to publish). Then tick these permissions in that zone policy, using the search box:
          <table className="perm-table">
            <thead><tr><th>Search for</th><th>Tick the row</th><th>Boxes</th><th>Description shown next to it</th></tr></thead>
            <tbody>
              <tr><td><code>DNS</code></td><td><strong>DNS</strong></td><td>Read and Edit</td><td className="muted">Grants write access to DNS</td></tr>
              <tr><td><code>Zone</code></td><td><strong>Zone</strong></td><td>Read</td><td className="muted">Grants read access to zone management</td></tr>
              <tr><td><code>Zone</code></td><td><strong>Zone Settings</strong></td><td>Read and Edit</td><td className="muted">Grants write access to zone settings</td></tr>
            </tbody>
          </table>
        </li>
        <li>
          Add a policy for your <strong>account</strong> and tick these permissions in it:
          <table className="perm-table">
            <thead><tr><th>Search for</th><th>Tick the row</th><th>Boxes</th><th>What it is for</th></tr></thead>
            <tbody>
              <tr><td><code>Tunnel</code></td><td><strong>Cloudflare Tunnel</strong></td><td>Read and Edit</td><td className="muted">Create tunnels and write their routes</td></tr>
              <tr><td><code>Access</code></td><td><strong>Access: Apps and Policies</strong></td><td>Read and Edit</td><td className="muted">Optional: only for hosts that require sign-in</td></tr>
            </tbody>
          </table>
        </li>
        <li>
          Watch out for <em>DNS Settings</em>: it is a different permission and gives no access to DNS records. The row you
          need is called just <em>DNS</em>.
        </li>
        <li>Do not set an expiry date (TTL): once the token expires, this panel can no longer make changes.</li>
        <li>Create the token. Cloudflare shows it only once: copy it and paste it below.</li>
      </ol>
      <p className="field-hint">
        Editing the token later does not change its value, so there is no need to paste it again: press "Check permissions".
        Changes can take a couple of minutes to apply. Never use the Global API Key here.
      </p>
    </div>
  )
}

function CloudflareForm({ onSaved }: { onSaved: () => void }) {
  const toast = useToast()
  const [token, setToken] = useState('')
  const [accounts, setAccounts] = useState<Account[] | null>(null)
  const [accountId, setAccountId] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const res = await api.configureCloudflare(token.trim(), accountId)
      if (res.account) {
        toast(`Connected to ${res.account.name}`)
        setToken('')
        setAccounts(null)
        setAccountId('')
        onSaved()
      } else if (res.accounts) {
        setAccounts(res.accounts)
        setAccountId(res.accounts[0]?.id ?? '')
      }
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="form-narrow" onSubmit={submit}>
      <Field label="API token">
        <input type="password" value={token} onChange={e => { setToken(e.target.value); setAccounts(null); setAccountId('') }}
          autoComplete="off" spellCheck={false} required placeholder="Paste the token here" />
      </Field>
      {accounts && (
        <Field label="Account" hint="The token can see several accounts: choose the one to manage.">
          <select value={accountId} onChange={e => setAccountId(e.target.value)}>
            {accounts.map(a => <option key={a.id} value={a.id}>{a.name}</option>)}
          </select>
        </Field>
      )}
      {error && <p className="notice notice-error">{error}</p>}
      <div><button className="btn btn-primary" disabled={busy || !token}>{busy ? 'Checking…' : 'Connect account'}</button></div>
    </form>
  )
}

function PasswordForm() {
  const toast = useToast()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [error, setError] = useState('')

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    try {
      await api.changePassword(current, next)
      setCurrent('')
      setNext('')
      toast('Password changed')
    } catch (e: any) {
      setError(e.message)
    }
  }

  return (
    <form className="form-narrow" onSubmit={submit}>
      <Field label="Current password">
        <input type="password" value={current} onChange={e => setCurrent(e.target.value)} autoComplete="current-password" required />
      </Field>
      <Field label="New password" hint="At least 10 characters. Other devices will be signed out.">
        <input type="password" value={next} onChange={e => setNext(e.target.value)} autoComplete="new-password" required minLength={10} />
      </Field>
      {error && <p className="notice notice-error">{error}</p>}
      <div><button className="btn btn-primary">Change password</button></div>
    </form>
  )
}
