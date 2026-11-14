<script setup lang="ts">
import { computed, ref } from 'vue'
import { api, duration } from '../api'

interface Row {
  repo: string
  workflow: string
  job: string
  hostedMedian: number
  hostedCount: number
  pitlaneMedian: number
  pitlaneCount: number
}

const rows = ref<Row[] | null>(null)
const loading = ref(false)
const error = ref('')
const onlyBoth = ref(true)

async function load(refresh = false) {
  loading.value = true
  error.value = ''
  try {
    rows.value = await api<Row[]>('/api/compare' + (refresh ? '?refresh=1' : ''))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

const shown = computed(() => (rows.value ?? []).filter((r) => !onlyBoth.value || (r.hostedCount && r.pitlaneCount)))
const overall = computed(() => {
  const both = (rows.value ?? []).filter((r) => r.hostedCount && r.pitlaneCount)
  const hosted = both.reduce((n, r) => n + r.hostedMedian, 0)
  const pit = both.reduce((n, r) => n + r.pitlaneMedian, 0)
  return both.length ? { hosted, pit, ratio: hosted / pit } : null
})
function ratio(r: Row) {
  if (!r.hostedCount || !r.pitlaneCount || !r.pitlaneMedian) return ''
  const x = r.hostedMedian / r.pitlaneMedian
  return x >= 1 ? `${x.toFixed(1)}× faster` : `${(1 / x).toFixed(1)}× slower`
}
</script>

<template>
  <section class="stack">
    <div class="row-between">
      <h2>Pitlane vs GitHub-hosted</h2>
      <div class="filters">
        <label v-if="rows" class="inline"><input v-model="onlyBoth" type="checkbox" /> Only jobs run on both</label>
        <button class="small" :disabled="loading" @click="load(!!rows)">{{ loading ? 'Comparing…' : rows ? 'Refresh' : 'Compare' }}</button>
      </div>
    </div>
    <p class="muted small">
      Median duration of each successful job in the recent Actions history of your enabled repositories (last 60 runs).
    </p>
    <p v-if="error" class="alert error">{{ error }}</p>
    <p v-if="overall" class="alert" :class="overall.ratio >= 1 ? 'ok' : 'warn'">
      Jobs run on both: {{ duration(overall.hosted) }} on GitHub-hosted runners vs {{ duration(overall.pit) }} on Pitlane
      ({{ overall.ratio >= 1 ? `${overall.ratio.toFixed(1)}× faster` : `${(1 / overall.ratio).toFixed(1)}× slower` }}).
    </p>
    <div v-if="rows" class="table-wrap">
      <table class="table">
        <thead>
          <tr><th>Repository</th><th>Workflow / job</th><th>GitHub-hosted</th><th>Pitlane</th><th></th></tr>
        </thead>
        <tbody>
          <tr v-if="!shown.length"><td colspan="5" class="muted center">Nothing to compare yet.</td></tr>
          <tr v-for="r in shown" :key="r.repo + r.workflow + r.job">
            <td class="ellipsis">{{ r.repo }}</td>
            <td class="ellipsis">{{ r.job }} <span class="muted small">{{ r.workflow }}</span></td>
            <td>{{ r.hostedCount ? duration(r.hostedMedian) : '—' }} <span class="muted small">{{ r.hostedCount ? `(${r.hostedCount})` : '' }}</span></td>
            <td>{{ r.pitlaneCount ? duration(r.pitlaneMedian) : '—' }} <span class="muted small">{{ r.pitlaneCount ? `(${r.pitlaneCount})` : '' }}</span></td>
            <td :class="ratio(r).includes('faster') ? 'ok-text' : 'error-text'">{{ ratio(r) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
