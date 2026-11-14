<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, live, type Job } from '../api'
import { jobSource } from '../source'
import JobTable from '../components/JobTable.vue'
import CompareTable from '../components/CompareTable.vue'

const jobs = ref<Job[]>([])
const repo = ref('')
const result = ref('')
let last = 0

async function load() {
  last = Date.now()
  jobs.value = await api<Job[]>('/api/jobs?limit=500')
}
onMounted(load)
watch(() => live.snap?.time, () => {
  if (Date.now() - last > 5000) load()
})

const branch = ref('')
const repos = computed(() => [...new Set(jobs.value.map((j) => `${j.owner}/${j.repo}`))].sort())
const filtered = computed(() =>
  jobs.value.filter((j) => {
    if (repo.value && `${j.owner}/${j.repo}` !== repo.value) return false
    if (branch.value && !(jobSource(j).ref?.name ?? '').toLowerCase().includes(branch.value.trim().toLowerCase())) return false
    if (result.value === 'running') return !j.finishedAt
    if (result.value) return j.result.toLowerCase() === result.value
    return true
  }),
)
</script>

<template>
  <div class="stack">
    <div class="row-between">
      <h1>Jobs</h1>
      <div class="filters">
        <input v-model="branch" type="search" placeholder="Filter by branch or ref…" aria-label="Filter by branch or ref" />
        <select v-model="repo">
          <option value="">All repositories</option>
          <option v-for="r in repos" :key="r" :value="r">{{ r }}</option>
        </select>
        <select v-model="result">
          <option value="">All results</option>
          <option value="running">Running / queued</option>
          <option value="succeeded">Succeeded</option>
          <option value="failed">Failed</option>
          <option value="canceled">Canceled</option>
        </select>
      </div>
    </div>
    <JobTable :jobs="filtered" :empty="jobs.length ? 'No job matches these filters.' : undefined" />
    <CompareTable />
  </div>
</template>
