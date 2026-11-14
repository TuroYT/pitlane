import { describe, expect, it } from 'vitest'
import type { ContainerUsage, Job } from './api'
import { cpuTime, cpuTimeTitle, peakRam, peakRamTitle, pressure, worstPressure } from './usage'

const none: ContainerUsage = {
  memPeak: null, cpuUsec: null, cpuSomeUsec: null, ioFullUsec: null, memFullUsec: null,
  cpuPressurePct: null, ioPressurePct: null, memPressurePct: null,
}

function job(usage: Job['usage']): Job {
  return {
    id: 1, targetId: 1, jobId: 'j', runId: 1, owner: 'o', repo: 'r', workflowRef: 'ci.yml', jobName: 'build', event: 'push',
    runnerName: 'pitlane-x', slot: 0, queuedAt: 1, startedAt: 2, finishedAt: 3, result: 'succeeded', usage,
  }
}

describe('job usage', () => {
  const j = job({
    wallMs: 100_000,
    memPeak: 3 * 1024 ** 3,
    runner: { ...none, memPeak: 1024 ** 3, cpuUsec: 60e6, cpuPressurePct: 12.5, ioPressurePct: 1, memPressurePct: 0 },
    slot: { ...none, memPeak: 2 * 1024 ** 3, cpuUsec: 30e6, cpuPressurePct: 40, ioPressurePct: 0.5 },
  })

  it('formats peak memory and CPU time', () => {
    expect(peakRam(j)).toBe('3.0 GB')
    expect(peakRamTitle(j)).toContain('runner 1.0 GB + slot 2.0 GB')
    expect(cpuTime(j)).toBe('1m 30s')
    expect(cpuTimeTitle(j)).toContain('runner 1m (0.6 CPUs on average)')
  })

  it('shows the worse pressure of runner and slot', () => {
    expect(worstPressure(j, 'cpuPressurePct')).toBe(40)
    expect(worstPressure(j, 'memPressurePct')).toBe(0)
    expect(pressure(j)).toBe('cpu 40% · io 1.0% · mem 0.0%')
  })

  it('shows dashes when not measured', () => {
    const n = job(null)
    expect(peakRam(n)).toBe('—')
    expect(cpuTime(n)).toBe('—')
    expect(pressure(n)).toBe('—')
    expect(peakRamTitle(n)).toBe('Not measured')
    const partial = job({ wallMs: 1000, memPeak: null, runner: none, slot: none })
    expect(cpuTime(partial)).toBe('—')
    expect(pressure(partial)).toBe('cpu — · io — · mem —')
  })
})
