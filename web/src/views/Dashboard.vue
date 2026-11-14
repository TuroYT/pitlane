<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, live, bytes, duration, ago, type Job } from '../api'
import JobTable from '../components/JobTable.vue'

const jobs = ref<Job[]>([])
let last = 0
async function loadJobs() {
  last = Date.now()
  jobs.value = await api<Job[]>('/api/jobs?limit=12')
}
onMounted(loadJobs)
watch(() => live.snap?.time, () => {
  if (Date.now() - last > 3000) loadJobs()
})

const snap = computed(() => live.snap)
const targetsById = computed(() => new Map((snap.value?.targets ?? []).map((t) => [t.id, t])))
const enabled = computed(() => (snap.value?.targets ?? []).filter((t) => t.enabled))
const busySlots = computed(() => (snap.value?.slots ?? []).filter((s) => s.status === 'busy').length)
const waiting = computed(() => enabled.value.reduce((n, t) => n + t.waiting, 0))
const problems = computed(() => enabled.value.filter((t) => t.status === 'error' || t.status === 'no-credentials'))

const stats = computed(() => {
  const done = jobs.value.filter((j) => j.startedAt && j.queuedAt)
  if (!done.length) return null
  const waits = done.map((j) => j.startedAt - j.queuedAt).sort((a, b) => a - b)
  const runs = jobs.value.filter((j) => j.finishedAt && j.startedAt).map((j) => j.finishedAt - j.startedAt).sort((a, b) => a - b)
  const median = (a: number[]) => (a.length ? a[Math.floor(a.length / 2)] : NaN)
  return { wait: median(waits), run: median(runs) }
})

function slotLabel(targetId: number) {
  const t = targetsById.value.get(targetId)
  if (!t) return ''
  return t.kind === 'repo' ? t.repo : t.owner
}
const now = computed(() => snap.value?.time ?? Date.now() / 1000)
</script>

<template>
  <div v-if="!snap" class="muted">Connecting…</div>
  <div v-else class="stack-lg">
    <section class="kpis">
      <div class="kpi">
        <div class="kpi-label">Running jobs</div>
        <div class="kpi-value">{{ busySlots }} <span class="muted">/ {{ snap.slots.length }}</span></div>
      </div>
      <div class="kpi">
        <div class="kpi-label">Waiting for a slot</div>
        <div class="kpi-value" :class="{ warn: waiting > 0 }">{{ waiting }}</div>
      </div>
      <div class="kpi">
        <div class="kpi-label">Median wait (recent)</div>
        <div class="kpi-value">{{ stats ? duration(stats.wait) : '—' }}</div>
      </div>
      <div class="kpi">
        <div class="kpi-label">Median job duration</div>
        <div class="kpi-value">{{ stats ? duration(stats.run) : '—' }}</div>
      </div>
    </section>

    <p v-for="t in problems" :key="t.id" class="alert error">
      <strong>{{ t.kind === 'repo' ? `${t.owner}/${t.repo}` : t.owner }}</strong>:
      {{ t.status === 'no-credentials' ? 'no GitHub credentials. Finish the setup or install the App on this account.' : t.error }}
    </p>

    <p v-if="!enabled.length" class="alert info">
      No repository uses Pitlane yet. <RouterLink to="/targets">Enable a repository</RouterLink>, then use
      <code>runs-on: pitlane</code> in your workflows.
    </p>

    <section>
      <h2>Slots</h2>
      <p class="muted small">Each slot runs one job at a time and keeps its Docker cache warm between jobs.</p>
      <div class="slots">
        <div v-for="s in snap.slots" :key="s.index" class="slot" :class="s.status">
          <div class="slot-head">
            <span class="slot-name">#{{ s.index }}</span>
            <span class="badge" :class="s.status">{{ s.status }}</span>
          </div>
          <div v-if="s.status === 'busy'" class="slot-body">
            <div class="ellipsis">{{ slotLabel(s.targetId) || s.owner }}</div>
            <div class="muted small">
              {{ duration(now - s.since) }}
              <RouterLink v-if="s.runner" :to="`/logs/${s.runner}`">logs</RouterLink>
            </div>
          </div>
          <div v-else-if="s.status === 'error'" class="slot-body small error-text">{{ s.error }}</div>
          <div v-else class="slot-body muted small">
            {{ s.owner ? `warm for ${s.owner}` : 'empty cache' }}
          </div>
        </div>
      </div>
    </section>

    <section>
      <div class="row-between">
        <h2>Recent jobs</h2>
        <RouterLink to="/jobs" class="small">All jobs →</RouterLink>
      </div>
      <JobTable :jobs="jobs" compact />
    </section>

    <section>
      <h2>Host</h2>
      <div class="host">
        <div>
          <span class="muted">CPU load</span>
          <div class="bar"><div :style="{ width: Math.min(100, (snap.host.load1 / snap.host.cpus) * 100) + '%' }" /></div>
          <span class="small">{{ snap.host.load1.toFixed(2) }} / {{ snap.host.cpus }} CPUs</span>
        </div>
        <div>
          <span class="muted">Memory</span>
          <div class="bar"><div :style="{ width: ((1 - snap.host.memAvailable / snap.host.memTotal) * 100) + '%' }" /></div>
          <span class="small">{{ bytes(snap.host.memTotal - snap.host.memAvailable) }} / {{ bytes(snap.host.memTotal) }}</span>
        </div>
        <div>
          <span class="muted">Docker disk</span>
          <div class="bar"><div :style="{ width: ((1 - snap.host.diskFree / snap.host.diskTotal) * 100) + '%' }" /></div>
          <span class="small">{{ bytes(snap.host.diskFree) }} free of {{ bytes(snap.host.diskTotal) }}</span>
        </div>
      </div>
      <p class="muted small">Updated {{ ago(snap.time) }}</p>
    </section>
  </div>
</template>
