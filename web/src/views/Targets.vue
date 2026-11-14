<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, post, live, targetName, type Installation, type TargetState } from '../api'
import { status } from '../session'

const installs = ref<Installation[]>([])
const installUrl = ref('')
const loading = ref(false)
const error = ref('')
const search = ref('')
const manual = ref({ kind: 'repo' as 'repo' | 'org', name: '' })

async function loadInstalls() {
  if (status.value?.mode !== 'app') return
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

async function enable(kind: 'repo' | 'org', owner: string, repo: string, installationId: number, strict = false) {
  error.value = ''
  try {
    await post('/api/targets', { kind, owner, repo, installationId, strict })
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
  await api(`/api/targets/${t.id}`, { method: 'DELETE' })
}

function statusClass(s: string) {
  return { listening: 'ok', starting: 'busy', error: 'error', disabled: 'muted', 'no-credentials': 'error' }[s] ?? 'muted'
}

function filteredRepos(inst: Installation) {
  const q = search.value.toLowerCase()
  return inst.repos.filter((r) => !q || r.fullName.toLowerCase().includes(q))
}

function copy(text: string) {
  navigator.clipboard?.writeText(text)
}
</script>

<template>
  <div class="stack-lg">
    <h1>Repositories</h1>
    <p v-if="error" class="alert error">{{ error }}</p>

    <section>
      <h2>Using Pitlane</h2>
      <p class="muted small">
        In a workflow, put <code>runs-on: &lt;label&gt;</code>. Repository-level and organization-level labels both work.
      </p>
      <p class="muted small">
        <strong>Warm runners</strong>: each one keeps one slot reserved for that repository, so its jobs start instantly
        (no runner start-up), but that slot cannot run jobs of other repositories. New repositories get one; set 0 to free its slot.
      </p>
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
              <th title="Strict isolation: fresh caches and a wiped Docker daemon for every job. Recommended for public repositories (pull requests from forks).">Strict</th>
              <th>Enabled</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!targets.length">
              <td colspan="9" class="muted center">Nothing yet: enable a repository below.</td>
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
                <button class="chip" title="Copy" @click="copy(`runs-on: ${t.scaleSetName}`)">runs-on: {{ t.scaleSetName }}</button>
              </td>
              <td>
                <span class="badge" :class="statusClass(t.status)" :title="t.error">{{ t.status }}</span>
                <div v-if="t.error" class="small error-text ellipsis" :title="t.error">{{ t.error }}</div>
              </td>
              <td>
                <input type="number" class="num" min="0" :max="t.maxRunners" :value="t.minRunners"
                  @change="update(t, { minRunners: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <input type="number" class="num" min="1" max="64" :value="t.maxRunners"
                  @change="update(t, { maxRunners: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <input type="number" class="num" min="0" step="10" :value="t.maxJobMinutes"
                  @change="update(t, { maxJobMinutes: +($event.target as HTMLInputElement).value })" />
              </td>
              <td>
                <label class="switch">
                  <input type="checkbox" :checked="t.strict" @change="update(t, { strict: ($event.target as HTMLInputElement).checked })" />
                  <span />
                </label>
              </td>
              <td>
                <label class="switch">
                  <input type="checkbox" :checked="t.enabled" @change="update(t, { enabled: ($event.target as HTMLInputElement).checked })" />
                  <span />
                </label>
              </td>
              <td class="actions"><button class="ghost small danger" @click="remove(t)">Remove</button></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <section v-if="status?.mode === 'app'">
      <div class="row-between">
        <h2>Add repositories</h2>
        <div class="filters">
          <input v-model="search" placeholder="Filter repositories…" />
          <button class="ghost small" @click="loadInstalls" :disabled="loading">{{ loading ? 'Loading…' : 'Refresh' }}</button>
          <a v-if="installUrl" class="button small" :href="installUrl" target="_blank" rel="noopener">Install on another account ↗</a>
        </div>
      </div>
      <p v-if="!loading && !installs.length" class="alert info">
        The GitHub App is not installed anywhere yet.
        <a :href="installUrl" target="_blank" rel="noopener">Install it</a>, then click Refresh.
      </p>
      <div v-for="inst in installs" :key="inst.id" class="card account">
        <div class="row-between">
          <h3>{{ inst.account }} <span class="muted small">{{ inst.type === 'Organization' ? 'organization' : 'personal account' }}</span></h3>
          <button v-if="inst.type === 'Organization'" class="small"
            :disabled="has(`org:${inst.account}`)" @click="enable('org', inst.account, '', inst.id)">
            {{ has(`org:${inst.account}`) ? 'Whole organization enabled' : 'Enable for the whole organization' }}
          </button>
        </div>
        <p v-if="inst.error" class="alert error">{{ inst.error }}</p>
        <ul class="repo-list">
          <li v-for="r in filteredRepos(inst)" :key="r.fullName">
            <span>{{ r.name }} <span v-if="r.private" class="muted small">private</span>
              <span v-else class="badge warn" title="Anyone can open a pull request that runs code on your runners">public</span></span>
            <button class="small" :class="{ primary: !has(`repo:${r.fullName}`) }" :disabled="has(`repo:${r.fullName}`)"
              @click="enable('repo', inst.account, r.name, inst.id, !r.private)">
              {{ has(`repo:${r.fullName}`) ? 'Enabled' : 'Enable' }}
            </button>
          </li>
          <li v-if="!filteredRepos(inst).length" class="muted small">No repository matches.</li>
        </ul>
      </div>
    </section>

    <section v-else class="card">
      <h2>Add a repository or organization</h2>
      <form class="filters" @submit.prevent="addManual">
        <select v-model="manual.kind">
          <option value="repo">Repository</option>
          <option value="org">Organization</option>
        </select>
        <input v-model="manual.name" :placeholder="manual.kind === 'repo' ? 'owner/repo' : 'organization'" required />
        <button class="primary">Add</button>
      </form>
    </section>
  </div>
</template>
