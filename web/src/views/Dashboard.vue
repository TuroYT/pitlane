<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, live, bytes, duration, ago, type Job } from '../api'
import { quantile } from '../charts'
import JobTable from '../components/JobTable.vue'
import JobStatus from '../components/JobStatus.vue'
import JobSource from '../components/JobSource.vue'
import JobLinks from '../components/JobLinks.vue'
import TrendCharts from '../components/TrendCharts.vue'
import CompareChart from '../components/CompareChart.vue'
import MemoryPanel from '../components/MemoryPanel.vue'

// Recent jobs are refreshed with the live state; the longer history behind the
// charts only once a minute.
const HISTORY = 1000
const recent = ref<Job[]>([])
const history = ref<Job[]>([])
let lastRecent = 0
let lastHistory = 0
async function loadRecent() {
  lastRecent = Date.now()
  recent.value = await api<Job[]>('/api/jobs?limit=50')
}
async function loadHistory() {
  lastHistory = Date.now()
  history.value = await api<Job[]>(`/api/jobs?limit=${HISTORY}`)
}
onMounted(() => {
  loadRecent()
  loadHistory()
})
watch(() => live.snap?.time, () => {
  if (Date.now() - lastRecent > 3000) loadRecent()
  if (Date.now() - lastHistory > 60000) loadHistory()
})

const snap = computed(() => live.snap)
const now = computed(() => snap.value?.time ?? Date.now() / 1000)
const enabled = computed(() => (snap.value?.targets ?? []).filter((t) => t.enabled))
const slots = computed(() => (snap.value?.slots ?? []).filter((s) => s.status !== 'retiring'))
const busySlots = computed(() => slots.value.filter((s) => s.status === 'busy').length)
const waiting = computed(() => enabled.value.reduce((n, t) => n + t.waiting, 0))
const problems = computed(() => enabled.value.filter((t) => t.status === 'error' || t.status === 'no-credentials'))

const active = computed(() => recent.value.filter((j) => !j.finishedAt).sort((a, b) => (b.startedAt || 0) - (a.startedAt || 0)))
const byRunner = computed(() => new Map(recent.value.filter((j) => j.runnerName).map((j) => [j.runnerName, j])))

const day = computed(() => {
  const from = now.value - 86400
  const started = history.value.filter((j) => j.startedAt >= from && j.queuedAt)
  const done = history.value.filter((j) => j.finishedAt >= from && j.startedAt)
  return {
    wait: quantile(started.map((j) => j.startedAt - j.queuedAt), 0.5),
    run: quantile(done.map((j) => j.finishedAt - j.startedAt), 0.5),
    count: done.length,
  }
})

function slotJob(runner: string) {
  return runner ? byRunner.value.get(runner) : undefined
}
// What an idle slot keeps between jobs, and the tooltip explaining it.
function slotIdle(owner: string): { label: string; title: string } {
  if (owner === '(unknown)') {
    return { label: 'cache: unknown owner', title: 'Pitlane does not know who used this slot last: it is wiped before its next job.' }
  }
  if (owner === '(strict)') {
    return { label: 'cache: none (strict)', title: 'Last used by a strict job: the slot is rebuilt before its next job, nothing is kept.' }
  }
  if (!owner) {
    return { label: 'cache: empty', title: 'This slot has not run a job yet (or was rebuilt): its Docker cache is empty.' }
  }
  return {
    label: `cache: ${owner}`,
    title: `Docker images, layers and build cache of ${owner}'s last jobs are kept here, so the next ${owner} job starts warm. A job of another owner gets this slot rebuilt (wiped) first.`,
  }
}
const slotLabel: Record<string, string> = {
  starting: 'Starting', ready: 'Ready', busy: 'Busy', cleaning: 'Cleaning', error: 'Error', retiring: 'Retiring',
}
function pct(used: number, total: number) {
  return total > 0 ? Math.min(100, Math.max(0, (used / total) * 100)) : 0
}
function level(p: number) {
  return p >= 90 ? 'error' : p >= 75 ? 'warn' : ''
}
</script>

