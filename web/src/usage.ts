import { bytes, duration, type ContainerUsage, type Job } from './api'

// Formatting of the resources a job used (job.usage), for the job table.

const parts = (u: NonNullable<Job['usage']>) => [
  ['runner', u.runner],
  ['slot', u.slot],
] as const

/** Peak memory of the job (runner + slot). */
export function peakRam(j: Job): string {
  const v = j.usage?.memPeak
  return v != null ? bytes(v) : '—'
}

export function peakRamTitle(j: Job): string {
  const u = j.usage
  if (!u) return 'Not measured'
  const each = parts(u)
    .filter(([, c]) => c.memPeak != null)
    .map(([name, c]) => `${name} ${bytes(c.memPeak!)}`)
  return `Peak memory: ${each.join(' + ')} (page cache included; both peaks need not coincide)`
}

function cpuSeconds(c: ContainerUsage): number | null {
  return c.cpuUsec != null ? c.cpuUsec / 1e6 : null
}

/** CPU time of the job (runner + slot). */
export function cpuTime(j: Job): string {
  const u = j.usage
  if (!u) return '—'
  const s = parts(u).map(([, c]) => cpuSeconds(c)).filter((v): v is number => v != null)
  return s.length ? duration(s.reduce((a, b) => a + b, 0)) : '—'
}

export function cpuTimeTitle(j: Job): string {
  const u = j.usage
  if (!u) return 'Not measured'
  const wall = u.wallMs / 1000
  const each = parts(u)
    .filter(([, c]) => c.cpuUsec != null)
    .map(([name, c]) => {
      const s = cpuSeconds(c)!
      return `${name} ${duration(s)}${wall > 0 ? ` (${(s / wall).toFixed(1)} CPUs on average)` : ''}`
    })
  return `CPU time over ${duration(wall)}: ${each.join(', ')}`
}

function pct(v: number | null | undefined): string {
  return v != null ? `${v < 10 ? v.toFixed(1) : Math.round(v)}%` : '—'
}

type PressureKey = 'cpuPressurePct' | 'ioPressurePct' | 'memPressurePct'

/** The higher pressure of the runner and the slot (null: unknown). */
export function worstPressure(j: Job, key: PressureKey): number | null {
  const u = j.usage
  if (!u) return null
  const v = [u.runner[key], u.slot[key]].filter((x): x is number => x != null)
  return v.length ? Math.max(...v) : null
}

/** "cpu 12% · io 3.0% · mem 0.0%", or '—' when not measured. */
export function pressure(j: Job): string {
  if (!j.usage) return '—'
  return `cpu ${pct(worstPressure(j, 'cpuPressurePct'))} · io ${pct(worstPressure(j, 'ioPressurePct'))} · mem ${pct(worstPressure(j, 'memPressurePct'))}`
}

export function pressureTitle(j: Job): string {
  const u = j.usage
  if (!u) return 'Not measured'
  const line = (name: string, c: ContainerUsage) =>
    `${name}: cpu ${pct(c.cpuPressurePct)}, io ${pct(c.ioPressurePct)}, memory ${pct(c.memPressurePct)}`
  return (
    'Share of the job\'s time spent stalled (Linux PSI): CPU = some task waited for a CPU, ' +
    'I/O and memory = every task was stalled.\n' +
    parts(u)
      .map(([name, c]) => line(name, c))
      .join('\n')
  )
}
