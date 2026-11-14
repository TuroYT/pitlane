<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { Chart, type ChartConfiguration } from 'chart.js'

// Renders a Chart.js configuration. Live refreshes (SSE, the clock) patch the
// existing chart in place and redraw without animation, so the chart does not
// jump; nothing is redrawn when the data did not change. Only the first draw
// (or a change of chart type) animates.
const props = defineProps<{ config: ChartConfiguration; label: string; height?: number }>()
const canvas = ref<HTMLCanvasElement>()
const chart = shallowRef<Chart>()
let type = ''
let lastData = ''
let lastOptions = ''

function draw() {
  if (!canvas.value) return
  const cfg = props.config
  const data = JSON.stringify(cfg.data)
  const options = JSON.stringify(cfg.options ?? {}) // functions are skipped: callbacks never change
  const c = chart.value
  if (c && type === cfg.type) {
    if (data === lastData && options === lastOptions) return
    if (options !== lastOptions) c.options = cfg.options ?? {}
    c.data.labels = cfg.data.labels
    const next = cfg.data.datasets
    if (c.data.datasets.length !== next.length) {
      c.data.datasets = next
    } else {
      // Keep the dataset objects Chart.js already tracks: only their values change.
      next.forEach((d, i) => Object.assign(c.data.datasets[i], d))
    }
    lastData = data
    lastOptions = options
    c.update('none')
    return
  }
  c?.destroy()
  chart.value = new Chart(canvas.value, cfg)
  type = cfg.type
  lastData = data
  lastOptions = options
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
