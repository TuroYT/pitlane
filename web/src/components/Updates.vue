<script setup lang="ts">
// Updates: Pitlane releases (self-update) and the runner image. Self-contained:
// its own API types and calls (GET /api/updates, POST /api/updates/*).
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ago, api, post } from '../api'

interface UpdateSettings {
  autoUpdate: boolean
  window: { start: number; end: number }
  autoPull: boolean
}

interface UpdatesState {
  pitlane: {
    current: string
    latest?: string
    latestUrl?: string
    latestPublished?: number
    updateAvailable: boolean
    autoCandidate?: string
    autoStatus?: string
    capability: { mode: string; canSelfUpdate: boolean; reason?: string; executable?: string }
    manualCommand?: string
    checkedAt?: number
    checkError?: string
    phase?: string
    applyError?: string
    lastUpdate?: string
  }
  runner: {
    image: string
    imageId?: string
    digest?: string
    version?: string
    versionError?: string
    latest?: string
    latestUrl?: string
    status: 'current' | 'outdated' | 'expiring' | 'expired' | 'unknown'
    deadline?: number
    pulledAt?: number
    pulling: boolean
    pullError?: string
    checkError?: string
  }
  settings: UpdateSettings
}

const st = ref<UpdatesState | null>(null)
const prefs = ref<UpdateSettings | null>(null)
const error = ref('')
const busy = ref('')
const savedPrefs = ref(false)
let timer: number | undefined

const p = computed(() => st.value?.pitlane)
const r = computed(() => st.value?.runner)
const working = computed(() => !!p.value?.phase || !!r.value?.pulling)

const runnerBadge = computed(() => {
  switch (r.value?.status) {
    case 'current': return { cls: 'ok', text: 'up to date' }
    case 'outdated': return { cls: 'warn', text: 'update available' }
    case 'expiring': return { cls: 'warn', text: 'GitHub refuses it soon' }
    case 'expired': return { cls: 'error', text: 'GitHub may refuse it' }
    default: return { cls: '', text: 'unknown' }
  }
})

function day(unix?: number) {
  return unix ? new Date(unix * 1000).toLocaleDateString() : '—'
}

function schedule() {
  window.clearTimeout(timer)
  // Fast while something runs (and while Pitlane restarts), slow otherwise.
  timer = window.setTimeout(load, working.value ? 2000 : 60000)
}

async function load() {
  const before = p.value
  try {
    st.value = await api<UpdatesState>('/api/updates')
    if (!prefs.value) prefs.value = structuredClone(st.value.settings)
    if (before?.phase === 'restarting' && st.value.pitlane.current !== before.current) {
      window.location.reload() // new version: reload its UI
      return
    }
  } catch (e) {
    // Expected for a few seconds while Pitlane restarts into the new version.
    if (before?.phase !== 'restarting') error.value = (e as Error).message
  } finally {
    schedule()
  }
}

async function act(name: 'check' | 'apply' | 'pull-runner', confirmText?: string) {
  if (confirmText && !window.confirm(confirmText)) return
  busy.value = name
  error.value = ''
  try {
    st.value = await post<UpdatesState>('/api/updates/' + name, {})
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = ''
    schedule()
  }
}

async function savePrefs() {
  if (!prefs.value) return
  error.value = ''
  try {
    st.value = await post<UpdatesState>('/api/updates/settings', prefs.value, 'PUT')
    prefs.value = structuredClone(st.value.settings)
    savedPrefs.value = true
    setTimeout(() => (savedPrefs.value = false), 2500)
  } catch (e) {
    error.value = (e as Error).message
  }
}

onMounted(load)
onUnmounted(() => window.clearTimeout(timer))
</script>

