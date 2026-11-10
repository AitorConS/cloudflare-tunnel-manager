import { useEffect, useMemo, useState, type FormEvent } from 'react'
import { api, ApiError, lineColor, type DnsRecord, type Host, type HostInput, type Tunnel, type Zone } from './api'
import { Confirm, Field, Modal, Toggle, usePolling, useToast } from './ui'

const schemes = ['http', 'https', 'tcp', 'ssh', 'rdp', 'smb'] as const
const sessions: [string, string][] = [
  ['30m', '30 minutes'], ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours'], ['168h', '1 week'], ['730h', '1 month'],
]

function hostState(h: Host, t?: Tunnel): { cls: string; label: string; title?: string } {
  if (h.syncError) return { cls: 'bad', label: 'Out of sync', title: h.syncError }
  if (!h.enabled) return { cls: 'off', label: 'Disabled' }
  const remote = t?.remote?.status
  if (remote === 'healthy') return { cls: 'ok', label: 'Online' }
  if (remote === 'degraded') return { cls: 'warn', label: 'Tunnel degraded' }
  if (remote === 'unknown' || !remote) return { cls: 'off', label: 'Unknown status', title: t?.remote?.error }
  return { cls: 'bad', label: 'Tunnel down' }
}

export function HostsPage() {
  const toast = useToast()
  const { data, error, refresh } = usePolling(async () => {
    const [hosts, tunnels] = await Promise.all([api.hosts(), api.tunnels()])
    return { hosts, tunnels }
  }, 15000)
  const [query, setQuery] = useState('')
  const [tunnelFilter, setTunnelFilter] = useState<number | null>(null)
  const [editing, setEditing] = useState<Host | 'new' | null>(null)
  const [deleting, setDeleting] = useState<{ host: Host; force: boolean; reason?: string } | null>(null)

  const tunnelsById = useMemo(() => new Map((data?.tunnels ?? []).map(t => [t.id, t])), [data])
  const shown = (data?.hosts ?? []).filter(h =>
    (tunnelFilter === null || h.tunnelId === tunnelFilter) &&
    (h.hostname + h.path + h.service).toLowerCase().includes(query.toLowerCase()))
  const online = (data?.hosts ?? []).filter(h => hostState(h, tunnelsById.get(h.tunnelId)).cls === 'ok').length

  async function toggle(h: Host, enabled: boolean) {
    try {
      const res = await api.setHostEnabled(h.id, enabled)
      if (res.syncError) toast(res.syncError, 'error')
      else toast(enabled ? `${h.hostname} enabled` : `${h.hostname} disabled`)
    } catch (e: any) {
      toast(e.message, 'error')
    }
    refresh()
  }

  async function remove() {
    if (!deleting) return
    const { host, force } = deleting
    try {
      await api.deleteHost(host.id, force)
      toast(`${host.hostname} deleted`)
      setDeleting(null)
    } catch (e: any) {
      // Cloudflare refused the cleanup: offer to drop the host locally anyway.
      setDeleting({ host, force: true, reason: e.message })
    }
    refresh()
  }

  if (!data) return <p className={error ? 'notice notice-error' : 'muted'}>{error || 'Loading…'}</p>

  return (
    <>
      <div className="page-head">
        <div className="page-title">
          <h1>Hosts</h1>
          {data.hosts.length > 0 && <p className="muted">{online} of {data.hosts.length} online</p>}
        </div>
        {data.hosts.length > 0 && (
          <input className="search" type="search" placeholder="Search hosts or targets" value={query}
            onChange={e => setQuery(e.target.value)} aria-label="Search" />
        )}
        <button className="btn btn-primary" disabled={data.tunnels.length === 0} onClick={() => setEditing('new')}>Add host</button>
      </div>
      {error && <p className="notice notice-error">{error}</p>}

      {data.hosts.length > 0 && data.tunnels.length > 1 && (
        <div className="chips" role="group" aria-label="Filter by tunnel">
          <button className="chip" aria-pressed={tunnelFilter === null} onClick={() => setTunnelFilter(null)}>All</button>
          {data.tunnels.map(t => (
            <button key={t.id} className="chip" aria-pressed={tunnelFilter === t.id} style={{ '--line': lineColor(t.id) } as any}
              onClick={() => setTunnelFilter(tunnelFilter === t.id ? null : t.id)}>
              <span className="chip-dot" aria-hidden="true" />{t.name}
              <span className="muted">{data.hosts.filter(h => h.tunnelId === t.id).length}</span>
            </button>
          ))}
        </div>
      )}

      {data.hosts.length === 0 ? (
        <div className="empty">
          <h2>{data.tunnels.length === 0 ? 'Two steps left to publish your first service' : 'One step left: publish your first service'}</h2>
          <ol className="setup">
            <li className="done">
              <strong>Connect Cloudflare</strong>
              <span>Done. The panel can now manage your domains.</span>
            </li>
            <li className={data.tunnels.length ? 'done' : 'next'}>
              <strong>Create a tunnel</strong>
              <span>The outbound connection between your network and Cloudflare. With it, there is no need to open ports on your router.</span>
              {data.tunnels.length === 0 && <a className="btn btn-primary" href="#/tunnels">Create a tunnel</a>}
            </li>
            <li className={data.tunnels.length ? 'next' : ''}>
              <strong>Add a host</strong>
              <span>Links a public domain, such as <code>app.yourdomain.com</code>, to a service on your local network.</span>
              {data.tunnels.length > 0 && <button className="btn btn-primary" onClick={() => setEditing('new')}>Add host</button>}
            </li>
          </ol>
        </div>
      ) : (
        <ul className="routes">
          {shown.map(h => {
            const t = tunnelsById.get(h.tunnelId)
            const st = hostState(h, t)
            return (
              <li key={h.id} className={`route${h.enabled ? '' : ' route-off'}`} style={{ '--line': lineColor(h.tunnelId) } as any}>
                <div className="route-main">
                  <span className="station" aria-hidden="true" />
                  <span className="route-from">
                    {h.hostname.startsWith('*')
                      ? <span>{h.hostname}</span>
                      : <a href={`https://${h.hostname}`} target="_blank" rel="noreferrer">{h.hostname}</a>}
                    {h.path && <span className="route-path">{h.path}</span>}
                  </span>
                  <span className="track" aria-hidden="true" />
                  <span className="station station-end" aria-hidden="true" />
                  <span className="route-to">{h.service}</span>
                </div>
                <div className="route-meta">
                  <span className="line-tag">{t?.name ?? 'deleted tunnel'}</span>
                  {h.access.enabled && (
                    <span className="tag" title={`Cloudflare Access: ${[...h.access.emails, ...h.access.emailDomains.map(d => '@' + d)].join(', ')}`}>
                      <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path fill="currentColor" d="M4 7V5a4 4 0 0 1 8 0v2h1v8H3V7h1Zm2 0h4V5a2 2 0 0 0-4 0v2Z" /></svg>
                      Sign-in required
                    </span>
                  )}
                  <span className={`state state-${st.cls}`} title={st.title}>{st.label}</span>
                  <span className="route-actions">
                    <Toggle checked={h.enabled} onChange={v => toggle(h, v)} label={h.enabled ? 'Disable host' : 'Enable host'} />
                    <button className="btn btn-small" onClick={() => setEditing(h)}>Edit</button>
                    <button className="btn btn-small btn-quiet-danger" onClick={() => setDeleting({ host: h, force: false })}>Delete</button>
                  </span>
                </div>
                {h.syncError && <p className="route-error">{h.syncError}</p>}
              </li>
            )
          })}
          {shown.length === 0 && <li className="muted">No hosts match the filter.</li>}
        </ul>
      )}

      {editing && (
        <HostForm host={editing === 'new' ? null : editing} tunnels={data.tunnels}
          onClose={() => setEditing(null)} onSaved={() => { setEditing(null); refresh() }} />
      )}
      {deleting && (
        <Confirm danger title={deleting.force ? 'Could not clean up in Cloudflare' : `Delete ${deleting.host.hostname}`}
          action={deleting.force ? 'Delete here only' : 'Delete host'}
          onClose={() => setDeleting(null)} onConfirm={remove}
          body={deleting.force
            ? <><p className="notice notice-error">{deleting.reason}</p><p>You can delete the host from this panel only. Anything left in Cloudflare (DNS record, Access application) has to be removed by hand.</p></>
            : <p>This removes the route from the tunnel and deletes its DNS record and Access application. The local service is not touched.</p>} />
      )}
    </>
  )
}

