<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, duration, type Job } from '../api'
import JobStatus from '../components/JobStatus.vue'
import JobSource from '../components/JobSource.vue'
import JobLinks from '../components/JobLinks.vue'

const route = useRoute()
const name = String(route.params.name)
const tab = ref<'job' | 'runner'>('job')
const raw = ref('')
const error = ref('')
const streaming = ref(false)
const follow = ref(true)
const timestamps = ref(false)
const pre = ref<HTMLPreElement>()
let ctrl: AbortController | null = null

// GitHub log lines: "2026-11-14T16:50:33.2074587Z ##[group]Run actions/checkout@v4"
const tsRe = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z ?/
// eslint-disable-next-line no-control-regex
const ansiRe = /\x1b\[[0-9;]*[A-Za-z]/g

const text = computed(() => {
  const lines = raw.value.replace(/^﻿/, '').replace(ansiRe, '').split('\n')
  return lines
    .map((l) => {
      const ts = l.match(tsRe)?.[0] ?? ''
      let body = l.slice(ts.length)
      if (body.startsWith('##[group]')) body = '▸ ' + body.slice(9)
      else if (body.startsWith('##[endgroup]')) return null
      else if (body.startsWith('##[error]')) body = '✖ ' + body.slice(9)
      else if (body.startsWith('##[warning]')) body = '⚠ ' + body.slice(11)
      return (timestamps.value && ts ? ts.slice(11, 19) + '  ' : '') + body
    })
    .filter((l) => l !== null)
    .join('\n')
})

async function load() {
  ctrl?.abort()
  ctrl = new AbortController()
  raw.value = ''
  error.value = ''
  streaming.value = true
  const path = tab.value === 'job' ? 'job-log' : 'logs?follow=1'
  try {
    const res = await fetch(`/api/runners/${encodeURIComponent(name)}/${path}`, { signal: ctrl.signal })
    if (!res.ok) {
      let msg = res.statusText
      try {
        msg = (await res.json()).error ?? msg
      } catch {
        /* not JSON */
      }
      error.value = msg
      return
    }
    const reader = res.body!.getReader()
    const dec = new TextDecoder()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      raw.value += dec.decode(value, { stream: true })
      if (follow.value) {
        await nextTick()
        pre.value?.scrollTo({ top: pre.value.scrollHeight })
      }
    }
  } catch (e) {
    if ((e as Error).name !== 'AbortError') error.value = String(e)
  } finally {
    streaming.value = false
  }
}
watch(tab, load, { immediate: true })

// The job this runner picked, for its origin and links to GitHub.
const job = ref<Job | null>(null)
async function loadJob() {
  try {
    const jobs = await api<Job[]>('/api/jobs?limit=1000')
    job.value = jobs.find((j) => j.runnerName === name) ?? null
  } catch {
    job.value = null
  }
}
loadJob()
watch(streaming, (now, before) => {
  if (before && !now) loadJob()
})
const elapsed = computed(() => {
  const j = job.value
  if (!j?.startedAt) return ''
  return duration((j.finishedAt || Date.now() / 1000) - j.startedAt)
})
onBeforeUnmount(() => ctrl?.abort())
</script>

<template>
  <div class="stack">
    <header class="job-header">
      <RouterLink to="/jobs" class="small">Jobs</RouterLink>
      <h1>
        <JobStatus v-if="job" :job="job" />
        {{ job?.jobName || 'Runner' }}
      </h1>
      <JobSource v-if="job" :job="job" />
      <div class="filters small muted">
        <span>Runner <code>{{ name }}</code></span>
        <span v-if="job && job.slot >= 0">· slot #{{ job.slot }}</span>
        <span v-if="elapsed">· {{ job?.finishedAt ? 'ran' : 'running for' }} {{ elapsed }}</span>
        <JobLinks v-if="job" :job="job" />
      </div>
    </header>
    <div class="row-between">
      <div class="tabs" role="tablist">
        <button role="tab" :aria-selected="tab === 'job'" :class="{ active: tab === 'job' }" @click="tab = 'job'">Job output</button>
        <button role="tab" :aria-selected="tab === 'runner'" :class="{ active: tab === 'runner' }" @click="tab = 'runner'">Runner</button>
      </div>
      <div class="filters">
        <span class="badge" :class="streaming ? 'busy' : 'muted'">{{ streaming ? 'live' : 'finished' }}</span>
        <label class="inline"><input type="checkbox" v-model="timestamps" /> Timestamps</label>
        <label class="inline"><input type="checkbox" v-model="follow" /> Follow</label>
      </div>
    </div>
    <p class="muted small">
      {{ tab === 'job'
        ? 'Output of every step: live while the job runs, fetched from GitHub once it is done.'
        : 'Runner process: connection to GitHub, job start and result.' }}
    </p>
    <p v-if="error" class="alert error">{{ error }}</p>
    <pre ref="pre" class="logs">{{ text || (streaming ? 'Waiting for output…' : 'No output.') }}</pre>
  </div>
</template>
