<script setup lang="ts">
import { computed } from 'vue'
import type { Job } from '../api'
import { jobSource } from '../source'
import Icon from './Icon.vue'

const props = defineProps<{ job: Job; logs?: boolean }>()
const s = computed(() => jobSource(props.job))
</script>

<template>
  <span class="links">
    <RouterLink v-if="logs && job.runnerName" :to="`/logs/${job.runnerName}`" title="Job output and runner logs">
      <Icon name="logs" :size="14" />Logs
    </RouterLink>
    <a v-if="s.runUrl" :href="s.runUrl" target="_blank" rel="noopener" title="Open the workflow run on GitHub">
      Run<Icon name="external" :size="12" />
    </a>
    <a v-if="s.jobUrl" :href="s.jobUrl" target="_blank" rel="noopener" title="Open this job on GitHub">
      Job<Icon name="external" :size="12" />
    </a>
  </span>
</template>
