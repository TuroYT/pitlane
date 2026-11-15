import { describe, expect, it } from 'vitest'
import type { Job } from './api'
import { buckets, jobsByResult, niceStep, percentiles, quantile, utilisation } from './charts'

const sec = (d: Date) => d.getTime() / 1000
// Built from local components so the tests do not depend on the timezone.
const now = sec(new Date(2025, 5, 15, 12, 30))
const at = (h: number, mi = 0) => sec(new Date(2025, 5, 15, h, mi))
const sum = (a: number[]) => a.reduce((x, y) => x + y, 0)

function job(p: Partial<Job>): Job {
  return {
    id: 1, targetId: 1, jobId: 'j', runId: 1, owner: 'o', repo: 'r', workflowRef: 'ci.yml', jobName: 'build', event: 'push',
    runnerName: 'pitlane-x', slot: 0, queuedAt: 1, startedAt: 2, finishedAt: 3, result: 'succeeded', ...p,
  }
}

describe('niceStep', () => {
  it('picks the smallest step giving at most 4 intervals', () => {
    expect(niceStep(0)).toBe(1)
    expect(niceStep(4)).toBe(1)
    expect(niceStep(5)).toBe(2)
    expect(niceStep(40)).toBe(10)
    expect(niceStep(41)).toBe(15)
    expect(niceStep(240)).toBe(60)
    expect(niceStep(3600 * 4)).toBe(3600)
  })

  it('returns undefined past the largest step', () => {
    expect(niceStep(14400 * 4)).toBe(14400)
    expect(niceStep(14400 * 4 + 1)).toBeUndefined()
  })
})

describe('quantile', () => {
  it('returns null on an empty list', () => {
    expect(quantile([], 0.5)).toBeNull()
  })

  it('uses nearest-rank', () => {
    const v = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
    expect(quantile(v, 0.5)).toBe(5)
    expect(quantile(v, 0.9)).toBe(9)
    expect(quantile(v, 1)).toBe(10)
    expect(quantile(v, 0)).toBe(1)
  })

  it('returns the only value of a single-item list', () => {
    expect(quantile([7], 0.5)).toBe(7)
    expect(quantile([7], 0.9)).toBe(7)
  })

  it('sorts numerically without mutating its input', () => {
    const v = [10, 2, 33, 1]
    expect(quantile(v, 0.5)).toBe(2)
    expect(quantile(v, 1)).toBe(33)
    expect(v).toEqual([10, 2, 33, 1])
  })
})

describe('buckets', () => {
  it('builds 24 hourly buckets aligned on the hour', () => {
    const b = buckets('24h', now)
    expect(b.n).toBe(24)
    expect(b.size).toBe(3600)
    expect(b.starts).toHaveLength(24)
    expect(b.labels).toHaveLength(24)
    expect(b.start).toBe(b.starts[0])
    expect(b.starts[23]).toBe(at(12))
    for (let i = 1; i < 24; i++) expect(b.starts[i] - b.starts[i - 1]).toBe(3600)
  })

  it.each([['7d', 7], ['30d', 30]] as const)('builds %s daily buckets at local midnight', (range, n) => {
    const b = buckets(range, now)
    expect(b.n).toBe(n)
    expect(b.size).toBe(86400)
    expect(b.starts).toHaveLength(n)
    expect(b.labels).toHaveLength(n)
    expect(b.starts[n - 1]).toBe(sec(new Date(2025, 5, 15)))
    expect(b.starts[0]).toBe(sec(new Date(2025, 5, 15 - (n - 1))))
    for (const s of b.starts) {
      const d = new Date(s * 1000)
      expect([d.getHours(), d.getMinutes(), d.getSeconds()]).toEqual([0, 0, 0])
    }
  })

  it('keeps midnight alignment across a month boundary', () => {
    const b = buckets('7d', sec(new Date(2025, 6, 2, 8)))
    expect(b.starts[0]).toBe(sec(new Date(2025, 5, 26)))
    expect(b.starts[6]).toBe(sec(new Date(2025, 6, 2)))
  })
})

