// Typed client for the manager's JSON API.

export interface User { id: number; username: string }
export interface CloudflareInfo { configured: boolean; accountId: string; accountName: string }
export interface Status {
  setupRequired: boolean
  user: User | null
  version: string
  cloudflare?: CloudflareInfo
  cloudflared?: { available: boolean; version: string }
}
export interface PermCheck { key: string; ok: boolean; detail: string; optional: boolean }
export interface UpdateInfo { current: string; latest: string; url: string; updateAvailable: boolean; canSelfUpdate: boolean; why: string }
export interface Account { id: string; name: string }
export interface Zone { id: string; name: string; status: string }

export interface Connector {
  id: string
  version: string
  arch: string
  conns: { colo_name: string; origin_ip: string; opened_at: string }[] | null
}
export interface Tunnel {
  id: number
  cfId: string
  name: string
  mode: 'managed' | 'external'
  enabled: boolean
  createdAt: number
  lastSyncAt: number
  lastSyncError: string
  hostCount: number
  runner: { state: 'stopped' | 'running' | 'restarting'; startedAt: number; restarts: number; lastError: string }
  remote: { status: string; connectors: Connector[]; error?: string } | null
}

export interface OriginOptions {
  noTLSVerify?: boolean
  httpHostHeader?: string
  originServerName?: string
  http2Origin?: boolean
  disableChunkedEncoding?: boolean
  connectTimeout?: number
}
export interface AccessConfig {
  enabled: boolean
  emails: string[]
  emailDomains: string[]
  sessionDuration: string
}
export interface HostInput {
  tunnelId: number
  hostname: string
  path: string
  service: string
  enabled: boolean
  options: OriginOptions
  access: AccessConfig
  overwriteDns?: boolean
}
export interface Host extends HostInput {
  id: number
  zoneName: string
  accessAppId: string
  syncError: string
}
export interface DnsRecord { id: string; type: string; name: string; content: string }
export interface Metrics {
  available: boolean
  totalRequests: number
  requestErrors: number
  concurrentRequests: number
  haConnections: number
  responseCodes: Record<string, number>
}
export interface LogLine { seq: number; t: number; text: string }

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string, public data: any) {
    super(message)
  }
}

let onUnauthorized = () => {}
export function setUnauthorizedHandler(fn: () => void) { onUnauthorized = fn }

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response
  try {
    res = await fetch('/api' + path, {
      method,
      credentials: 'same-origin',
      headers: { 'X-CTM': '1', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
  } catch {
    throw new ApiError(0, 'network', 'Could not reach the server. Check that it is running and try again.', null)
  }
  let data: any = null
  try { data = await res.json() } catch { /* empty or non-JSON body */ }
  if (!res.ok) {
    if (res.status === 401 && data?.code === 'unauthorized') onUnauthorized()
    throw new ApiError(res.status, data?.code ?? 'error', data?.error ?? `Error ${res.status}`, data)
  }
  return data as T
}

export const api = {
  status: () => call<Status>('GET', '/status'),
  setup: (username: string, password: string) => call<{ user: User }>('POST', '/setup', { username, password }),
  login: (username: string, password: string) => call<{ user: User }>('POST', '/login', { username, password }),
  logout: () => call('POST', '/logout'),
  changePassword: (current: string, next: string) => call('PUT', '/password', { current, new: next }),
  configureCloudflare: (token: string, accountId = '') =>
    call<{ account: Account | null; accounts: Account[] | null }>('PUT', '/cloudflare', { token, accountId }),
  checkCloudflare: () => call<PermCheck[]>('GET', '/cloudflare/check'),
  checkUpdate: (fresh = false) => call<UpdateInfo>('GET', `/update${fresh ? '?fresh=1' : ''}`),
  applyUpdate: () => call<{ version: string }>('POST', '/update'),
  zones: () => call<Zone[]>('GET', '/zones'),
  accessStatus: () => call<{ available: boolean; error: string }>('GET', '/access-status'),

  tunnels: () => call<Tunnel[]>('GET', '/tunnels'),
  createTunnel: (name: string, mode: string) => call<Tunnel>('POST', '/tunnels', { name, mode }),
  setTunnelRun: (id: number, mode: string, enabled: boolean) => call('PATCH', `/tunnels/${id}`, { mode, enabled }),
  restartTunnel: (id: number) => call('POST', `/tunnels/${id}/restart`),
  syncTunnel: (id: number) => call<{ hosts: number; errors: string[] }>('POST', `/tunnels/${id}/sync`),
  deleteTunnel: (id: number, force = false) => call('DELETE', `/tunnels/${id}${force ? '?force=1' : ''}`),
  tunnelToken: (id: number) => call<{ token: string }>('GET', `/tunnels/${id}/token`),
  tunnelMetrics: (id: number) => call<Metrics>('GET', `/tunnels/${id}/metrics`),

  hosts: () => call<Host[]>('GET', '/hosts'),
  createHost: (h: HostInput) => call<Host>('POST', '/hosts', h),
  updateHost: (id: number, h: HostInput) => call<Host>('PUT', `/hosts/${id}`, h),
  setHostEnabled: (id: number, enabled: boolean) => call<Host>('PATCH', `/hosts/${id}`, { enabled }),
  deleteHost: (id: number, force = false) => call('DELETE', `/hosts/${id}${force ? '?force=1' : ''}`),
}

// Each tunnel gets a line colour, like a line on a transit map; hosts carry
// the colour of the tunnel that serves them.
const LINES = ['#e8590c', '#1971c2', '#2f9e44', '#9c36b5', '#0c8599', '#c2255c', '#5f3dc4', '#b08900']
export const lineColor = (tunnelId: number) => LINES[(tunnelId - 1 + LINES.length * 1000) % LINES.length]
