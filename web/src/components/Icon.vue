<script setup lang="ts">
// Small, original 16 px stroke icons (1.5 px, round caps), drawn for Pitlane.
import { computed } from 'vue'

const paths: Record<string, string> = {
  dashboard: '<rect x="2.25" y="2.25" width="4.5" height="4.5" rx="1"/><rect x="9.25" y="2.25" width="4.5" height="4.5" rx="1"/><rect x="2.25" y="9.25" width="4.5" height="4.5" rx="1"/><rect x="9.25" y="9.25" width="4.5" height="4.5" rx="1"/>',
  repo: '<path d="M3.25 12.5V3.25a1 1 0 0 1 1-1h8.5v9.5h-8.5a1 1 0 0 0-1 1 1 1 0 0 0 1 1h2"/><path d="M8.25 12.5v2l1.25-.9 1.25.9v-2"/>',
  jobs: '<circle cx="8" cy="8" r="5.75"/><path d="M6.75 5.75 10 8l-3.25 2.25z"/>',
  settings: '<path d="M2.5 4.5h4M9.5 4.5h4M2.5 11.5h7M12.5 11.5h1"/><circle cx="8" cy="4.5" r="1.5"/><circle cx="11" cy="11.5" r="1.5"/>',
  check: '<circle cx="8" cy="8" r="6.25"/><path d="m5.5 8.25 1.75 1.75 3.25-3.5"/>',
  x: '<circle cx="8" cy="8" r="6.25"/><path d="m6 6 4 4m0-4-4 4"/>',
  stop: '<circle cx="8" cy="8" r="6.25"/><path d="M3.75 12.25 12.25 3.75"/>',
  queued: '<circle cx="8" cy="8" r="6.25" stroke-dasharray="2.2 2.4"/>',
  running: '<circle cx="8" cy="8" r="6.25" opacity=".3"/><path d="M8 1.75A6.25 6.25 0 0 1 14.25 8"/>',
  branch: '<circle cx="4.5" cy="3.75" r="1.5"/><circle cx="4.5" cy="12.25" r="1.5"/><circle cx="11.5" cy="5.25" r="1.5"/><path d="M4.5 5.25v5.5M11.5 6.75c0 2.5-3.5 2.25-6.25 3.75"/>',
  pr: '<circle cx="4.5" cy="3.75" r="1.5"/><circle cx="4.5" cy="12.25" r="1.5"/><circle cx="11.5" cy="12.25" r="1.5"/><path d="M4.5 5.25v5.5M11.5 10.75V6a1.75 1.75 0 0 0-1.75-1.75H7.5m1.5-1.5-1.5 1.5 1.5 1.5"/>',
  tag: '<path d="M2.25 2.25h5.5l6 6-5.5 5.5-6-6z"/><circle cx="5.25" cy="5.25" r=".75" fill="currentColor"/>',
  commit: '<circle cx="8" cy="8" r="2.5"/><path d="M1.75 8h3.75M10.5 8h3.75"/>',
  play: '<path d="M4.75 2.75v10.5L13 8z"/>',
  clock: '<circle cx="8" cy="8" r="6.25"/><path d="M8 4.75V8l2.25 1.5"/>',
  workflow: '<rect x="1.75" y="2.25" width="5" height="4" rx="1"/><rect x="9.25" y="9.75" width="5" height="4" rx="1"/><path d="M4.25 6.25v3.5a1.5 1.5 0 0 0 1.5 1.5h3.5"/>',
  external: '<path d="M9.25 2.25h4.5v4.5M13.5 2.5 7.5 8.5M11.75 9.5v3.25a1 1 0 0 1-1 1h-7.5a1 1 0 0 1-1-1v-7.5a1 1 0 0 1 1-1H6.5"/>',
  logs: '<rect x="1.75" y="2.25" width="12.5" height="11.5" rx="1.5"/><path d="m4.5 6 2 2-2 2M8.5 10.5h3"/>',
  event: '<path d="M8.75 1.75 3.5 9h4l-.75 5.25L12 7H8z"/>',
  slot: '<rect x="2.25" y="3.25" width="11.5" height="9.5" rx="1.5"/><path d="M5 6.25h6M5 9.25h3.5"/>',
}

const props = withDefaults(defineProps<{ name: string; size?: number; label?: string }>(), { size: 16, label: undefined })
const body = computed(() => paths[props.name] ?? '')
const spin = computed(() => props.name === 'running')
</script>

<template>
  <svg class="icon" :class="{ spin }" :width="size" :height="size" viewBox="0 0 16 16" fill="none" stroke="currentColor"
    stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" :role="label ? 'img' : undefined"
    :aria-label="label" :aria-hidden="label ? undefined : 'true'" v-html="body" />
</template>
