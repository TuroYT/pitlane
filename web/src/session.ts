import { ref } from 'vue'
import type { Status } from './api'

/** Server status, loaded by App.vue on boot. */
export const status = ref<Status | null>(null)
