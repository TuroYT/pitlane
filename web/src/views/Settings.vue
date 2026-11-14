<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, post, type Settings } from '../api'
import Updates from '../components/Updates.vue'

const s = ref<Settings | null>(null)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const metricsToken = ref('')

onMounted(async () => {
  s.value = await api<Settings>('/api/settings')
})

async function save(clearMetricsToken = false) {
  if (!s.value) return
  saving.value = true
  saved.value = false
  error.value = ''
  const body: Settings = { ...s.value }
  delete body.metricsToken
  if (clearMetricsToken) body.metricsToken = ''
  else if (metricsToken.value.trim()) body.metricsToken = metricsToken.value.trim()
  try {
    s.value = await post<Settings>('/api/settings', body, 'PUT')
    metricsToken.value = ''
    saved.value = true
    setTimeout(() => (saved.value = false), 2500)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <form v-if="s" class="stack-lg settings" @submit.prevent="save()">
    <div class="row-between">
      <h1>Settings</h1>
      <div class="filters">
        <span v-if="saved" class="ok-text">Saved: applied live</span>
        <button class="primary" :disabled="saving">{{ saving ? 'Saving…' : 'Save' }}</button>
      </div>
    </div>
    <p v-if="error" class="alert error">{{ error }}</p>

    <section class="card stack">
      <h2>Capacity</h2>
      <label>
        Parallel jobs (slots)
        <input v-model.number="s.slots" type="number" min="1" max="64" />
        <small>Each slot is a warm Docker daemon that runs one job at a time. A good start is half your CPU cores.</small>
      </label>
      <div class="grid2">
        <label>
          Memory limit per job (MB)
          <input v-model.number="s.memoryMb" type="number" min="0" />
          <small>0 = no limit (fastest).</small>
        </label>
        <label>
          CPU weight per job
          <input v-model.number="s.cpuShares" type="number" min="0" />
          <small>Relative share when the host is saturated (Docker cpu-shares). 0 = default. A lone job always gets every core.</small>
        </label>
      </div>
    </section>

    <section class="card stack">
      <h2>Speed &amp; caches</h2>
      <label>
        Cache sharing
        <select v-model="s.cacheScope">
          <option value="owner">Per GitHub owner: repositories of different users/organizations never share a cache</option>
          <option value="shared">Shared: every repository shares caches (fastest, single-owner setups)</option>
        </select>
      </label>
      <label class="inline">
        <input v-model="s.mirror" type="checkbox" /> Local Docker Hub mirror (faster pulls, no rate limits)
      </label>
      <label class="inline">
        <input v-model="s.qemu" type="checkbox" /> Multi-architecture builds (QEMU emulation, applied at next restart)
      </label>
      <label>
        Docker build cache kept per slot (GB)
        <input v-model.number="s.keepBuildGb" type="number" min="1" />
      </label>
      <label>
        Minimum free disk space (GB)
        <input v-model.number="s.minFreeDiskGb" type="number" min="0" />
        <small>Checked after each job. Below it, Pitlane prunes the build caches, then the images of idle slots, then the largest unused per-owner cache volumes. Busy slots are never touched. 0 = never prune.</small>
      </label>
    </section>

    <section class="card stack">
      <h2>Images</h2>
      <label>
        Runner image
        <input v-model="s.runnerImage" />
        <small>Must be based on <code>ghcr.io/actions/actions-runner</code>. Changes apply to the next jobs.</small>
      </label>
      <label>
        Docker-in-Docker image
        <input v-model="s.dindImage" />
        <small>Applies to new slots.</small>
      </label>
    </section>

    <section class="card stack">
      <h2>Access</h2>
      <label>
        Allowed GitHub users
        <input v-model="s.allowedUsers" placeholder="alice, bob" />
        <small>Comma-separated GitHub logins that can sign in to this Pitlane.</small>
      </label>
      <label>
        Public URL
        <input v-model="s.baseUrl" />
        <small>Used for GitHub sign-in redirects. Must match the GitHub App callback URL.</small>
      </label>
      <label>
        Keep job history and logs (days)
        <input v-model.number="s.historyDays" type="number" min="1" />
      </label>
      <label>
        Metrics token
        <input v-model="metricsToken" type="password" autocomplete="new-password"
          :placeholder="s.metricsTokenSet ? 'set (type a new one to replace it)' : 'not set'" />
        <small>
          When set, <code>/metrics</code> requires <code>Authorization: Bearer &lt;token&gt;</code> (at least 16 characters).
          When not set, only signed-in users and clients on this machine can read it.
          <button v-if="s.metricsTokenSet" type="button" class="ghost small" @click="save(true)">Remove token</button>
        </small>
      </label>
    </section>
  </form>
  <Updates v-if="s" class="settings" />
</template>
