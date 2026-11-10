import { useEffect, useRef, useState, type FormEvent } from 'react'
import { api, lineColor, type LogLine, type Metrics, type Tunnel } from './api'
import { Confirm, CopyButton, Field, Modal, timeAgo, usePolling, useToast } from './ui'

const remoteLabels: Record<string, [string, string]> = {
  healthy: ['ok', 'Connected'],
  degraded: ['warn', 'Degraded'],
  down: ['bad', 'Down'],
  inactive: ['off', 'Never connected'],
  unknown: ['off', 'Unknown status'],
}

export function TunnelsPage() {
  const toast = useToast()
  const { data: tunnels, error, refresh } = usePolling(api.tunnels, 10000)
  const [creating, setCreating] = useState(false)
  const [tokenFor, setTokenFor] = useState<Tunnel | null>(null)
  const [logsFor, setLogsFor] = useState<number | null>(null)
  const [deleting, setDeleting] = useState<{ tunnel: Tunnel; force: boolean; reason?: string } | null>(null)

  async function run(what: string, fn: () => Promise<unknown>) {
    try {
      await fn()
      toast(what)
    } catch (e: any) {
      toast(e.message, 'error')
    }
    refresh()
  }

  async function sync(t: Tunnel) {
    try {
      const res = await api.syncTunnel(t.id)
      if (res.errors.length) toast(`Synced with errors: ${res.errors.join('; ')}`, 'error')
      else toast(`${t.name} synced with Cloudflare`)
    } catch (e: any) {
      toast(e.message, 'error')
    }
    refresh()
  }

  async function remove() {
    if (!deleting) return
    const { tunnel, force } = deleting
    try {
      await api.deleteTunnel(tunnel.id, force)
      toast(`${tunnel.name} deleted`)
      setDeleting(null)
    } catch (e: any) {
      setDeleting({ tunnel, force: true, reason: e.message })
    }
    refresh()
  }

  if (!tunnels) return <p className={error ? 'notice notice-error' : 'muted'}>{error || 'Loading…'}</p>

  return (
    <>
      <div className="page-head">
        <h1>Tunnels</h1>
        <button className="btn btn-primary" onClick={() => setCreating(true)}>Create tunnel</button>
      </div>
      {error && <p className="notice notice-error">{error}</p>}
      {tunnels.length === 0 && (
        <div className="empty">
          <h2>Create your first tunnel</h2>
          <p>
            A tunnel opens an outbound connection from your network to Cloudflare. All traffic for your domains comes in
            through it, so you can close ports 80 and 443 on your router.
          </p>
          <button className="btn btn-primary" onClick={() => setCreating(true)}>Create tunnel</button>
        </div>
      )}
      <div className="tunnels">
        {tunnels.map(t => {
          const [cls, label] = remoteLabels[t.remote?.status ?? 'unknown'] ?? remoteLabels.unknown
          const conns = (t.remote?.connectors ?? []).flatMap(c => c.conns ?? [])
          const colos = [...new Set(conns.map(c => c.colo_name))]
          const managed = t.mode === 'managed'
          return (
            <section key={t.id} className="tunnel" style={{ '--line': lineColor(t.id) } as any}>
              <header className="tunnel-head">
                <span className="line-badge" aria-hidden="true" />
                <div className="tunnel-title">
                  <h2>{t.name}</h2>
                  <p className="muted">
                    {t.hostCount} {t.hostCount === 1 ? 'host' : 'hosts'}.{' '}
                    {managed ? 'The connector runs on this server.' : 'The connector runs on another machine.'}
                  </p>
                </div>
                <span className={`state state-${cls}`} title={t.remote?.error}>{label}</span>
              </header>

              <dl className="kv">
                <dt>Connections</dt>
                <dd>{conns.length ? `${conns.length} to Cloudflare (${colos.join(', ')})` : 'none'}</dd>
                {(t.remote?.connectors ?? []).length > 0 && (
                  <>
                    <dt>Connectors</dt>
                    <dd>{t.remote!.connectors.map(c => `cloudflared ${c.version} (${c.arch})`).join(', ')}</dd>
                  </>
                )}
                {managed && (
                  <>
                    <dt>Local process</dt>
                    <dd>
                      {!t.enabled ? 'stopped'
                        : t.runner.state === 'running' ? `running, started ${timeAgo(t.runner.startedAt)}`
                        : `restarting: ${t.runner.lastError || 'starting…'}`}
                      {t.runner.restarts > 0 && `, ${t.runner.restarts} restarts`}
                    </dd>
                  </>
                )}
                <dt>Last sync</dt>
                <dd className={t.lastSyncError ? 'text-bad' : ''}>{t.lastSyncError || timeAgo(t.lastSyncAt)}</dd>
                <dt>ID</dt>
                <dd><code>{t.cfId}</code></dd>
              </dl>

              {managed && t.enabled && <MetricsRow id={t.id} />}

              <footer className="tunnel-actions">
                {managed ? (
                  <>
                    <button className="btn btn-small" onClick={() => run(t.enabled ? `${t.name} stopped` : `${t.name} started`, () => api.setTunnelRun(t.id, t.mode, !t.enabled))}>
                      {t.enabled ? 'Stop' : 'Start'}
                    </button>
                    {t.enabled && <button className="btn btn-small" onClick={() => run(`${t.name} restarted`, () => api.restartTunnel(t.id))}>Restart</button>}
                    <button className="btn btn-small" onClick={() => setLogsFor(logsFor === t.id ? null : t.id)} aria-expanded={logsFor === t.id}>
                      {logsFor === t.id ? 'Hide log' : 'Show log'}
                    </button>
                  </>
                ) : (
                  <button className="btn btn-small" onClick={() => setTokenFor(t)}>How to connect</button>
                )}
                <button className="btn btn-small" onClick={() => sync(t)}>Sync</button>
                <button className="btn btn-small" onClick={() => run(
                  managed ? 'The connector now runs on another machine' : 'The connector now runs on this server',
                  () => api.setTunnelRun(t.id, managed ? 'external' : 'managed', true))}>
                  {managed ? 'Move to another machine' : 'Run here'}
                </button>
                <button className="btn btn-small btn-quiet-danger push" onClick={() => setDeleting({ tunnel: t, force: false })}>Delete</button>
              </footer>
              {logsFor === t.id && <LogPanel id={t.id} />}
            </section>
          )
        })}
      </div>

      {creating && <CreateTunnel onClose={() => setCreating(false)} onCreated={t => { setCreating(false); refresh(); if (t.mode === 'external') setTokenFor(t) }} />}
      {tokenFor && <TokenModal tunnel={tokenFor} onClose={() => setTokenFor(null)} />}
      {deleting && (
        <Confirm danger title={deleting.force ? 'Could not clean up in Cloudflare' : `Delete tunnel ${deleting.tunnel.name}`}
          action={deleting.force ? 'Delete here only' : 'Delete tunnel'} onClose={() => setDeleting(null)} onConfirm={remove}
          body={deleting.force
            ? <><p className="notice notice-error">{deleting.reason}</p><p>You can delete it from this panel only; anything left in Cloudflare has to be removed by hand.</p></>
            : <p>
                This deletes the tunnel from Cloudflare
                {deleting.tunnel.hostCount > 0 && <>, along with its <strong>{deleting.tunnel.hostCount} hosts</strong>, their DNS records and their Access applications</>}.
                Those domains will stop working.
              </p>} />
      )}
    </>
  )
}

