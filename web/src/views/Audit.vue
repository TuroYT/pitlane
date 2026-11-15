<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { apiPage, type AuditEntry } from '../api'

const PAGE = 50
const entries = ref<AuditEntry[]>([])
const total = ref(0)
const page = ref(1)
const loading = ref(false)
const error = ref('')

const pages = computed(() => Math.max(1, Math.ceil(total.value / PAGE)))
const from = computed(() => (total.value ? (page.value - 1) * PAGE + 1 : 0))
const to = computed(() => Math.min(page.value * PAGE, total.value))

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await apiPage<AuditEntry>(`/api/audit?limit=${PAGE}&offset=${(page.value - 1) * PAGE}`)
    entries.value = r.items
    total.value = r.total
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)
watch(page, load)

function go(p: number) {
  page.value = Math.min(Math.max(1, p), pages.value)
}

function when(unix: number) {
  return new Date(unix * 1000).toLocaleString()
}
</script>

<template>
  <div class="stack-lg">
    <div class="row-between">
      <h1>Audit log</h1>
      <button type="button" class="small" :disabled="loading" @click="load">{{ loading ? 'Loading…' : 'Refresh' }}</button>
    </div>
    <p class="muted small">Changes to targets, settings, users, rules and updates, latest first. Kept as long as the job history.</p>
    <p v-if="error" class="alert error">{{ error }}</p>
    <div class="table-wrap">
      <table class="table">
        <thead>
          <tr><th>When</th><th>Who</th><th>Action</th><th>On</th><th>Details</th></tr>
        </thead>
        <tbody>
          <tr v-if="!entries.length && !loading">
            <td colspan="5" class="muted center">Nothing recorded yet.</td>
          </tr>
          <tr v-for="e in entries" :key="e.id">
            <td class="small nowrap">{{ when(e.at) }}</td>
            <td>@{{ e.actor }}</td>
            <td><code>{{ e.action }}</code></td>
            <td>{{ e.subject }}</td>
            <td class="small detail">{{ e.detail }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <nav v-if="total > PAGE" class="pager" aria-label="Pages">
      <span class="muted small" aria-live="polite">{{ from }}–{{ to }} of {{ total.toLocaleString() }}</span>
      <div class="filters">
        <button type="button" class="small" :disabled="page <= 1 || loading" @click="go(page - 1)">Newer</button>
        <span class="small">Page {{ page }} / {{ pages }}</span>
        <button type="button" class="small" :disabled="page >= pages || loading" @click="go(page + 1)">Older</button>
      </div>
    </nav>
  </div>
</template>

<style scoped>
.nowrap { white-space: nowrap; }
.detail { word-break: break-word; }
</style>
