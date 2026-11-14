<script setup lang="ts">
import { computed, ref } from 'vue'
import type { ChartConfiguration } from 'chart.js'
import { api, duration } from '../api'
import { baseOptions, fmtSeconds, theme } from '../charts'
import ChartCanvas from './ChartCanvas.vue'

interface CompareRow {
  repo: string
  workflow: string
  job: string
  hostedMedian: number
  hostedCount: number
  pitlaneMedian: number
  pitlaneCount: number
}

// Loaded on demand: the server reads the Actions history of every enabled
// repository from GitHub (cached for 15 minutes).
const rows = ref<CompareRow[] | null>(null)
const loading = ref(false)
const error = ref('')

async function load(refresh = false) {
  loading.value = true
  error.value = ''
  try {
    rows.value = await api<CompareRow[]>('/api/compare' + (refresh ? '?refresh=1' : ''))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

const MAX = 10
const both = computed(() => (rows.value ?? []).filter((r) => r.hostedCount && r.pitlaneCount && r.pitlaneMedian))
const top = computed(() => [...both.value].sort((a, b) => b.hostedMedian - a.hostedMedian).slice(0, MAX))
const overall = computed(() => {
  const hosted = both.value.reduce((n, r) => n + r.hostedMedian, 0)
  const pit = both.value.reduce((n, r) => n + r.pitlaneMedian, 0)
  return both.value.length ? { hosted, pit, ratio: hosted / pit } : null
})
const ratioText = (x: number) => (x >= 1 ? `${x.toFixed(1)}× faster` : `${(1 / x).toFixed(1)}× slower`)

const config = computed<ChartConfiguration>(() => {
  const t = theme.value
  const o = baseOptions(t, fmtSeconds)
  return {
    type: 'bar',
    data: {
      labels: top.value.map((r) => `${r.job} · ${r.repo.split('/')[1] ?? r.repo}`),
      datasets: [
        { label: 'GitHub-hosted', data: top.value.map((r) => r.hostedMedian), backgroundColor: t.hosted, borderRadius: 2, maxBarThickness: 14 },
        { label: 'Pitlane', data: top.value.map((r) => r.pitlaneMedian), backgroundColor: t.pitlane, borderRadius: 2, maxBarThickness: 14 },
      ],
    },
    options: {
      ...o,
      indexAxis: 'y',
      interaction: { mode: 'index', intersect: false, axis: 'y' },
      plugins: {
        ...o.plugins,
        tooltip: {
          ...o.plugins.tooltip,
          callbacks: { label: (c) => `${c.dataset.label}: ${duration(Number(c.parsed.x))} median` },
        },
      },
      scales: {
        x: { beginAtZero: true, grid: { color: t.grid }, border: { display: false }, ticks: { maxTicksLimit: 6, callback: (v) => fmtSeconds(Number(v)) } },
        y: { grid: { display: false }, border: { color: t.grid }, ticks: { autoSkip: false } },
      },
    },
  }
})
</script>

<template>
  <section class="box chart-box">
    <div class="box-head">
      <div>
        <h3>Pitlane vs GitHub-hosted</h3>
        <p class="muted small">Median duration of the same successful jobs, from the recent Actions history (last 60 runs per repository).</p>
      </div>
      <div class="filters">
        <div v-if="rows" class="legend">
          <span><i :style="{ background: theme.hosted }" />GitHub-hosted</span>
          <span><i :style="{ background: theme.pitlane }" />Pitlane</span>
        </div>
        <button class="small" :disabled="loading" @click="load(!!rows)">{{ loading ? 'Comparing…' : rows ? 'Refresh' : 'Compare' }}</button>
      </div>
    </div>
    <div class="box-body stack">
      <p v-if="error" class="alert error">Could not read the Actions history from GitHub: {{ error }}</p>
      <p v-if="!rows && !loading && !error" class="chart-empty" style="height: 96px">
        Reads your repositories' recent Actions history from GitHub to compare each job on both kinds of runners.
      </p>
      <p v-else-if="loading && !rows" class="chart-empty" style="height: 96px">Reading the Actions history…</p>
      <template v-else-if="rows">
        <p v-if="overall" class="small">
          <strong>{{ ratioText(overall.ratio) }}</strong> on {{ both.length }} job{{ both.length === 1 ? '' : 's' }} that ran on both:
          {{ duration(overall.hosted) }} on GitHub-hosted runners, {{ duration(overall.pit) }} on Pitlane (sum of medians).
          <RouterLink to="/jobs">Per-job details</RouterLink>
        </p>
        <ChartCanvas v-if="top.length" :config="config" :height="48 + top.length * 34"
          label="Median job duration on GitHub-hosted runners and on Pitlane" />
        <p v-else class="chart-empty" style="height: 96px">
          No job has run on both kinds of runners yet. Run a workflow once with <code>runs-on: ubuntu-latest</code> and once with
          <code>runs-on: pitlane</code> to compare.
        </p>
        <p v-if="both.length > top.length" class="muted small">Showing the {{ MAX }} longest jobs on GitHub-hosted runners.</p>
      </template>
    </div>
  </section>
</template>