function CreateTunnel({ onClose, onCreated }: { onClose: () => void; onCreated: (t: Tunnel) => void }) {
  const toast = useToast()
  const [name, setName] = useState('')
  const [mode, setMode] = useState('managed')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const t = await api.createTunnel(name, mode)
      toast(`Tunnel ${t.name} created`)
      onCreated(t)
    } catch (e: any) {
      setError(e.message)
      setBusy(false)
    }
  }

  return (
    <Modal title="Create tunnel" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="modal-body">
          <Field label="Name" hint="Something to recognize it by, e.g. the server name.">
            <input value={name} onChange={e => setName(e.target.value)} placeholder="home" autoFocus required maxLength={100} />
          </Field>
          <fieldset>
            <legend>Where the connector runs</legend>
            <label className="check">
              <input type="radio" name="mode" checked={mode === 'managed'} onChange={() => setMode('managed')} />
              <span><strong>On this server.</strong> The panel starts and monitors cloudflared for you.</span>
            </label>
            <label className="check">
              <input type="radio" name="mode" checked={mode === 'external'} onChange={() => setMode('external')} />
              <span><strong>On another machine.</strong> You get the command to run there; routes are still managed from here.</span>
            </label>
          </fieldset>
          {error && <p className="notice notice-error">{error}</p>}
        </div>
        <footer className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" disabled={busy}>{busy ? 'Creating…' : 'Create tunnel'}</button>
        </footer>
      </form>
    </Modal>
  )
}

