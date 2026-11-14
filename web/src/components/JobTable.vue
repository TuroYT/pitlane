<script setup lang="ts">
import { duration, type Job } from '../api'
import JobStatus from './JobStatus.vue'
import JobSource from './JobSource.vue'
import JobLinks from './JobLinks.vue'
import { cpuTime, cpuTimeTitle, peakRam, peakRamTitle, pressure, pressureTitle } from '../usage'

defineProps<{ jobs: Job[]; compact?: boolean; empty?: string }>()

function wait(j: Job) {
  if (!j.queuedAt) return '—'
  const end = j.startedAt || (j.finishedAt ? 0 : Date.now() / 1000)
  return end ? duration(end - j.queuedAt) : '—'
}
function runTime(j: Job) {
  if (!j.startedAt) return '—'
  return duration((j.finishedAt || Date.now() / 1000) - j.startedAt)
}
function when(j: Job) {
  return j.queuedAt ? new Date(j.queuedAt * 1000).toLocaleString() : ''
}
function whenShort(j: Job) {
  if (!j.queuedAt) return '—'
  const d = new Date(j.queuedAt * 1000)
  const today = new Date().toDateString() === d.toDateString()
  return today ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' })
}
</script>

<template>
  <div class="table-wrap">
    <table class="table">
      <thead>
        <tr>
          <th style="width: 28px"><span class="sr-only">Status</span></th>
          <th>Job</th>
          <th class="right" title="Time between the job being queued and a runner picking it up">Wait</th>
          <th class="right">Duration</th>
          <th v-if="!compact" class="right">Slot</th>
          <template v-if="!compact">
            <th class="right" title="Peak memory of the runner plus its slot (services, docker build), page cache included">Peak RAM</th>
            <th class="right" title="CPU time used by the runner and its slot">CPU time</th>
            <th class="right" title="Share of the job's time stalled waiting for CPU, I/O or memory (worse of runner and slot)">Pressure</th>
          </template>
          <th class="right">Queued</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="!jobs.length">
          <td :colspan="compact ? 6 : 10" class="muted center">{{ empty ?? 'No jobs yet. They appear here as soon as GitHub assigns one to Pitlane.' }}</td>
        </tr>
        <tr v-for="j in jobs" :key="j.id">
          <td><JobStatus :job="j" /></td>
          <td style="min-width: 260px">
            <RouterLink v-if="j.runnerName" class="job-title" :to="`/logs/${j.runnerName}`">{{ j.jobName || 'Unnamed job' }}</RouterLink>
            <span v-else class="job-title">{{ j.jobName || 'Unnamed job' }}</span>
            <JobSource :job="j" />
          </td>
          <td class="right">{{ wait(j) }}</td>
          <td class="right">{{ runTime(j) }}</td>
          <td v-if="!compact" class="right mono">{{ j.slot >= 0 ? '#' + j.slot : '—' }}</td>
          <template v-if="!compact">
            <td class="right num" :title="peakRamTitle(j)">{{ peakRam(j) }}</td>
            <td class="right num" :title="cpuTimeTitle(j)">{{ cpuTime(j) }}</td>
            <td class="right small muted num nowrap" :title="pressureTitle(j)">{{ pressure(j) }}</td>
          </template>
          <td class="right small muted" :title="when(j)">{{ whenShort(j) }}</td>
          <td class="actions"><JobLinks :job="j" /></td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
