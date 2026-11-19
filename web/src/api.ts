export interface Session {
  token: string
  username: string
  livekitUrl: string
}

export interface DeviceStatus {
  id: string
  name: string
  online: boolean
  busy: boolean
  lastSeen?: string
}

export interface CallRecord {
  id: string
  caller: string
  callee: string
  startedAt: string
  answeredAt?: string
  endedAt?: string
  endReason?: string
}

const STORAGE_KEY = 'nexcall.session'

export function loadSession(): Session | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw ? (JSON.parse(raw) as Session) : null
  } catch {
    return null
  }
}

export function saveSession(s: Session | null) {
  if (s) localStorage.setItem(STORAGE_KEY, JSON.stringify(s))
  else localStorage.removeItem(STORAGE_KEY)
}

export class UnauthorizedError extends Error {}

async function request<T>(path: string, init: RequestInit = {}, token?: string): Promise<T> {
  const headers = new Headers(init.headers)
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')
  const resp = await fetch(path, { ...init, headers })
  if (resp.status === 401) throw new UnauthorizedError('unauthorized')
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) throw new Error(data.error ?? `HTTP ${resp.status}`)
  return data as T
}

export function login(username: string, password: string) {
  return request<Session>('/api/login', { method: 'POST', body: JSON.stringify({ username, password }) })
}

export function me(token: string) {
  return request<{ username: string; livekitUrl: string }>('/api/me', {}, token)
}

export function listCalls(token: string, limit = 30) {
  return request<CallRecord[]>(`/api/calls?limit=${limit}`, {}, token)
}