function TokenModal({ tunnel, onClose }: { tunnel: Tunnel; onClose: () => void }) {
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  useEffect(() => {
    api.tunnelToken(tunnel.id).then(r => setToken(r.token)).catch(e => setError(e.message))
  }, [tunnel.id])
  const docker = `docker run -d --name cloudflared-${tunnel.name.replace(/[^a-zA-Z0-9_.-]/g, '-')} --restart unless-stopped \\\n  -e TUNNEL_TOKEN=${token} \\\n  cloudflare/cloudflared:latest tunnel --no-autoupdate run`
  return (
    <Modal wide title={`Connect ${tunnel.name} from another machine`} onClose={onClose}>
      <div className="modal-body">
        {error && <p className="notice notice-error">{error}</p>}
        <p>Run this on the machine that can reach your services. Once it connects, the tunnel shows as "Connected".</p>
        <div className="code-head"><span>With Docker</span>{token && <CopyButton text={docker} />}</div>
        <pre className="code">{token ? docker : 'Loading…'}</pre>
        <div className="code-head"><span>With cloudflared installed</span>{token && <CopyButton text={`cloudflared service install ${token}`} />}</div>
        <pre className="code">{token ? `sudo cloudflared service install ${token}` : 'Loading…'}</pre>
        <p className="notice notice-warn">The token gives control of this tunnel: treat it like a password.</p>
      </div>
      <footer className="modal-foot"><button className="btn btn-primary" onClick={onClose}>Done</button></footer>
    </Modal>
  )
}

function MetricsRow({ id }: { id: number }) {
  const { data } = usePolling<Metrics>(() => api.tunnelMetrics(id), 5000)
  if (!data?.available) return null
  const errors5xx = Object.entries(data.responseCodes).filter(([c]) => c.startsWith('5')).reduce((n, [, v]) => n + v, 0)
  const items: [string, number][] = [
    ['Requests', data.totalRequests],
    ['In progress', data.concurrentRequests],
    ['5xx responses', errors5xx],
    ['Proxy errors', data.requestErrors],
  ]
  return (
    <dl className="metrics" title="Since the connector last started">
      {items.map(([label, v]) => <div key={label}><dt>{label}</dt><dd>{v.toLocaleString('en-US')}</dd></div>)}
    </dl>
  )
}

function LogPanel({ id }: { id: number }) {
  const [lines, setLines] = useState<LogLine[]>([])
  const box = useRef<HTMLPreElement>(null)
  const pinned = useRef(true)

  useEffect(() => {
    setLines([])
    const es = new EventSource(`/api/tunnels/${id}/logs`)
    es.onmessage = ev => {
      const line: LogLine = JSON.parse(ev.data)
      // A reconnect replays the backlog: keep only lines we have not seen.
      setLines(prev => (prev.length && line.seq <= prev[prev.length - 1].seq ? prev : [...prev, line].slice(-1000)))
    }
    return () => es.close()
  }, [id])

  useEffect(() => {
    const el = box.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [lines])

  return (
    <pre className="log" ref={box} tabIndex={0} aria-label="cloudflared log"
      onScroll={e => { const el = e.currentTarget; pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40 }}>
      {lines.length === 0 && <span className="muted">No lines yet.</span>}
      {lines.map(l => (
        <div key={l.seq} className={/\bERR\b|\bFTL\b/.test(l.text) ? 'log-err' : /\bWRN\b/.test(l.text) ? 'log-warn' : l.text.startsWith('[manager]') ? 'log-mgr' : ''}>
          {l.text}
        </div>
      ))}
    </pre>
  )
}
