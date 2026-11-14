<script setup lang="ts">
import { computed, ref } from 'vue'
import type { ChartConfiguration } from 'chart.js'
import type { Job } from '../api'
import {
  baseOptions, buckets, fmtPercent, fmtSeconds, jobsByResult, percentiles, theme, utilisation, type Range,
} from '../charts'
import ChartCanvas from './ChartCanvas.vue'

const props = defineProps<{ jobs: Job[]; slots: number; limit: number; now: number }>()

const ranges: { id: Range; label: string }[] = [
  { id: '24h', label: '24 hours' },
  { id: '7d', label: '7 days' },
  { id: '30d', label: '30 days' },
]
const range = ref<Range>(load())
function load(): Range {
  try {
    const v = localStorage.getItem('pitlane.trends.range')
    if (v === '24h' || v === '7d' || v === '30d') return v
  } catch {
    /* storage unavailable */
  }
  return '24h'
}
function pick(r: Range) {
  range.value = r
  try {
    localStorage.setItem('pitlane.trends.range', r)
  } catch {
    /* storage unavailable */
  }
}

// Recompute buckets once a minute at most (the parent passes the live clock).
const b = computed(() => buckets(range.value, Math.floor(props.now / 60) * 60))
const inRange = computed(() => props.jobs.filter((j) => Math.max(j.queuedAt, j.finishedAt) >= b.value.start))
const truncated = computed(() => {
  if (props.jobs.length < props.limit) return ''
  const oldest = Math.min(...props.jobs.map((j) => j.queuedAt || Infinity))
  return oldest > b.value.start ? new Date(oldest * 1000).toLocaleString() : ''
})

const results = computed(() => jobsByResult(inRange.value, b.value))
const finished = computed(() => results.value.succeeded.concat(results.value.failed, results.value.canceled).reduce((a, n) => a + n, 0))
const waits = computed(() => percentiles(inRange.value, b.value, (j) => j.startedAt, (j) => (j.startedAt && j.queuedAt ? j.startedAt - j.queuedAt : null)))
const durations = computed(() => percentiles(inRange.value, b.value, (j) => j.finishedAt, (j) => (j.finishedAt && j.startedAt ? j.finishedAt - j.startedAt : null)))
const util = computed(() => utilisation(inRange.value, b.value, props.slots, props.now))
const has = (a: (number | null)[]) => a.some((v) => v != null && v > 0)

const jobsChart = computed<ChartConfiguration>(() => {
  const t = theme.value
  const o = baseOptions(t)
  return {
    type: 'bar',
    data: {
      labels: b.value.labels,
      datasets: [
        { label: 'Succeeded', data: results.value.succeeded, backgroundColor: t.ok, stack: 'r', borderRadius: 2, maxBarThickness: 24 },
        { label: 'Failed', data: results.value.failed, backgroundColor: t.error, stack: 'r', borderRadius: 2, maxBarThickness: 24 },
        { label: 'Canceled', data: results.value.canceled, backgroundColor: t.neutral, stack: 'r', borderRadius: 2, maxBarThickness: 24 },
      ],
    },
    options: { ...o, scales: { x: { ...o.scales.x, stacked: true }, y: { ...o.scales.y, stacked: true, ticks: { ...o.scales.y.ticks, precision: 0 } } } },
  }
})

function lines(p: { p50: (number | null)[]; p90: (number | null)[] }): ChartConfiguration {
  const t = theme.value
  // Gaps stay gaps (no job, no value); points show values with no neighbour.
  const common = { spanGaps: false, borderWidth: 2, pointRadius: 2, pointHoverRadius: 4, cubicInterpolationMode: 'monotone' as const }
  return {
    type: 'line',
    data: {
      labels: b.value.labels,
      datasets: [
        { label: 'p50', data: p.p50, borderColor: t.p50, backgroundColor: t.p50, ...common },
        { label: 'p90', data: p.p90, borderColor: t.p90, backgroundColor: t.p90, borderDash: [5, 4], ...common },
      ],
    },
    options: baseOptions(t, fmtSeconds),
  }
}
const waitChart = computed(() => lines(waits.value))
const durationChart = computed(() => lines(durations.value))

const utilChart = computed<ChartConfiguration>(() => {
  const t = theme.value
  const o = baseOptions(t, fmtPercent)
  return {
    type: 'line',
    data: {
      labels: b.value.labels,
      datasets: [{
        label: 'Busy', data: util.value, borderColor: t.pitlane, borderWidth: 2, pointRadius: 0, pointHoverRadius: 4, cubicInterpolationMode: 'monotone',
        fill: 'origin', backgroundColor: t.pitlane + '26', spanGaps: true,
      }],
    },
    options: { ...o, scales: { ...o.scales, y: { ...o.scales.y, max: 100 } } },
  }
})
</script>

<template>
  <section>
    <div class="section-head">
      <div>
        <h2>Trends</h2>
        <p class="muted small">
          From the job history{{ truncated ? `: the latest ${limit} jobs, since ${truncated}` : '' }}.
        </p>
      </div>
      <div class="seg" role="group" aria-label="Time range">
        <button v-for="r in ranges" :key="r.id" :class="{ active: range === r.id }" :aria-pressed="range === r.id" @click="pick(r.id)">
          {{ r.label }}
        </button>
      </div>
    </div>
    <div class="charts">
      <div class="box chart-box">
        <div class="box-head">
          <h3>Jobs</h3>
          <div class="legend">
            <span><i :style="{ background: theme.ok }" />Succeeded</span>
            <span><i :style="{ background: theme.error }" />Failed</span>
            <span><i :style="{ background: theme.neutral }" />Canceled</span>
          </div>
        </div>
        <div class="box-body">
          <ChartCanvas v-if="finished" :config="jobsChart" label="Finished jobs over time, by result" />
          <p v-else class="chart-empty">No job finished in this period.</p>
        </div>
      </div>
      <div class="box chart-box">
        <div class="box-head">
          <h3>Slot utilization</h3>
          <div class="legend"><span>Busy time over {{ slots }} slot{{ slots === 1 ? '' : 's' }}</span></div>
        </div>
        <div class="box-body">
          <ChartCanvas v-if="has(util)" :config="utilChart" label="Share of slot time spent running jobs" />
          <p v-else class="chart-empty">No job ran in this period: every slot stayed idle and warm.</p>
        </div>
      </div>
      <div class="box chart-box">
        <div class="box-head">
          <h3 title="Time between a job being queued on GitHub and a runner picking it up">Queue wait</h3>
          <div class="legend">
            <span><i class="line" :style="{ background: theme.p50 }" />p50</span>
            <span><i class="line" :style="{ background: theme.p90 }" />p90</span>
          </div>
        </div>
        <div class="box-body">
          <ChartCanvas v-if="has(waits.p90)" :config="waitChart" label="Queue wait time, median and 90th percentile" />
          <p v-else class="chart-empty">No job started in this period.</p>
        </div>
      </div>
      <div class="box chart-box">
        <div class="box-head">
          <h3>Job duration</h3>
          <div class="legend">
            <span><i class="line" :style="{ background: theme.p50 }" />p50</span>
            <span><i class="line" :style="{ background: theme.p90 }" />p90</span>
          </div>
        </div>
        <div class="box-body">
          <ChartCanvas v-if="has(durations.p90)" :config="durationChart" label="Job duration, median and 90th percentile" />
          <p v-else class="chart-empty">No job finished in this period.</p>
        </div>
      </div>
    </div>
  </section>
</template>
