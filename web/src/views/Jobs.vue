<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, apiPage, live, type Job } from '../api'
import JobTable from '../components/JobTable.vue'
import CompareTable from '../components/CompareTable.vue'

const PAGE = 50

const jobs = ref<Job[]>([])
const total = ref(0)
const page = ref(1)
const repos = ref<string[]>([])
const repo = ref('')
const result = ref('')
const branch = ref('')
const ref_ = ref('') // branch filter actually sent, debounced
const loading = ref(false)
const error = ref('')
let last = 0
let seq = 0

const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE)))
const from = computed(() => (total.value ? (page.value - 1) * PAGE + 1 : 0))
const to = computed(() => Math.min(page.value * PAGE, total.value))
const filtering = computed(() => !!(repo.value || result.value || ref_.value))

async function load() {
  last = Date.now()
  const q = new URLSearchParams({ limit: String(PAGE), offset: String((page.value - 1) * PAGE) })
  if (repo.value) q.set('repo', repo.value)
  if (result.value) q.set('result', result.value)
  if (ref_.value) q.set('ref', ref_.value)
  const mine = ++seq // ignore answers that arrive after a newer request
  loading.value = true
  try {
    const r = await apiPage<Job>(`/api/jobs?${q}`)
    if (mine !== seq) return
    jobs.value = r.items
    total.value = r.total
    error.value = ''
    // The page emptied (jobs pruned, filter changed): go to the last one.
    if (!r.items.length && page.value > 1 && r.total > 0) {
      page.value = Math.ceil(r.total / PAGE)
    }
  } catch (e) {
    if (mine === seq) error.value = (e as Error).message
  } finally {
    if (mine === seq) loading.value = false
  }
}

async function loadRepos() {
  try {
    repos.value = await api<string[]>('/api/jobs/repos')
  } catch {
    /* the filter just lists fewer repositories */
  }
}

onMounted(() => {
  load()
  loadRepos()
})

// Live refresh, at most every 5 s, of the page being looked at.
watch(() => live.snap?.time, () => {
  if (Date.now() - last > 5000) load()
})

let debounce: ReturnType<typeof setTimeout> | undefined
watch(branch, (v) => {
  clearTimeout(debounce)
  debounce = setTimeout(() => (ref_.value = v.trim()), 300)
})
onBeforeUnmount(() => clearTimeout(debounce))

// A new filter starts again from the first page.
watch([repo, result, ref_], () => {
  if (page.value !== 1) page.value = 1
  else load()
})
watch(page, load)

function go(p: number) {
  page.value = Math.min(Math.max(1, p), pages.value)
}
</script>

<template>
  <div class="stack">
    <div class="row-between">
      <h1>Jobs</h1>
      <div class="filters">
        <input v-model="branch" type="search" placeholder="Filter by branch or ref…" aria-label="Filter by branch or ref" />
        <select v-model="repo" aria-label="Repository">
          <option value="">All repositories</option>
          <option v-for="r in repos" :key="r" :value="r">{{ r }}</option>
        </select>
        <select v-model="result" aria-label="Result">
          <option value="">All results</option>
          <option value="running">Running / queued</option>
          <option value="succeeded">Succeeded</option>
          <option value="failed">Failed</option>
          <option value="canceled">Canceled</option>
        </select>
      </div>
    </div>
    <p v-if="error" class="alert error">{{ error }}</p>
    <JobTable :jobs="jobs" :empty="filtering ? 'No job matches these filters.' : undefined" />
    <nav v-if="total > PAGE" class="pager" aria-label="Pages">
      <span class="muted small" aria-live="polite">{{ from }}–{{ to }} of {{ total.toLocaleString() }}</span>
      <div class="filters">
        <button type="button" class="small" :disabled="page <= 1 || loading" aria-label="First page" @click="go(1)">«</button>
        <button type="button" class="small" :disabled="page <= 1 || loading" @click="go(page - 1)">Previous</button>
        <span class="small">Page {{ page }} / {{ pages }}</span>
        <button type="button" class="small" :disabled="page >= pages || loading" @click="go(page + 1)">Next</button>
        <button type="button" class="small" :disabled="page >= pages || loading" aria-label="Last page" @click="go(pages)">»</button>
      </div>
    </nav>
    <CompareTable />
  </div>
</template>
