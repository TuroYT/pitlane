<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'

const route = useRoute()
const name = String(route.params.name)
const text = ref('')
const error = ref('')
const streaming = ref(false)
const follow = ref(true)
const pre = ref<HTMLPreElement>()
let ctrl: AbortController | null = null

async function load() {
  ctrl = new AbortController()
  streaming.value = true
  try {
    const res = await fetch(`/api/runners/${encodeURIComponent(name)}/logs?follow=1`, { signal: ctrl.signal })
    if (!res.ok) {
      error.value = res.status === 404 ? 'No logs for this runner (they may have expired).' : res.statusText
      return
    }
    const reader = res.body!.getReader()
    const dec = new TextDecoder()
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      text.value += dec.decode(value, { stream: true })
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
onMounted(load)
onBeforeUnmount(() => ctrl?.abort())
</script>

<template>
  <div class="stack">
    <div class="row-between">
      <h1>Runner <code>{{ name }}</code></h1>
      <div class="filters">
        <span class="badge" :class="streaming ? 'busy' : 'muted'">{{ streaming ? 'live' : 'finished' }}</span>
        <label class="inline"><input type="checkbox" v-model="follow" /> Follow</label>
      </div>
    </div>
    <p class="muted small">Runner process output. Step-by-step job logs are on the GitHub run page.</p>
    <p v-if="error" class="alert error">{{ error }}</p>
    <pre ref="pre" class="logs">{{ text || (streaming ? 'Waiting for output…' : '') }}</pre>
  </div>
</template>
