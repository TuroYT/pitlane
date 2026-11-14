<script setup lang="ts">
import { computed } from 'vue'
import type { Job } from '../api'
import { jobState } from '../source'
import Icon from './Icon.vue'

const props = defineProps<{ job: Job; label?: boolean }>()
const meta = {
  queued: ['queued', 'queued', 'Queued'],
  running: ['running', 'busy', 'In progress'],
  succeeded: ['check', 'ok', 'Success'],
  failed: ['x', 'error', 'Failure'],
  canceled: ['stop', 'muted', 'Canceled'],
  done: ['check', 'muted', 'Done'],
} as const
const m = computed(() => meta[jobState(props.job)])
</script>

<template>
  <span class="status" :class="m[1]" :title="m[2]">
    <Icon :name="m[0]" :label="label ? undefined : m[2]" />
    <span v-if="label" class="label">{{ m[2] }}</span>
  </span>
</template>