<template>
  <section v-if="st && p && r" class="card stack updates">
    <div class="row-between">
      <h2>Updates</h2>
      <button type="button" class="small" :disabled="busy === 'check'" @click="act('check')">
        {{ busy === 'check' ? 'Checking…' : 'Check now' }}
      </button>
    </div>
    <p v-if="error" class="alert error">{{ error }}</p>

    <h3>Pitlane</h3>
    <p>
      Running <code>{{ p.current }}</code>.
      <template v-if="p.latest">
        Latest release <a :href="p.latestUrl" target="_blank" rel="noopener">{{ p.latest }}</a>
        <span class="muted small">({{ day(p.latestPublished) }}, release notes)</span>.
      </template>
      <span v-if="p.updateAvailable" class="badge warn">update available</span>
      <span v-else-if="p.latest && p.capability.mode !== 'dev'" class="badge ok">up to date</span>
    </p>
    <p v-if="p.checkError" class="alert warn">Cannot check for Pitlane releases: {{ p.checkError }}</p>
    <p v-if="p.phase" class="alert info">Updating: {{ p.phase }}…</p>
    <p v-if="p.applyError" class="alert error">Update failed, nothing was replaced: {{ p.applyError }}</p>
    <p v-if="p.lastUpdate" class="muted small">Last update: {{ p.lastUpdate }}</p>

    <template v-if="p.capability.canSelfUpdate">
      <div v-if="p.updateAvailable" class="filters">
        <button type="button" class="primary" :disabled="!!p.phase || busy === 'apply'"
          @click="act('apply', `Install Pitlane ${p.latest} and restart? Running jobs keep running.`)">
          Update now to {{ p.latest }}
        </button>
        <small class="muted">Downloads the release binary, checks its SHA-256 against the release's
          <code>checksums.txt</code>, keeps the current one as <code>{{ p.capability.executable }}.prev</code>, then restarts.</small>
      </div>
    </template>
    <div v-else class="stack">
      <p class="muted small">{{ p.capability.reason }}. To update, run:</p>
      <pre class="code">{{ p.manualCommand }}</pre>
    </div>

    <div v-if="prefs" class="stack">
      <label class="inline">
        <input type="checkbox" v-model="prefs.autoUpdate" :disabled="!p.capability.canSelfUpdate" />
        Install updates automatically (patch and minor releases of the same major version, never pre-releases, only when no job runs)
      </label>
      <div v-if="prefs.autoUpdate" class="grid2">
        <label>
          Window start (hour, local time)
          <input type="number" min="0" max="23" v-model.number="prefs.window.start" />
        </label>
        <label>
          Window end (hour)
          <input type="number" min="0" max="23" v-model.number="prefs.window.end" />
          <small>Same as the start = any time.</small>
        </label>
      </div>
      <p v-if="st.settings.autoUpdate && p.autoStatus" class="muted small">
        Automatic update: {{ p.autoCandidate ? p.autoCandidate + ', ' : '' }}{{ p.autoStatus }}.
      </p>
    </div>

    <h3>Runner image</h3>
    <p>
      <code>{{ r.image }}</code> runs GitHub runner <code>{{ r.version || '?' }}</code>.
      <template v-if="r.latest">
        Latest runner <a :href="r.latestUrl" target="_blank" rel="noopener">{{ r.latest }}</a>.
      </template>
      <span :class="['badge', runnerBadge.cls]">{{ runnerBadge.text }}</span>
    </p>
    <p v-if="r.status === 'expired' || r.status === 'expiring'" class="alert warn">
      GitHub stops accepting a runner version about 30 days after a newer one is released
      (deadline {{ day(r.deadline) }}). Pull the latest image; if it is still old, the image itself must be rebuilt.
    </p>
    <p v-if="r.versionError" class="muted small">Runner version: {{ r.versionError }}</p>
    <p v-if="r.checkError" class="alert warn">Cannot check for runner releases: {{ r.checkError }}</p>
    <p v-if="r.pullError" class="alert error">Pull failed: {{ r.pullError }}</p>
    <p class="muted small">
      Digest <code>{{ r.digest || r.imageId || 'unknown' }}</code> · last pulled {{ ago(r.pulledAt || 0) }}
    </p>
    <div class="filters">
      <button type="button" :disabled="r.pulling || busy === 'pull-runner'" @click="act('pull-runner')">
        {{ r.pulling ? 'Pulling…' : 'Pull latest' }}
      </button>
      <small class="muted">New runners use the pulled image; busy runners are never touched.</small>
    </div>
    <label v-if="prefs" class="inline">
      <input type="checkbox" v-model="prefs.autoPull" /> Pull the runner image automatically every day
    </label>
    <div v-if="prefs" class="filters">
      <button type="button" class="small" @click="savePrefs">Save update preferences</button>
      <span v-if="savedPrefs" class="ok-text">Saved</span>
    </div>
  </section>
</template>

<style scoped>
.updates { margin-top: 28px; }
.updates h3 { margin-top: 6px; }
.updates pre.code { white-space: pre-wrap; word-break: break-all; }
</style>
