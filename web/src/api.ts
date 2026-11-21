import { reactive } from 'vue'

/** Roles, from the least to the most privileged ('' when signed out). */
export type Role = 'viewer' | 'operator' | 'admin'

const roleRanks: Record<string, number> = { viewer: 1, operator: 2, admin: 3 }

/** Whether role grants at least what min does. */
export function hasRole(role: string | undefined, min: Role): boolean {
  return (roleRanks[role ?? ''] ?? 0) >= roleRanks[min]
}

export const roleHelp: Record<Role, string> = {
  viewer: 'Reads the dashboard, jobs, logs and metrics; changes nothing',
  operator: 'Viewer, plus pausing or resuming targets, checking for updates and pulling the runner image',
  admin: 'Everything: targets, settings, users, updates',
}

export interface Status {
  version: string
  mode: 'none' | 'app' | 'pat'
  needsSetup: boolean
  setupUnlocked: boolean
  signedIn: boolean
  user: string
  role: Role | ''
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
  /** Memory reserved for its job (bytes). */
  predicted: number
}

/** Why a target's jobs wait. */
export type WaitReason = 'slot' | 'memory' | 'strict_slot'

export interface TargetState extends Target {
  status: string
  error?: string
  desired: number
  waiting: number
  waitReason?: WaitReason
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
  /** Reserved for strict jobs. */
  strictOnly?: boolean
  /** Memory reserved for the running job and what it uses now (bytes). */
  predicted?: number
  actual?: number
  /** Storage driver of the slot's Docker daemon, read when it starts (absent when unknown). */
  storageDriver?: string
}

/** Memory admission: jobs start only when their predicted memory fits. */
export interface MemoryState {
  enabled: boolean
  total: number
  reserve: number
  budget: number
  predicted: number
  actual: number
  committed: number
  waitingTarget?: number
  waitingBytes?: number
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
  memory?: MemoryState
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
  /** Resources the job used (null: not measured). */
  usage?: JobUsage | null
}

/** What one container used during a job; null fields are unknown. */
export interface ContainerUsage {
  memPeak: number | null
  cpuUsec: number | null
  cpuSomeUsec: number | null
  ioFullUsec: number | null
  memFullUsec: number | null
  cpuPressurePct: number | null
  ioPressurePct: number | null
  memPressurePct: number | null
}

export interface JobUsage {
  wallMs: number
  /** Runner peak + slot peak, bytes. */
  memPeak: number | null
  runner: ContainerUsage
  slot: ContainerUsage
}

/** GET /api/jobs/memory */
export interface MemorySizing {
  windowDays: number
  count: number
  p50: number | null
  p90: number | null
  max: number | null
  memTotal: number
  usable: number
  suggestedSlots: number | null
  slots: number
  memoryLimitMb: number
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
  historyDays: number
  baseUrl: string
  actionsCache: boolean
  actionsCacheGb: number
  /** Share of the actions cache size one owner may use, in percent. */
  actionsCacheOwnerPct: number
  // Read-only; missing when the cache listener is not running.
  actionsCacheStats?: ActionsCacheStats
  memoryAdmission: boolean
  memoryReserveMb: number
  /** -1: one slot while a strict target is enabled. */
  strictSlots: number
  // Read-only: slots reserved for strict jobs right now.
  strictSlotsActive: number
}

export interface ActionsCacheStats {
  entries: number
  bytes: number
  limitBytes: number
  /** Most one owner may store (0 without a limit); optional for older servers. */
  ownerLimitBytes?: number
  hits: number
  misses: number
  saves: number
  evictions: number
}

// One line for Settings: what the actions cache stores and how it is used.
export function cacheSummary(st: ActionsCacheStats): string {
  const lookups = st.hits + st.misses
  const rate = lookups ? ` (${Math.round((100 * st.hits) / lookups)}% hits)` : ''
  return `${bytes(st.bytes)} in ${st.entries} ${st.entries === 1 ? 'entry' : 'entries'}. ` +
    `Since start: ${st.hits} restored, ${st.misses} missed${rate}, ${st.saves} saved, ${st.evictions} evicted.`
}

export interface Installation {
  id: number
  account: string
  type: 'User' | 'Organization'
  repos: { name: string; fullName: string; private: boolean }[]
  error?: string
}

export interface User {
  login: string
  role: Role
  createdAt: number
  createdBy: string
}

/** Grants role to the members of an organization (team '') or of a team. */
export interface AccessRule {
  id: number
  org: string
  team: string
  role: Role
  createdAt: number
  createdBy: string
}

/** GET /api/users */
export interface Access {
  users: User[]
  rules: AccessRule[]
}

export interface AuditEntry {
  id: number
  at: number
  actor: string
  action: string
  subject: string
  detail: string
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
  const s = Date.now() / 1000 - unix
  // Past two days, hours and minutes are noise ("48h 0m ago").
  if (s >= 2 * 86400) return `${Math.floor(s / 86400)} days ago`
  return duration(s) + ' ago'
}

export function bytes(n: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / 1024 ** i).toFixed(i >= 3 ? 1 : 0)} ${u[i]}`
}

export const waitLabel: Record<WaitReason, string> = {
  slot: 'waiting for a slot',
  memory: 'waiting for memory',
  strict_slot: 'waiting for a strict slot',
}

export const waitTitle: Record<WaitReason, string> = {
  slot: 'Every slot is busy.',
  memory: 'A slot is free, but the memory predicted for this job does not fit next to the running jobs. It starts when one of them finishes.',
  strict_slot: 'Strict jobs only use the slots reserved for them, or an empty slot, so they never wipe a warm cache. One will be free shortly.',
}

/** Why a job that has not started yet waits ('' when unknown or not waiting). */
export function jobWait(j: { targetId: number; startedAt: number; finishedAt: number }): WaitReason | '' {
  if (j.startedAt || j.finishedAt) return ''
  const t = live.snap?.targets.find((x) => x.id === j.targetId)
  return t && t.waiting > 0 ? (t.waitReason ?? 'slot') : ''
}

/** Storage drivers that use the kernel's overlay filesystem (overlayfs is the containerd snapshotter's name). */
const FAST_STORAGE_DRIVERS = ['overlay2', 'overlayfs']

/** The tooltip of a slot whose daemon uses a slow storage driver, or '' when the driver is fine or unknown. */
export function slowStorageTitle(driver?: string): string {
  if (!driver || FAST_STORAGE_DRIVERS.includes(driver)) return ''
  return `This slot's Docker daemon uses the ${driver} storage driver instead of overlay2: jobs with heavy file or layer work run much slower. See Troubleshooting in the docs.`
}

export function targetName(t: { kind: string; owner: string; repo?: string }) {
  return t.kind === 'repo' ? `${t.owner}/${t.repo}` : `${t.owner} (organization)`
}
