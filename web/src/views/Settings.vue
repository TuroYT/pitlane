<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, post, type Settings } from '../api'

const s = ref<Settings | null>(null)
const saving = ref(false)
const saved = ref(false)
const error = ref('')

onMounted(async () => {
  s.value = await api<Settings>('/api/settings')
})

async function save() {
  if (!s.value) return
  saving.value = true
  saved.value = false
  error.value = ''
  try {
    s.value = await post<Settings>('/api/settings', s.value, 'PUT')
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
  <form v-if="s" class="stack-lg settings" @submit.prevent="save">
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
        <input type="number" min="1" max="64" v-model.number="s.slots" />
        <small>Each slot is a warm Docker daemon that runs one job at a time. A good start is half your CPU cores.</small>
      </label>
      <div class="grid2">
        <label>
          Memory limit per job (MB)
          <input type="number" min="0" v-model.number="s.memoryMb" />
          <small>0 = no limit (fastest).</small>
        </label>
        <label>
          CPU weight per job
          <input type="number" min="0" v-model.number="s.cpuShares" />
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
        <input type="checkbox" v-model="s.mirror" /> Local Docker Hub mirror (faster pulls, no rate limits)
      </label>
      <label class="inline">
        <input type="checkbox" v-model="s.qemu" /> Multi-architecture builds (QEMU emulation, applied at next restart)
      </label>
      <label>
        Docker build cache kept per slot (GB)
        <input type="number" min="1" v-model.number="s.keepBuildGb" />
      </label>
      <label>
        Minimum free disk space (GB)
        <input type="number" min="0" v-model.number="s.minFreeDiskGb" />
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
        <input type="number" min="1" v-model.number="s.historyDays" />
      </label>
    </section>
  </form>
</template>
