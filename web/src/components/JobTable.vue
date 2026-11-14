<script setup lang="ts">
import { duration, type Job } from '../api'

defineProps<{ jobs: Job[]; compact?: boolean }>()

function resultClass(j: Job) {
  if (!j.finishedAt) return j.startedAt ? 'busy' : 'queued'
  return { succeeded: 'ok', failed: 'error', canceled: 'muted', cancelled: 'muted' }[j.result.toLowerCase()] ?? 'muted'
}
function resultLabel(j: Job) {
  if (!j.finishedAt) return j.startedAt ? 'running' : 'queued'
  return j.result.toLowerCase() || 'done'
}
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
function runUrl(j: Job) {
  return j.runId ? `https://github.com/${j.owner}/${j.repo}/actions/runs/${j.runId}` : ''
}
</script>

<template>
  <div class="table-wrap">
    <table class="table">
      <thead>
        <tr>
          <th>Status</th>
          <th>Job</th>
          <th>Repository</th>
          <th title="Time between the job being queued and a runner picking it up">Wait</th>
          <th>Duration</th>
          <th v-if="!compact">Slot</th>
          <th v-if="!compact">Queued</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="!jobs.length">
          <td colspan="8" class="muted center">No jobs yet.</td>
        </tr>
        <tr v-for="j in jobs" :key="j.id">
          <td><span class="badge" :class="resultClass(j)">{{ resultLabel(j) }}</span></td>
          <td class="ellipsis" :title="j.workflowRef">
            {{ j.jobName || '—' }} <span class="muted small">{{ j.workflowRef }}</span>
          </td>
          <td class="ellipsis">{{ j.owner }}/{{ j.repo }}</td>
          <td>{{ wait(j) }}</td>
          <td>{{ runTime(j) }}</td>
          <td v-if="!compact">{{ j.slot >= 0 ? '#' + j.slot : '—' }}</td>
          <td v-if="!compact" class="small muted">{{ when(j) }}</td>
          <td class="actions">
            <RouterLink v-if="j.runnerName" :to="`/logs/${j.runnerName}`">logs</RouterLink>
            <a v-if="runUrl(j)" :href="runUrl(j)" target="_blank" rel="noopener">run ↗</a>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
