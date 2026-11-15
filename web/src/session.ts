import { ref } from 'vue'
import { hasRole, type Role, type Status } from './api'

/** Server status, loaded by App.vue on boot. */
export const status = ref<Status | null>(null)

/** Whether the signed-in user has at least role min (the server enforces it too). */
export function can(min: Role): boolean {
  return hasRole(status.value?.role, min)
}
