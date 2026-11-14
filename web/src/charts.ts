import { ref } from 'vue'
import {
  Chart,
  BarController,
  BarElement,
  LineController,
  LineElement,
  PointElement,
  CategoryScale,
  LinearScale,
  Filler,
  Tooltip,
} from 'chart.js'
import { duration, type Job } from './api'
import { jobState } from './source'

// Only what the dashboard draws: bar and line charts on category/linear axes.
Chart.register(BarController, BarElement, LineController, LineElement, PointElement, CategoryScale, LinearScale, Filler, Tooltip)

// ------------------------------------------------------------------ theme

export interface Theme {
  text: string
  muted: string
  grid: string
  bg: string
  border: string
  ok: string
  error: string
  neutral: string
  pitlane: string
  hosted: string
  p50: string
  p90: string
}

function readTheme(): Theme {
  const cs = getComputedStyle(document.documentElement)
  const v = (n: string) => cs.getPropertyValue(n).trim()
  return {
    text: v('--text'),
    muted: v('--muted'),
    grid: v('--chart-grid'),
    bg: v('--bg'),
    border: v('--border'),
    ok: v('--ok'),
    error: v('--error'),
    neutral: v('--neutral'),
    pitlane: v('--chart-pitlane'),
    hosted: v('--chart-hosted'),
    p50: v('--chart-p50'),
    p90: v('--chart-p90'),
  }
}

/** Current chart colors; updated when the system switches between light and dark. */
export const theme = ref<Theme>(readTheme())
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  // CSS variables switch first; read them on the next frame.
  requestAnimationFrame(() => (theme.value = readTheme()))
})

/** Shared options: GitHub-like, quiet axes, tooltips in the page's colors. */
export function baseOptions(t: Theme, yFormat?: (v: number) => string) {
  Chart.defaults.font.family = getComputedStyle(document.body).fontFamily
  Chart.defaults.font.size = 12
  Chart.defaults.color = t.muted
  return {
    responsive: true,
    maintainAspectRatio: false,
    animation: { duration: 250 },
    interaction: { mode: 'index' as const, intersect: false },
    plugins: {
      legend: { display: false },
      tooltip: {
        backgroundColor: t.bg,
        titleColor: t.text,
        bodyColor: t.text,
        borderColor: t.border,
        borderWidth: 1,
        padding: 10,
        boxPadding: 4,
        usePointStyle: true,
        callbacks: yFormat
          ? { label: (c: { dataset: { label?: string }; parsed: { y: number | null; x: number | null } }) => {
              const val = c.parsed.y
              return `${c.dataset.label}: ${val == null ? '—' : yFormat(val)}`
            } }
          : {},
      },
    },
    scales: {
      x: { grid: { display: false }, border: { color: t.grid }, ticks: { maxRotation: 0, autoSkipPadding: 12 } },
      y: {
        beginAtZero: true,
        grid: { color: t.grid },
        border: { display: false },
        ticks: { maxTicksLimit: 5, callback: yFormat ? (v: number | string) => yFormat(Number(v)) : undefined },
      },
    },
  }
}

export const fmtSeconds = (v: number) => (v === 0 ? '0' : duration(v))
export const fmtPercent = (v: number) => `${Math.round(v)}%`

// ------------------------------------------------------------------ series

export type Range = '24h' | '7d' | '30d'

export interface Buckets {
  start: number
  size: number
  n: number
  labels: string[]
  starts: number[]
}

/** Hourly buckets over the last 24 hours, daily buckets over 7 or 30 days. */
export function buckets(range: Range, now = Date.now() / 1000): Buckets {
  const starts: number[] = []
  const labels: string[] = []
  if (range === '24h') {
    const size = 3600
    const last = Math.floor(now / size) * size
    for (let i = 23; i >= 0; i--) {
      const s = last - i * size
      starts.push(s)
      labels.push(new Date(s * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }))
    }
    return { start: starts[0], size, n: 24, labels, starts }
  }
  const n = range === '7d' ? 7 : 30
  const d = new Date(now * 1000)
  d.setHours(0, 0, 0, 0)
  for (let i = n - 1; i >= 0; i--) {
    const day = new Date(d)
    day.setDate(d.getDate() - i)
    starts.push(day.getTime() / 1000)
    labels.push(day.toLocaleDateString([], { month: 'short', day: 'numeric' }))
  }
  return { start: starts[0], size: 86400, n, labels, starts }
}

function index(b: Buckets, t: number): number {
  if (!t || t < b.start) return -1
  // Daily buckets follow local midnights (DST-safe), hourly ones are regular.
  for (let i = b.n - 1; i >= 0; i--) if (t >= b.starts[i]) return i
  return -1
}

/** Nearest-rank quantile of an unsorted list, or null when empty. */
export function quantile(values: number[], q: number): number | null {
  if (!values.length) return null
  const a = [...values].sort((x, y) => x - y)
  return a[Math.max(0, Math.ceil(q * a.length) - 1)]
}

/** Finished jobs per bucket and result. */
export function jobsByResult(jobs: Job[], b: Buckets) {
  const out = { succeeded: Array(b.n).fill(0), failed: Array(b.n).fill(0), canceled: Array(b.n).fill(0) }
  for (const j of jobs) {
    const i = index(b, j.finishedAt)
    if (i < 0) continue
    const s = jobState(j)
    if (s === 'failed') out.failed[i]++
    else if (s === 'canceled') out.canceled[i]++
    else out.succeeded[i]++
  }
  return out
}

/** p50 and p90 of a per-job value, grouped by the bucket of a timestamp. */
export function percentiles(jobs: Job[], b: Buckets, at: (j: Job) => number, value: (j: Job) => number | null) {
  const groups: number[][] = Array.from({ length: b.n }, () => [])
  for (const j of jobs) {
    const i = index(b, at(j))
    const v = value(j)
    if (i >= 0 && v != null && v >= 0) groups[i].push(v)
  }
  return { p50: groups.map((g) => quantile(g, 0.5)), p90: groups.map((g) => quantile(g, 0.9)) }
}

/** Busy slot time over available slot time per bucket, in percent. */
export function utilisation(jobs: Job[], b: Buckets, slots: number, now = Date.now() / 1000): (number | null)[] {
  const busy = Array(b.n).fill(0)
  for (const j of jobs) {
    if (!j.startedAt) continue
    const end = j.finishedAt || now
    for (let i = 0; i < b.n; i++) {
      const s = b.starts[i]
      const e = i + 1 < b.n ? b.starts[i + 1] : s + b.size
      const o = Math.min(end, e) - Math.max(j.startedAt, s)
      if (o > 0) busy[i] += o
    }
  }
  return busy.map((t, i) => {
    const s = b.starts[i]
    const e = Math.min(now, i + 1 < b.n ? b.starts[i + 1] : s + b.size)
    const span = (e - s) * slots
    return span > 0 && slots > 0 ? Math.min(100, (t / span) * 100) : null
  })
}
