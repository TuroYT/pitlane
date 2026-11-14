import { reactive } from 'vue'

export interface Status {
  version: string
  mode: 'none' | 'app' | 'pat'
  needsSetup: boolean
  setupUnlocked: boolean
  signedIn: boolean
  user: string
  baseUrl: string
  app: { slug: string; name: string; htmlUrl: string; owner: string }
}

export interface Target {
  id: number
  kind: 'repo' | 'org'
  owner: string
  repo?: string
  installationId: number
  enabled: boolean
  minRunners: number
  maxRunners: number
  scaleSetName: string
  scaleSetId: number
  strict: boolean
  maxJobMinutes: number
  /** Why the target must run strict (set by the server from GitHub). */
  strictRequired?: string
  /** When Pitlane switched the target to strict on its own (Unix time). */
  strictForcedAt?: number
}

export interface RunnerState {
  name: string
  slot: number
  busy: boolean
  jobName: string
  repo: string
  startedAt: number
}

export interface TargetState extends Target {
  status: string
  error?: string
  desired: number
  waiting: number
  runners: RunnerState[]
}

export interface Slot {
  index: number
  status: 'starting' | 'ready' | 'busy' | 'cleaning' | 'error' | 'retiring'
  owner: string
  targetId: number
  runner: string
  error?: string
  since: number
}

export interface Host {
  cpus: number
  cpuPercent: number
  memTotal: number
  memAvailable: number
  diskTotal: number
  diskFree: number
}

export interface Snapshot {
  targets: TargetState[]
  slots: Slot[]
  host: Host
  time: number
}

export interface Job {
  id: number
  targetId: number
  jobId: string
  runId: number
  owner: string
  repo: string
  workflowRef: string
  jobName: string
  event: string
  runnerName: string
  slot: number
  queuedAt: number
  startedAt: number
  finishedAt: number
  result: string
  /** Full workflow reference, e.g. "owner/repo/.github/workflows/ci.yml@refs/heads/main" ('' for old jobs). */
  workflowFullRef?: string
  /** Git ref of the workflow: "refs/heads/main", "refs/pull/42/merge", "refs/tags/v1"… */
  ref?: string
}

export interface Settings {
  slots: number
  runnerImage: string
  dindImage: string
  cpuShares: number
  memoryMb: number
  cacheScope: 'owner' | 'shared'
  mirror: boolean
  qemu: boolean
  keepBuildGb: number
  minFreeDiskGb: number
  metricsToken?: string
  metricsTokenSet: boolean
  allowedUsers: string
  historyDays: number
  baseUrl: string
}

export interface Installation {
  id: number
  account: string
  type: 'User' | 'Organization'
  repos: { name: string; fullName: string; private: boolean }[]
  error?: string
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

// Called when the session is gone (expired, signed out elsewhere, user
// removed): App.vue sends the user to the login page.
let unauthorized: () => void = () => {}

/** Sets what happens when an API call reports that the session is gone. */
export function onUnauthorized(fn: () => void) {
  unauthorized = fn
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await request(path, init)
  return res.json() as Promise<T>
}

/** A page of a list endpoint that reports its total in X-Total-Count. */
export async function apiPage<T>(path: string): Promise<{ items: T[]; total: number }> {
  const res = await request(path)
  const items = (await res.json()) as T[]
  const total = Number(res.headers.get('X-Total-Count'))
  return { items, total: Number.isFinite(total) && total >= items.length ? total : items.length }
}

async function request(path: string, init: RequestInit = {}): Promise<Response> {
  const headers: Record<string, string> = { 'X-Pitlane': '1' }
  if (init.body) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, { ...init, headers: { ...headers, ...(init.headers as Record<string, string>) } })
  if (res.status === 401) unauthorized()
  if (!res.ok) {
    let msg = res.statusText
    try {
      msg = (await res.json()).error ?? msg
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg)
  }
  return res
}

export const post = <T>(path: string, body: unknown, method = 'POST') =>
  api<T>(path, { method, body: JSON.stringify(body) })

// ------------------------------------------------------------- live state

export const live = reactive<{ snap: Snapshot | null; connected: boolean }>({ snap: null, connected: false })

let source: EventSource | null = null
let retry: ReturnType<typeof setTimeout> | undefined

/** Delay before reopening an event stream the browser gave up on. */
export const LIVE_RETRY_MS = 5000

export function connectLive() {
  if (source) return
  clearTimeout(retry)
  const es = new EventSource('/api/events')
  source = es
  es.onopen = () => (live.connected = true)
  es.onmessage = (e) => {
    live.snap = JSON.parse(e.data)
    live.connected = true
  }
  es.onerror = () => {
    live.connected = false
    // The browser retries network errors on its own, but closes the stream
    // for good on an HTTP error such as 401 (session expired): find out why.
    if (es.readyState === EventSource.CLOSED && source === es) {
      source = null
      void streamClosed()
    }
  }
}

async function streamClosed() {
  try {
    const s = await api<{ signedIn: boolean }>('/api/status')
    if (!s.signedIn) {
      unauthorized()
      return
    }
  } catch {
    /* Pitlane unreachable (restarting): retry below */
  }
  retry = setTimeout(connectLive, LIVE_RETRY_MS)
}

export function disconnectLive() {
  clearTimeout(retry)
  source?.close()
  source = null
}

// ---------------------------------------------------------------- format

export function duration(seconds: number): string {
  if (!isFinite(seconds) || seconds < 0) return '—'
  if (seconds < 60) return `${Math.round(seconds)}s`
  const m = Math.floor(seconds / 60)
  const s = Math.round(seconds % 60)
  if (m < 60) return s ? `${m}m ${s}s` : `${m}m`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

export function ago(unix: number): string {
  if (!unix) return '—'
  return duration(Date.now() / 1000 - unix) + ' ago'
}

export function bytes(n: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / 1024 ** i).toFixed(i >= 3 ? 1 : 0)} ${u[i]}`
}

export function targetName(t: { kind: string; owner: string; repo?: string }) {
  return t.kind === 'repo' ? `${t.owner}/${t.repo}` : `${t.owner} (organization)`
}