<template>
  <p v-if="!snap" class="muted" role="status">Connecting to Pitlane…</p>
  <div v-else class="stack-lg">
    <div class="stack">
      <p v-for="t in problems" :key="t.id" class="alert error">
        <strong>{{ t.kind === 'repo' ? `${t.owner}/${t.repo}` : t.owner }}</strong>:
        {{ t.status === 'no-credentials' ? 'no GitHub credentials. Finish the setup or install the App on this account.' : t.error }}
        <RouterLink to="/targets">Repositories</RouterLink>
      </p>
      <p v-if="!enabled.length" class="alert info">
        No repository uses Pitlane yet. <RouterLink to="/targets">Enable a repository</RouterLink>, then use
        <code>runs-on: pitlane</code> in your workflows.
      </p>

      <section class="counters" aria-label="Summary">
        <div class="counter">
          <div class="counter-label">Running jobs</div>
          <div class="counter-value">{{ busySlots }} <span class="of">of {{ slots.length }} slots</span></div>
        </div>
        <div class="counter">
          <div class="counter-label">Waiting for a slot</div>
          <div class="counter-value" :class="{ warn: waiting > 0 }">{{ waiting }}</div>
        </div>
        <div class="counter">
          <div class="counter-label" title="Median time between a job being queued and a runner picking it up, last 24 hours">Median wait, 24 h</div>
          <div class="counter-value">{{ day.wait != null ? duration(day.wait) : '—' }}</div>
        </div>
        <div class="counter">
          <div class="counter-label">Median job duration, 24 h</div>
          <div class="counter-value">{{ day.run != null ? duration(day.run) : '—' }} <span v-if="day.count" class="of">{{ day.count }} {{ day.count === 1 ? 'job' : 'jobs' }}</span></div>
        </div>
      </section>
    </div>

    <section>
      <div class="section-head">
        <h2>In progress</h2>
        <RouterLink to="/jobs" class="small">All jobs</RouterLink>
      </div>
      <div class="box running">
        <p v-if="!active.length" class="box-row muted">No job is running. Idle slots keep their caches warm for the next one.</p>
        <div v-for="j in active" :key="j.id" class="running-row">
          <JobStatus :job="j" />
          <div style="min-width: 0">
            <RouterLink v-if="j.runnerName" class="job-title" :to="`/logs/${j.runnerName}`">{{ j.jobName || 'Unnamed job' }}</RouterLink>
            <span v-else class="job-title">{{ j.jobName || 'Unnamed job' }}</span>
            <JobSource :job="j" />
          </div>
          <div class="when">
            <div v-if="j.startedAt">{{ duration(now - j.startedAt) }}<span v-if="j.slot >= 0" class="mono"> · slot #{{ j.slot }}</span></div>
            <div v-else>queued {{ ago(j.queuedAt) }}</div>
            <JobLinks :job="j" logs />
          </div>
        </div>
      </div>
    </section>

    <section>
      <div class="section-head">
        <div>
          <h2>Slots</h2>
          <p class="muted small">Each slot runs one job at a time and keeps its Docker cache warm between jobs.</p>
        </div>
      </div>
      <div class="slots" role="list" aria-label="Slots">
        <div v-for="s in snap.slots" :key="s.index" class="slot" :class="s.status" role="listitem">
          <div class="slot-head">
            <span class="dot" :class="{ ready: 'ok', busy: 'busy', error: 'error' }[s.status as string] ?? ''" aria-hidden="true" />
            <span class="slot-name">#{{ s.index }}</span>
            <span class="slot-state">{{ slotLabel[s.status] ?? s.status }}</span>
          </div>
          <div v-if="s.status === 'busy'" class="slot-body">
            <template v-if="slotJob(s.runner)">
              <RouterLink class="ellipsis" style="display: block" :to="`/logs/${s.runner}`" :title="slotJob(s.runner)!.jobName">
                {{ slotJob(s.runner)!.jobName }}
              </RouterLink>
              <div class="muted ellipsis">{{ slotJob(s.runner)!.repo }} · {{ duration(now - s.since) }}</div>
            </template>
            <template v-else>
              <div class="ellipsis">{{ s.owner }}</div>
              <div class="muted">{{ duration(now - s.since) }}
                <RouterLink v-if="s.runner" :to="`/logs/${s.runner}`">Logs</RouterLink></div>
            </template>
          </div>
          <div v-else-if="s.status === 'error'" class="slot-body error-text">{{ s.error }}</div>
          <div v-else class="slot-body muted" :title="slotIdle(s.owner).title">{{ slotIdle(s.owner).label }}</div>
        </div>
      </div>
    </section>

    <TrendCharts :jobs="history" :slots="slots.length" :limit="HISTORY" :now="now" />

    <MemoryPanel />

    <CompareChart />

    <section>
      <div class="section-head">
        <h2>Recent jobs</h2>
        <RouterLink to="/jobs" class="small">All jobs</RouterLink>
      </div>
      <JobTable :jobs="recent.slice(0, 10)" compact />
    </section>

    <section>
      <div class="section-head">
        <h2>Host</h2>
        <span class="muted small">Updated {{ ago(snap.time) }}</span>
      </div>
      <div class="box box-body host" aria-label="Host resources">
        <div>
          <div class="meter-head"><strong>CPU</strong><span class="muted num">{{ snap.host.cpuPercent.toFixed(0) }}% of {{ snap.host.cpus }} CPUs</span></div>
          <div class="bar" aria-hidden="true"><div :class="level(snap.host.cpuPercent)" :style="{ width: pct(snap.host.cpuPercent, 100) + '%' }" /></div>
        </div>
        <div>
          <div class="meter-head"><strong>Memory</strong><span class="muted num">{{ bytes(snap.host.memTotal - snap.host.memAvailable) }} of {{ bytes(snap.host.memTotal) }}</span></div>
          <div class="bar" aria-hidden="true"><div :class="level(pct(snap.host.memTotal - snap.host.memAvailable, snap.host.memTotal))"
            :style="{ width: pct(snap.host.memTotal - snap.host.memAvailable, snap.host.memTotal) + '%' }" /></div>
        </div>
        <div>
          <div class="meter-head"><strong>Docker disk</strong><span class="muted num">{{ bytes(snap.host.diskFree) }} free of {{ bytes(snap.host.diskTotal) }}</span></div>
          <div class="bar" aria-hidden="true"><div :class="level(pct(snap.host.diskTotal - snap.host.diskFree, snap.host.diskTotal))"
            :style="{ width: pct(snap.host.diskTotal - snap.host.diskFree, snap.host.diskTotal) + '%' }" /></div>
        </div>
      </div>
    </section>
  </div>
</template>