function parseService(service: string) {
  const m = service.match(/^(\w+):\/\/(\[[^\]]+\]|[^:/]+)(?::(\d+))?$/)
  if (m && (schemes as readonly string[]).includes(m[1])) return { scheme: m[1], target: m[2], port: m[3] ?? '', raw: '' }
  return { scheme: 'custom', target: '', port: '', raw: service }
}

const splitList = (s: string) => s.split(/[\s,;]+/).map(x => x.trim()).filter(Boolean)

function HostForm({ host, tunnels, onClose, onSaved }: { host: Host | null; tunnels: Tunnel[]; onClose: () => void; onSaved: () => void }) {
  const toast = useToast()
  const [zones, setZones] = useState<Zone[] | null>(null)
  const [zone, setZone] = useState(host?.zoneName ?? '')
  const [sub, setSub] = useState(() => {
    if (!host) return ''
    return host.hostname === host.zoneName ? '' : host.hostname.slice(0, -(host.zoneName.length + 1))
  })
  const [tunnelId, setTunnelId] = useState(host?.tunnelId ?? tunnels[0]?.id ?? 0)
  const svc = useMemo(() => (host ? parseService(host.service) : { scheme: 'http', target: '', port: '', raw: '' }), [host])
  const [scheme, setScheme] = useState(svc.scheme)
  const [target, setTarget] = useState(svc.target)
  const [port, setPort] = useState(svc.port)
  const [raw, setRaw] = useState(svc.raw)
  const [path, setPath] = useState(host?.path ?? '')
  const [opts, setOpts] = useState(host?.options ?? {})
  const [accessOn, setAccessOn] = useState(host?.access.enabled ?? false)
  const [emails, setEmails] = useState((host?.access.emails ?? []).join('\n'))
  const [domains, setDomains] = useState((host?.access.emailDomains ?? []).join('\n'))
  const [session, setSession] = useState(host?.access.sessionDuration || '24h')
  const [accessWarning, setAccessWarning] = useState('')
  const [conflict, setConflict] = useState<DnsRecord[] | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api.zones().then(z => {
      setZones(z)
      setZone(cur => cur || z[0]?.name || '')
    }).catch(e => setError(e.message))
  }, [])

  useEffect(() => {
    if (!accessOn) return setAccessWarning('')
    api.accessStatus().then(s => setAccessWarning(s.available ? '' : s.error)).catch(() => {})
  }, [accessOn])

  const hostname = (sub.trim() ? sub.trim() + '.' : '') + zone
  const service = scheme === 'custom' ? raw.trim() : `${scheme}://${target.trim()}${port ? ':' + port : ''}`
  useEffect(() => setConflict(null), [hostname, tunnelId])

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    const input: HostInput = {
      tunnelId, hostname, path: path.trim(), service, enabled: host?.enabled ?? true,
      options: { ...opts, noTLSVerify: scheme === 'https' && !!opts.noTLSVerify },
      access: { enabled: accessOn, emails: splitList(emails), emailDomains: splitList(domains), sessionDuration: session },
      overwriteDns: conflict !== null,
    }
    try {
      const saved = host ? await api.updateHost(host.id, input) : await api.createHost(input)
      if (saved.syncError) toast(`Saved, but not synced yet: ${saved.syncError}`, 'error')
      else toast(host ? `${saved.hostname} updated` : `${saved.hostname} published`)
      onSaved()
    } catch (e: any) {
      if (e instanceof ApiError && e.code === 'dns_conflict') setConflict(e.data.records ?? [])
      else setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const isHttp = scheme === 'http' || scheme === 'https'
  return (
    <Modal wide title={host ? `Edit ${host.hostname}` : 'Add host'} onClose={onClose}>
      <form onSubmit={submit}>
        <div className="modal-body">
          <fieldset>
            <legend>Public domain</legend>
            <div className="row">
              <Field label="Subdomain" hint="Leave empty for the root domain; use * for a wildcard.">
                <input value={sub} onChange={e => setSub(e.target.value)} placeholder="app" autoFocus spellCheck={false} autoCapitalize="off" />
              </Field>
              <Field label="Domain">
                <select value={zone} onChange={e => setZone(e.target.value)} required disabled={!zones}>
                  {!zones && <option value={zone}>{zone || 'Loading…'}</option>}
                  {zones?.map(z => <option key={z.id} value={z.name}>{z.name}</option>)}
                </select>
              </Field>
            </div>
            {zones?.length === 0 && <p className="notice notice-warn">Your token cannot see any zones. Check its Zone Resources in Cloudflare.</p>}
          </fieldset>

          <fieldset>
            <legend>Target service</legend>
            <div className="row row-service">
              <Field label="Protocol">
                <select value={scheme} onChange={e => setScheme(e.target.value)}>
                  {schemes.map(s => <option key={s} value={s}>{s}</option>)}
                  <option value="custom">other…</option>
                </select>
              </Field>
              {scheme === 'custom' ? (
                <Field label="Service" hint="cloudflared format, e.g. http_status:404 or unix:/run/app.sock">
                  <input value={raw} onChange={e => setRaw(e.target.value)} required spellCheck={false} />
                </Field>
              ) : (
                <>
                  <Field label="Host or IP">
                    <input value={target} onChange={e => setTarget(e.target.value)} placeholder="192.168.1.10" required spellCheck={false} autoCapitalize="off" />
                  </Field>
                  <Field label="Port">
                    <input value={port} onChange={e => setPort(e.target.value.replace(/\D/g, ''))} placeholder={scheme === 'https' ? '443' : scheme === 'http' ? '80' : ''}
                      inputMode="numeric" required={!isHttp} />
                  </Field>
                </>
              )}
            </div>
            <p className="field-hint">
              The address is resolved from wherever the tunnel's connector runs. Inside Docker, <code>localhost</code> is the
              container itself: use the machine's IP on your network or <code>host.docker.internal</code>.
            </p>
            <Field label="Tunnel">
              <select value={tunnelId} onChange={e => setTunnelId(Number(e.target.value))}>
                {tunnels.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
              </select>
            </Field>
          </fieldset>

          <fieldset>
            <legend>Access</legend>
            <label className="check">
              <input type="checkbox" checked={accessOn} onChange={e => setAccessOn(e.target.checked)} disabled={!!path.trim()} />
              <span>Require sign-in with Cloudflare Access before reaching the service</span>
            </label>
            {path.trim() && <p className="field-hint">Access protects the whole domain, so it is configured on the host without a path.</p>}
            {accessOn && (
              <>
                {accessWarning && (
                  <p className="notice notice-warn">
                    Access does not seem to be available with this token: {accessWarning}. Enable Zero Trust on your account (the free
                    plan is enough) and add the "Access: Apps and Policies → Edit" permission to the token.
                  </p>
                )}
                <div className="row">
                  <Field label="Allowed emails" hint="One per line.">
                    <textarea rows={3} value={emails} onChange={e => setEmails(e.target.value)} placeholder="you@example.com" spellCheck={false} />
                  </Field>
                  <Field label="Allowed email domains" hint="Any address at these domains.">
                    <textarea rows={3} value={domains} onChange={e => setDomains(e.target.value)} placeholder="example.org" spellCheck={false} />
                  </Field>
                </div>
                <Field label="Session duration">
                  <select value={session} onChange={e => setSession(e.target.value)}>
                    {sessions.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
                  </select>
                </Field>
              </>
            )}
          </fieldset>

          <details className="advanced" open={!!(path || opts.httpHostHeader || opts.originServerName || opts.http2Origin || opts.disableChunkedEncoding || opts.connectTimeout || opts.noTLSVerify)}>
            <summary>Advanced options</summary>
            <Field label="Path" hint="Regular expression. Only requests whose path matches go to this target, e.g. ^/api">
              <input value={path} onChange={e => { setPath(e.target.value); if (e.target.value.trim()) setAccessOn(false) }} spellCheck={false} />
            </Field>
            {scheme === 'https' && (
              <label className="check">
                <input type="checkbox" checked={!!opts.noTLSVerify} onChange={e => setOpts({ ...opts, noTLSVerify: e.target.checked })} />
                <span>Do not verify the target's certificate (self-signed certificates)</span>
              </label>
            )}
            {isHttp && (
              <>
                <div className="row">
                  <Field label="Host header" hint="Overrides the Host header the target receives.">
                    <input value={opts.httpHostHeader ?? ''} onChange={e => setOpts({ ...opts, httpHostHeader: e.target.value })} spellCheck={false} />
                  </Field>
                  <Field label="TLS server name" hint="Name expected in the target's certificate.">
                    <input value={opts.originServerName ?? ''} onChange={e => setOpts({ ...opts, originServerName: e.target.value })} spellCheck={false} />
                  </Field>
                </div>
                <label className="check">
                  <input type="checkbox" checked={!!opts.http2Origin} onChange={e => setOpts({ ...opts, http2Origin: e.target.checked })} />
                  <span>Use HTTP/2 to the target (required for gRPC)</span>
                </label>
                <label className="check">
                  <input type="checkbox" checked={!!opts.disableChunkedEncoding} onChange={e => setOpts({ ...opts, disableChunkedEncoding: e.target.checked })} />
                  <span>Disable chunked encoding (some WSGI servers need this)</span>
                </label>
              </>
            )}
            <Field label="Connection timeout (seconds)" hint="Leave empty for the cloudflared default (30).">
              <input className="input-short" inputMode="numeric" value={opts.connectTimeout || ''}
                onChange={e => setOpts({ ...opts, connectTimeout: Number(e.target.value.replace(/\D/g, '')) || 0 })} />
            </Field>
          </details>

          {conflict && (
            <div className="notice notice-warn">
              <p><strong>{hostname}</strong> already has DNS records pointing elsewhere:</p>
              <ul className="records">{conflict.map(r => <li key={r.id}><code>{r.type}</code> <code>{r.content}</code></li>)}</ul>
              <p>If you continue, they will be replaced with a record pointing to the tunnel and the domain will stop pointing there.</p>
            </div>
          )}
          {error && <p className="notice notice-error">{error}</p>}
        </div>
        <footer className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn-primary" disabled={busy || !zone}>
            {busy ? 'Applying in Cloudflare…' : conflict ? 'Replace DNS and save' : host ? 'Save changes' : 'Publish host'}
          </button>
        </footer>
      </form>
    </Modal>
  )
}