describe('jobsByResult', () => {
  const b = buckets('24h', now)

  it('counts finished jobs in the bucket of their end time, by result', () => {
    const out = jobsByResult(
      [
        job({ finishedAt: at(12, 10), result: 'succeeded' }),
        job({ finishedAt: at(12, 50), result: 'success' }),
        job({ finishedAt: at(12, 5), result: 'failed' }),
        job({ finishedAt: at(11, 59), result: 'canceled' }),
        job({ finishedAt: at(11, 0), result: 'cancelled' }),
        job({ finishedAt: at(9, 0), result: 'weird' }),
      ],
      b,
    )
    expect(out.succeeded).toHaveLength(24)
    expect(out.succeeded[23]).toBe(2)
    expect(out.failed[23]).toBe(1)
    expect(out.canceled[22]).toBe(2)
    // Unknown results count as succeeded.
    expect(out.succeeded[20]).toBe(1)
    expect(sum(out.succeeded)).toBe(3)
  })

  it('ignores jobs not finished or finished before the first bucket', () => {
    const out = jobsByResult([job({ finishedAt: 0, result: '' }), job({ finishedAt: b.start - 1 })], b)
    expect(sum(out.succeeded) + sum(out.failed) + sum(out.canceled)).toBe(0)
  })

  it('counts a job finished exactly at the first bucket start', () => {
    const out = jobsByResult([job({ finishedAt: b.start })], b)
    expect(out.succeeded[0]).toBe(1)
  })
})

describe('percentiles', () => {
  const b = buckets('24h', now)
  const finished = (j: Job) => j.finishedAt
  const wait = (j: Job) => j.startedAt - j.queuedAt

  it('computes p50 and p90 per bucket, with one entry per bucket', () => {
    const jobs = [10, 20, 30, 40, 50, 60, 70, 80, 90, 100].map((w) =>
      job({ queuedAt: at(12), startedAt: at(12) + w, finishedAt: at(12, 20) }),
    )
    const out = percentiles(jobs, b, finished, wait)
    expect(out.p50).toHaveLength(24)
    expect(out.p90).toHaveLength(24)
    expect(out.p50[23]).toBe(50)
    expect(out.p90[23]).toBe(90)
  })

  it('returns null for empty buckets', () => {
    const out = percentiles([job({ queuedAt: 0, startedAt: 5, finishedAt: at(12, 1) })], b, finished, wait)
    expect(out.p50[22]).toBeNull()
    expect(out.p90[0]).toBeNull()
    expect(out.p50[23]).toBe(5)
  })

  it('skips null and negative values and out-of-range timestamps', () => {
    const jobs = [job({ finishedAt: at(12) }), job({ finishedAt: at(12) }), job({ finishedAt: at(12) }), job({ finishedAt: 0 })]
    const values = [null, -1, 0, 99]
    let k = 0
    const out = percentiles(jobs, b, finished, () => values[k++])
    expect(out.p50[23]).toBe(0)
    expect(out.p90[23]).toBe(0)
    expect(out.p50.filter((v) => v != null)).toHaveLength(1)
  })
})

describe('utilisation', () => {
  const b = buckets('24h', now)

  it('is the busy share of slot time per bucket', () => {
    const out = utilisation([job({ startedAt: at(10), finishedAt: at(10, 30) })], b, 1, now)
    expect(out).toHaveLength(24)
    expect(out[21]).toBeCloseTo(50)
    expect(out[20]).toBe(0)
    expect(out[22]).toBe(0)
  })

  it('scales with the number of slots', () => {
    const out = utilisation([job({ startedAt: at(10), finishedAt: at(10, 30) })], b, 2, now)
    expect(out[21]).toBeCloseTo(25)
  })

  it('splits a job spanning two buckets', () => {
    const out = utilisation([job({ startedAt: at(10, 30), finishedAt: at(11, 30) })], b, 1, now)
    expect(out[21]).toBeCloseTo(50)
    expect(out[22]).toBeCloseTo(50)
  })

  it('counts a running job until now and clamps the current bucket to now', () => {
    const out = utilisation([job({ startedAt: at(12), finishedAt: 0 })], b, 1, now)
    expect(out[23]).toBeCloseTo(100)
    const half = utilisation([job({ startedAt: at(12, 15), finishedAt: 0 })], b, 1, now)
    expect(half[23]).toBeCloseTo(50)
  })

  it('ignores jobs that never started', () => {
    const out = utilisation([job({ startedAt: 0, finishedAt: at(11) })], b, 1, now)
    expect(out.every((v) => v === 0)).toBe(true)
  })

  it('caps at 100', () => {
    const jobs = [job({ startedAt: at(10), finishedAt: at(11) }), job({ startedAt: at(10), finishedAt: at(11) })]
    expect(utilisation(jobs, b, 1, now)[21]).toBe(100)
  })

  it('is null with no slots', () => {
    const out = utilisation([job({ startedAt: at(10), finishedAt: at(11) })], b, 0, now)
    expect(out.every((v) => v === null)).toBe(true)
  })

  it('is null for a bucket that has not started yet', () => {
    const out = utilisation([], b, 1, at(12))
    expect(out[23]).toBeNull()
    expect(out[22]).toBe(0)
  })
})
