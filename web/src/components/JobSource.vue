<script setup lang="ts">
import { computed } from 'vue'
import type { Job } from '../api'
import { jobSource } from '../source'
import Icon from './Icon.vue'

// Where a job comes from: repository, workflow file, branch / pull request /
// tag and the event that triggered it, each linked to GitHub.
const props = defineProps<{ job: Job; hideRepo?: boolean }>()
const s = computed(() => jobSource(props.job))
const refIcon = { branch: 'branch', pr: 'pr', tag: 'tag', ref: 'branch' } as const
</script>

<template>
  <div class="source">
    <a v-if="!hideRepo" :href="s.repoUrl" target="_blank" rel="noopener" :title="`Repository ${s.repo}`">
      <Icon name="repo" :size="14" /><span class="txt">{{ s.repo }}</span>
    </a>
    <a v-if="s.workflow" :href="s.workflowUrl" target="_blank" rel="noopener"
      :title="s.calledWorkflow ? `Reusable workflow ${s.calledWorkflow}` : `Workflow ${s.workflow}`">
      <Icon name="workflow" :size="14" /><span class="txt">{{ s.workflow }}</span>
    </a>
    <a v-if="s.ref" class="ref" :href="s.ref.url" target="_blank" rel="noopener"
      :title="s.ref.kind === 'pr' ? `Pull request #${s.ref.name}` : s.ref.kind === 'tag' ? `Tag ${s.ref.name}` : `Branch ${s.ref.name}`">
      <Icon :name="refIcon[s.ref.kind]" :size="12" /><span class="txt">{{ s.ref.label }}</span>
    </a>
    <span v-if="s.event" :title="`Triggered by ${s.event}`"><Icon :name="s.eventIcon" :size="14" />{{ s.eventLabel }}</span>
  </div>
</template>
