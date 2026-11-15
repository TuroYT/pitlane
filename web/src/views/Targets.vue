<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, post, live, targetName, type Installation, type TargetState } from '../api'
import { can, status } from '../session'
import Icon from '../components/Icon.vue'

// The server enforces the same: operators may only pause or resume.
const isAdmin = computed(() => can('admin'))
const canToggle = computed(() => can('operator'))

const installs = ref<Installation[]>([])
const installUrl = ref('')
const loading = ref(false)
const error = ref('')
const search = ref('')
const manual = ref({ kind: 'repo' as 'repo' | 'org', name: '' })

async function loadInstalls() {
  if (status.value?.mode !== 'app' || !isAdmin.value) return
  loading.value = true
  error.value = ''
  try {
    const res = await api<{ installations: Installation[]; installUrl: string }>('/api/installations')
    installs.value = res.installations
    installUrl.value = res.installUrl
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(loadInstalls)

const targets = computed(() => live.snap?.targets ?? [])
const existing = computed(() => new Set(targets.value.map((t) => (t.kind === 'repo' ? `repo:${t.owner}/${t.repo}` : `org:${t.owner}`).toLowerCase())))
const has = (key: string) => existing.value.has(key.toLowerCase())

// Strict mode is left to the server: it asks GitHub whether the target is
// public (and turns strict on when it cannot tell).
async function enable(kind: 'repo' | 'org', owner: string, repo: string, installationId: number) {
  error.value = ''
  try {
    await post('/api/targets', { kind, owner, repo, installationId })
  } catch (e) {
    error.value = (e as Error).message
  }
}

async function addManual() {
  const [owner, repo = ''] = manual.value.name.trim().split('/')
  if (!owner || (manual.value.kind === 'repo' && !repo)) {
    error.value = 'Use owner/repo for a repository, or the organization name.'
    return
  }
  await enable(manual.value.kind, owner, repo, 0)
  manual.value.name = ''
}

async function update(t: TargetState, patch: Partial<TargetState>) {
  error.value = ''
  try {
    await post(`/api/targets/${t.id}`, patch, 'PATCH')
  } catch (e) {
    error.value = (e as Error).message
  }
}

async function remove(t: TargetState) {
  if (!confirm(`Stop providing runners to ${targetName(t)}? Its scale set is deleted from GitHub once running jobs finish.`)) return
  error.value = ''
  try {
    await api(`/api/targets/${t.id}`, { method: 'DELETE' })
  } catch (e) {
    error.value = (e as Error).message
  }
}

function forcedDate(t: TargetState) {
  return t.strictForcedAt ? new Date(t.strictForcedAt * 1000).toLocaleString() : ''
}
function forcedDay(t: TargetState) {
  return t.strictForcedAt ? new Date(t.strictForcedAt * 1000).toLocaleDateString([], { month: 'short', day: 'numeric' }) : ''
}

function statusClass(s: string) {
  return { listening: 'ok', starting: 'busy', error: 'error', disabled: 'muted', 'no-credentials': 'error' }[s] ?? 'muted'
}
const statusLabel: Record<string, string> = {
  listening: 'Listening', starting: 'Starting', error: 'Error', disabled: 'Disabled', 'no-credentials': 'No credentials',
}

function filteredRepos(inst: Installation) {
  const q = search.value.toLowerCase()
  return inst.repos.filter((r) => !q || r.fullName.toLowerCase().includes(q))
}

// The label chip copies "runs-on: <label>" and says so for a moment.
const copied = ref('')
let copiedTimer: ReturnType<typeof setTimeout> | undefined
async function copy(text: string) {
  try {
    await navigator.clipboard?.writeText(text)
    copied.value = text
    clearTimeout(copiedTimer)
    copiedTimer = setTimeout(() => (copied.value = ''), 1500)
  } catch {
    /* clipboard unavailable (plain http): the label stays selectable */
  }
}
</script>

<template>
  <div class="stack-lg">
    <div class="page-head">
      <h1>Repositories</h1>
      <p class="muted">Repositories and organizations Pitlane provides runners to. In a workflow, use <code>runs-on: &lt;label&gt;</code>; repository and organization labels both work.</p>
    </div>
    <p v-if="!isAdmin" class="alert info">
      Your role is {{ status?.role }}: {{ canToggle ? 'you can enable or disable targets, nothing else' : 'read only' }}.
    </p>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <section>
      <div class="section-head">
        <div>
          <h2>Enabled targets</h2>
          <p class="muted small">
            <strong>Warm runners</strong> keep a slot reserved for the target, so its jobs start instantly, but that slot cannot
            run other jobs. New repositories get one, strict (public) ones none, so no idle runner waits for a pull request
            from a fork. Set 0 to free the slot.
          </p>
        </div>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>Repository / organization</th>
              <th>Label</th>
              <th>Status</th>
              <th title="Runners kept registered and idle even when no job is queued: jobs start instantly, but each warm runner keeps one slot reserved for this target">Warm runners</th>
              <th title="Maximum parallel jobs for this target">Max parallel</th>
              <th title="A job running longer is stopped and its slot cleaned. 0 = no limit.">Job limit (min)</th>
              <th title="Strict isolation: fresh caches and a rebuilt Docker daemon for every job, so nothing leaks between jobs. Always on for public repositories. Not a sandbox: a malicious job can still take over the host, so require approval for pull requests from forks.">Strict</th>
              <th>Enabled</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!targets.length">
              <td colspan="9" class="empty">No repository uses Pitlane yet. Enable one below.</td>
            </tr>
            <tr v-for="t in targets" :key="t.id">
              <td>
                <strong>{{ targetName(t) }}</strong>
                <div v-if="t.runners.length" class="muted small">
                  {{ t.runners.filter((r) => r.busy).length }} busy · {{ t.runners.filter((r) => !r.busy).length }} idle
                  <span v-if="t.waiting"> · {{ t.waiting }} waiting for a slot</span>
                </div>
              </td>
              <td>
                <button type="button" class="chip" :class="{ copied: copied === `runs-on: ${t.scaleSetName}` }"
                  :title="`Copy runs-on: ${t.scaleSetName}`" @click="copy(`runs-on: ${t.scaleSetName}`)">
                  {{ copied === `runs-on: ${t.scaleSetName}` ? 'Copied' : `runs-on: ${t.scaleSetName}` }}
                </button>
              </td>
              <td>
                <span class="badge" :class="statusClass(t.status)">{{ statusLabel[t.status] ?? t.status }}</span>
                <div v-if="t.error" class="small error-text clamp" :title="t.error">{{ t.error }}</div>
              </td>
              <td>
                <input type="number" class="num" min="0" :max="t.maxRunners" :value="t.minRunners" :disabled="!isAdmin"
                  :aria-label="`Warm runners for ${targetName(t)}`"
                  @change="update(t, { minRunners: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <input type="number" class="num" min="1" max="64" :value="t.maxRunners" :disabled="!isAdmin"
                  :aria-label="`Max parallel jobs for ${targetName(t)}`"
                  @change="update(t, { maxRunners: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <input type="number" class="num" min="0" step="10" :value="t.maxJobMinutes" :disabled="!isAdmin"
                  :aria-label="`Job time limit in minutes for ${targetName(t)}`"
                  @change="update(t, { maxJobMinutes: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <label class="switch" :title="t.strictRequired ? `Required: ${t.strictRequired}` : ''">
                  <input type="checkbox" :checked="t.strict" :disabled="!isAdmin || (!!t.strictRequired && t.strict)"
                    :aria-label="`Strict isolation for ${targetName(t)}`"
                    @change="update(t, { strict: ($event.target as HTMLInputElement).checked })" />
                  <span />
                </label>
                <div v-if="t.strictRequired" class="small muted nowrap">Required: {{ t.strictRequired }}</div>
                <div v-if="t.strictRequired && t.strictForcedAt" class="small warn-text nowrap" :title="`Pitlane switched this target to strict mode on ${forcedDate(t)}`">
                  Turned on {{ forcedDay(t) }}
                </div>
              </td>
              <td>
                <label class="switch">
                  <input type="checkbox" :checked="t.enabled" :disabled="!canToggle" :aria-label="`Enable ${targetName(t)}`" @change="update(t, { enabled: ($event.target as HTMLInputElement).checked })" />
                  <span />
                </label>
              </td>
              <td class="actions"><button v-if="isAdmin" type="button" class="ghost small danger" @click="remove(t)">Remove</button></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section v-if="isAdmin && status?.mode === 'app'">
      <div class="section-head">
        <h2>Add repositories</h2>
        <div class="filters">
          <input v-model="search" type="search" placeholder="Filter repositories…" aria-label="Filter repositories" />
          <button type="button" class="small" :disabled="loading" @click="loadInstalls">{{ loading ? 'Loading…' : 'Refresh' }}</button>
          <a v-if="installUrl" class="button small" :href="installUrl" target="_blank" rel="noopener">
            Install on another account<Icon name="external" :size="12" />
          </a>
        </div>
      </div>
      <p v-if="loading && !installs.length" class="muted" role="status">Loading the App's installations…</p>
      <p v-if="!loading && !installs.length" class="alert info">
        The GitHub App is not installed anywhere yet.
        <a :href="installUrl" target="_blank" rel="noopener">Install it</a>, then click Refresh.
      </p>
      <div v-for="inst in installs" :key="inst.id" class="card account">
        <div class="row-between">
          <h3>{{ inst.account }} <span class="muted small">{{ inst.type === 'Organization' ? 'Organization' : 'Personal account' }}</span></h3>
          <button v-if="inst.type === 'Organization'" type="button" class="small"
            :disabled="has(`org:${inst.account}`)" @click="enable('org', inst.account, '', inst.id)">
            {{ has(`org:${inst.account}`) ? 'Whole organization enabled' : 'Enable for the whole organization' }}
          </button>
        </div>
        <p v-if="inst.error" class="alert error">{{ inst.error }}</p>
        <ul class="repo-list">
          <li v-for="r in filteredRepos(inst)" :key="r.fullName">
            <span class="repo-name">{{ r.name }} <span v-if="r.private" class="muted small">Private</span>
              <span v-else class="badge warn" title="Anyone can open a pull request that runs code on your runners: always strict, and require approval for fork pull requests">Public</span></span>
            <button type="button" class="small" :disabled="has(`repo:${r.fullName}`)" :aria-label="`Enable ${r.fullName}`"
              @click="enable('repo', inst.account, r.name, inst.id)">
              {{ has(`repo:${r.fullName}`) ? 'Enabled' : 'Enable' }}
            </button>
          </li>
          <li v-if="!filteredRepos(inst).length" class="muted small">No repository matches.</li>
        </ul>
      </div>
    </section>

    <section v-else-if="isAdmin" class="card stack">
      <h2>Add a repository or organization</h2>
      <form class="filters" @submit.prevent="addManual">
        <select v-model="manual.kind" aria-label="Kind">
          <option value="repo">Repository</option>
          <option value="org">Organization</option>
        </select>
        <input v-model="manual.name" :placeholder="manual.kind === 'repo' ? 'owner/repo' : 'organization'"
          :aria-label="manual.kind === 'repo' ? 'Repository (owner/repo)' : 'Organization'" required />
        <button class="primary">Add</button>
      </form>
    </section>
  </div>
</template>
