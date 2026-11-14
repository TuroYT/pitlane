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

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = { 'X-Pitlane': '1' }
  if (init.body) headers['Content-Type'] = 'application/json'
  const res = await fetch(path, { ...init, headers: { ...headers, ...(init.headers as Record<string, string>) } })
  if (!res.ok) {
    let msg = res.statusText
    try {
      msg = (await res.json()).error ?? msg
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg)
  }
  return res.json() as Promise<T>
}

export const post = <T>(path: string, body: unknown, method = 'POST') =>
  api<T>(path, { method, body: JSON.stringify(body) })

// ------------------------------------------------------------- live state

export const live = reactive<{ snap: Snapshot | null; connected: boolean }>({ snap: null, connected: false })

let source: EventSource | null = null

export function connectLive() {
  if (source) return
  source = new EventSource('/api/events')
  source.onopen = () => (live.connected = true)
  source.onmessage = (e) => {
    live.snap = JSON.parse(e.data)
    live.connected = true
  }
  source.onerror = () => {
    live.connected = false
  }
}

export function disconnectLive() {
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
