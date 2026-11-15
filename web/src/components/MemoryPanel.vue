<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, bytes, live, type MemorySizing } from '../api'

// Peak memory per job over the last days, and how many slots it fits:
// production showed memory, not CPU, limits parallel jobs.
const m = ref<MemorySizing | null>(null)
const error = ref('')
let last = 0

async function load() {
  last = Date.now()
  try {
    m.value = await api<MemorySizing>('/api/jobs/memory')
    error.value = ''
  } catch (e) {
    error.value = (e as Error).message
  }
}
onMounted(load)
watch(() => live.snap?.time, () => {
  if (Date.now() - last > 60000) load()
})

const fmt = (v: number | null) => (v != null ? bytes(v) : '—')
const verdict = computed(() => {
  const s = m.value
  if (!s || s.suggestedSlots == null) return ''
  if (s.slots > s.suggestedSlots) return 'over'
  if (s.slots < s.suggestedSlots) return 'under'
  return 'ok'
})
</script>

<template>
  <section>
    <div class="section-head">
      <div>
        <h2>Memory per job</h2>
        <p class="muted small">Peak memory of each job (runner plus its slot) over the last {{ m?.windowDays ?? 7 }} days.</p>
      </div>
      <span v-if="m" class="muted small">{{ m.count }} measured {{ m.count === 1 ? 'job' : 'jobs' }}</span>
    </div>
    <p v-if="error" class="alert error">{{ error }}</p>
    <div v-else-if="m" class="box box-body memory">
      <div>
        <div class="counter-label">p50</div>
        <div class="counter-value">{{ fmt(m.p50) }}</div>
      </div>
      <div>
        <div class="counter-label">p90</div>
        <div class="counter-value">{{ fmt(m.p90) }}</div>
      </div>
      <div>
        <div class="counter-label">Max</div>
        <div class="counter-value">{{ fmt(m.max) }}</div>
      </div>
      <div class="sizing small">
        <template v-if="m.suggestedSlots != null">
          <div>
            <strong>Suggested slots: {{ m.suggestedSlots }}</strong>
            <span class="muted"> · configured: {{ m.slots }}</span>
          </div>
          <div class="muted" :title="`${bytes(m.memTotal)} of memory, minus ${bytes(m.memTotal - m.usable)} kept for the system, Docker and Pitlane`">
            {{ bytes(m.usable) }} usable ÷ {{ fmt(m.p90) }} (p90)
          </div>
          <div class="muted">
            Memory limit: {{ m.memoryLimitMb > 0 ? `${m.memoryLimitMb} MB per runner and per slot` : 'none' }}
          </div>
          <div v-if="verdict === 'over'" class="warn-text" role="note">More slots than fit in memory at p90: parallel jobs may swap or be OOM-killed.</div>
        </template>
        <div v-else class="muted">
          No measured job yet. Pitlane reads each job's usage from cgroup v2; it needs the host's /sys/fs/cgroup (not available on Docker Desktop or rootless Docker).
        </div>
      </div>
    </div>
  </section>
</template>
