<script setup lang="ts">
// The Pitlane mark (docs/logo.svg): a P split by two speed slits.
import { useId } from 'vue'

withDefaults(defineProps<{ size?: number }>(), { size: 24 })
const id = `pl-cut-${useId()}`
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 32 32" aria-hidden="true" class="mark">
    <mask :id="id">
      <rect width="32" height="32" fill="#fff" />
      <path d="M4 10h11M4 14h7" stroke="#000" stroke-width="2" />
    </mask>
    <rect width="32" height="32" rx="7" fill="#E5482D" />
    <path fill="#fff" fill-rule="evenodd" :mask="`url(#${id})`" d="M9 6.5h9.5a7 7 0 0 1 0 14H15v5H9zM15 11.5v4h3.5a2 2 0 0 0 0-4z" />
  </svg>
</template>
