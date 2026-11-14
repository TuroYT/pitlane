<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { Chart, type ChartConfiguration } from 'chart.js'

// Renders a Chart.js configuration; rebuilt whenever the configuration changes
// (new data, or a light/dark switch that changes the colors).
const props = defineProps<{ config: ChartConfiguration; label: string; height?: number }>()
const canvas = ref<HTMLCanvasElement>()
const chart = shallowRef<Chart>()
let type = ''

function draw() {
  if (!canvas.value) return
  const c = chart.value
  if (c && type === props.config.type) {
    c.data = props.config.data
    c.options = props.config.options ?? {}
    c.update()
    return
  }
  c?.destroy()
  chart.value = new Chart(canvas.value, props.config)
  type = props.config.type
}

onMounted(draw)
watch(() => props.config, draw)
onBeforeUnmount(() => chart.value?.destroy())
</script>

<template>
  <div class="chart-area" :style="height ? { height: height + 'px' } : undefined">
    <canvas ref="canvas" role="img" :aria-label="label" />
  </div>
</template>
