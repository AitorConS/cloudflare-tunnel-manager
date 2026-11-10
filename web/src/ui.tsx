import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'

// ---- toasts ----

interface Toast { id: number; text: string; kind: 'ok' | 'error' }
const ToastCtx = createContext<(text: string, kind?: 'ok' | 'error') => void>(() => {})
export const useToast = () => useContext(ToastCtx)

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const push = useCallback((text: string, kind: 'ok' | 'error' = 'ok') => {
    const id = Date.now() + Math.random()
    setToasts(t => [...t, { id, text, kind }])
    setTimeout(() => setToasts(t => t.filter(x => x.id !== id)), kind === 'error' ? 8000 : 3500)
  }, [])
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {toasts.map(t => <div key={t.id} className={`toast toast-${t.kind}`}>{t.text}</div>)}
      </div>
    </ToastCtx.Provider>
  )
}

// ---- modal ----

export function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (d && !d.open) d.showModal()
  }, [])
  return (
    <dialog ref={ref} className={`modal${wide ? ' modal-wide' : ''}`} onCancel={e => { e.preventDefault(); onClose() }}>
      <header className="modal-head">
        <h2>{title}</h2>
        <button type="button" className="icon-btn" onClick={onClose} aria-label="Close">✕</button>
      </header>
      {children}
    </dialog>
  )
}

export function Confirm({ title, body, action, danger, onConfirm, onClose }: {
  title: string; body: ReactNode; action: string; danger?: boolean
  onConfirm: () => Promise<void> | void; onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  return (
    <Modal title={title} onClose={onClose}>
      <div className="modal-body">{body}</div>
      <footer className="modal-foot">
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button type="button" className={`btn ${danger ? 'btn-danger' : 'btn-primary'}`} disabled={busy}
          onClick={async () => { setBusy(true); try { await onConfirm() } finally { setBusy(false) } }}>
          {busy ? 'One moment…' : action}
        </button>
      </footer>
    </Modal>
  )
}

// ---- small controls ----

export function Toggle({ checked, onChange, label, disabled }: { checked: boolean; onChange: (v: boolean) => void; label: string; disabled?: boolean }) {
  return (
    <button type="button" role="switch" aria-checked={checked} aria-label={label} title={label} disabled={disabled}
      className={`toggle${checked ? ' on' : ''}`} onClick={() => onChange(!checked)}>
      <span />
    </button>
  )
}

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  )
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false)
  return (
    <button type="button" className="btn btn-small" onClick={async () => {
      try {
        await navigator.clipboard.writeText(text)
        setDone(true)
        setTimeout(() => setDone(false), 1500)
      } catch { /* clipboard unavailable over plain http: the text stays selectable */ }
    }}>{done ? 'Copied' : label}</button>
  )
}

export function timeAgo(unixSeconds: number): string {
  if (!unixSeconds) return 'never'
  const s = Math.max(0, Date.now() / 1000 - unixSeconds)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return `${Math.floor(s / 86400)} d ago`
}

/** Polls `load` every `ms` while the tab is visible; returns data, error and a manual refresh. */
export function usePolling<T>(load: () => Promise<T>, ms: number) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const loadRef = useRef(load)
  loadRef.current = load
  const refresh = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }, [])
  useEffect(() => {
    refresh()
    const timer = setInterval(() => { if (!document.hidden) refresh() }, ms)
    return () => clearInterval(timer)
  }, [refresh, ms])
  return { data, error, refresh }
}
