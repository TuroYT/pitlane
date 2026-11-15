<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, cacheSummary, post, type Settings } from '../api'
import Updates from '../components/Updates.vue'
import { can } from '../session'

// Operators only see the updates (check, pull the runner image).
const isAdmin = can('admin')

const s = ref<Settings | null>(null)
const saving = ref(false)
const saved = ref(false)
const error = ref('')
const metricsToken = ref('')

// "Reserved slots for strict jobs": automatic (-1) or a count.
const strictAuto = computed({
  get: () => (s.value?.strictSlots ?? -1) < 0,
  set: (auto: boolean) => {
    if (s.value) s.value.strictSlots = auto ? -1 : Math.max(0, s.value.strictSlotsActive)
  },
})

const loadError = ref('')
onMounted(async () => {
  if (!isAdmin) return
  try {
    s.value = await api<Settings>('/api/settings')
  } catch (e) {
    loadError.value = (e as Error).message
  }
})

async function save(clearMetricsToken = false) {
  if (!s.value) return
  saving.value = true
  saved.value = false
  error.value = ''
  const body: Settings = { ...s.value }
  delete body.metricsToken
  delete body.actionsCacheStats
  body.strictSlots = strictAuto.value ? -1 : Math.max(0, body.strictSlots)
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
  <p v-if="isAdmin && !s && !loadError" class="muted settings" role="status">Loading settings…</p>
  <p v-if="loadError" class="alert error settings" role="alert">Could not load the settings: {{ loadError }}</p>
  <form v-if="s" class="stack-lg settings" @submit.prevent="save()">
    <div class="page-head">
      <div class="row-between">
        <h1>Settings</h1>
        <div class="filters">
          <span v-if="saved" class="ok-text" role="status">Saved and applied</span>
          <button type="submit" class="primary" :disabled="saving">{{ saving ? 'Saving…' : 'Save changes' }}</button>
        </div>
      </div>
      <p class="muted">Changes apply right away unless a setting says otherwise.</p>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

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
          <small>0 means no limit (fastest).</small>
        </label>
        <label>
          CPU weight per job
          <input v-model.number="s.cpuShares" type="number" min="0" />
          <small>Relative share when the host is saturated (Docker cpu-shares); 0 means the default. A lone job always gets every core.</small>
        </label>
      </div>
      <label class="inline">
        <input v-model="s.memoryAdmission" type="checkbox" /><span>Start a job only when its memory fits</span>
      </label>
      <p class="small muted">
        Each job's memory is predicted from its last runs (90th percentile of the runner's peak plus its slot's memory without
        page cache), then from its repository's jobs, then from every job, else 2 GB. A job waits while the running jobs and
        its prediction exceed the host's memory minus the reserve. A job bigger than that still runs, alone.
      </p>
      <div class="grid2">
        <label>
          Memory reserve (MB)
          <input v-model.number="s.memoryReserveMb" type="number" min="0" :disabled="!s.memoryAdmission" />
          <small>Kept for the system, Docker, Pitlane and the page cache. At most half the host's memory.</small>
        </label>
        <div class="stack">
          <label class="inline">
            <input v-model="strictAuto" type="checkbox" /><span>Reserve a slot for strict jobs when a strict target exists</span>
          </label>
          <label v-if="!strictAuto">
            Reserved slots for strict jobs
            <input v-model.number="s.strictSlots" type="number" min="0" :max="Math.max(0, s.slots - 1)" />
          </label>
          <small class="muted">
            Strict jobs (public repositories) only run in these slots, or in an empty slot, so they never wipe a private
            repository's warm cache; other jobs never use them. Strict jobs may queue behind each other.
            Reserved now: {{ s.strictSlotsActive }}.
          </small>
        </div>
      </div>
    </section>

    <section class="card stack">
      <h2>Speed and caches</h2>
      <label>
        Cache sharing
        <select v-model="s.cacheScope">
          <option value="owner">Per GitHub owner: repositories of different users/organizations never share a cache</option>
          <option value="shared">Shared: every repository shares caches (fastest, single-owner setups)</option>
        </select>
      </label>
      <label class="inline">
        <input v-model="s.mirror" type="checkbox" /><span>Local Docker Hub mirror (faster pulls, no rate limits)</span>
      </label>
      <label class="inline">
        <input v-model="s.actionsCache" type="checkbox" /><span>Serve <code>actions/cache</code> from this host
          (also <code>setup-*</code> actions with <code>cache:</code>)</span>
      </label>
      <label v-if="s.actionsCache">
        Actions cache size (GB)
        <input v-model.number="s.actionsCacheGb" type="number" min="1" />
        <small>
          Entries stay per repository and branch, never shared across owners; the least recently used go first beyond this size.
          Strict jobs keep using GitHub's cache.
          <template v-if="s.actionsCacheStats"><br />{{ cacheSummary(s.actionsCacheStats) }}</template>
          <template v-else><br />The cache listener is not running (see Pitlane's log): jobs use GitHub's cache.</template>
        </small>
      </label>
      <label v-if="s.actionsCache">
        Actions cache share per owner (%)
        <input v-model.number="s.actionsCacheOwnerPct" type="number" min="1" max="100" />
        <small>
          The most one user or organization may store, uploads in progress included. Beyond it, its own least recently
          used entries go first, so one owner cannot grow past its share at the others' expense. 100 turns the share off.
        </small>
      </label>
      <label class="inline">
        <input v-model="s.qemu" type="checkbox" /><span>Multi-architecture builds (QEMU emulation, applied at the next restart)</span>
      </label>
      <label>
        Docker build cache kept per slot (GB)
        <input v-model.number="s.keepBuildGb" type="number" min="1" />
      </label>
      <label>
        Minimum free disk space (GB)
        <input v-model.number="s.minFreeDiskGb" type="number" min="0" />
        <small>Checked after each job. Below it, Pitlane prunes the build caches, then the images of idle slots, then the least recently used actions cache entries, then the largest unused per-owner cache volumes. Busy slots are never touched. 0 means never prune.</small>
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
      <p class="small muted">Who can sign in, and with which role, is set in <RouterLink to="/users">Users</RouterLink>.</p>
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
          :placeholder="s.metricsTokenSet ? 'Set (type a new one to replace it)' : 'Not set'" />
        <small>
          When set, <code>/metrics</code> requires <code>Authorization: Bearer &lt;token&gt;</code> (at least 16 characters).
          When not set, only signed-in users and clients on this machine can read it.
        </small>
      </label>
      <div v-if="s.metricsTokenSet">
        <button type="button" class="small danger" :disabled="saving" @click="save(true)">Remove metrics token</button>
      </div>
    </section>
  </form>
  <div v-if="!isAdmin" class="stack settings">
    <h1>Settings</h1>
    <p class="alert info">Your role is operator: you can check for updates and pull the runner image. Settings are for admins.</p>
  </div>
  <Updates v-if="s || !isAdmin" class="settings" :admin="isAdmin" />
</template>
