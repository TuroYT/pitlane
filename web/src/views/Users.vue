<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, post, roleHelp, type Access, type Role } from '../api'
import { status } from '../session'

const roles: Role[] = ['viewer', 'operator', 'admin']
const access = ref<Access | null>(null)
const error = ref('')
const busy = ref(false)
const newUser = ref({ login: '', role: 'viewer' as Role })
const newRule = ref({ org: '', team: '', role: 'viewer' as Role })

async function run(f: () => Promise<Access>) {
  busy.value = true
  error.value = ''
  try {
    access.value = await f()
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}

onMounted(() => run(() => api<Access>('/api/users')))

const path = (login: string) => `/api/users/${encodeURIComponent(login)}`

async function addUser() {
  const login = newUser.value.login.trim().replace(/^@/, '')
  if (!login) return
  await run(() => post<Access>(path(login), { role: newUser.value.role }, 'PUT'))
  if (!error.value) newUser.value.login = ''
}

async function setRole(login: string, role: string) {
  if (login.toLowerCase() === status.value?.user.toLowerCase() && role !== 'admin' &&
    !confirm(`Make yourself ${role}? You lose access to this page right away.`)) {
    await run(() => api<Access>('/api/users')) // put the select back
    return
  }
  await run(() => post<Access>(path(login), { role }, 'PUT'))
}

async function removeUser(login: string) {
  if (!confirm(`Remove @${login}? Their sessions end now.`)) return
  await run(() => api<Access>(path(login), { method: 'DELETE' }))
}

async function addRule() {
  if (!newRule.value.org.trim()) return
  await run(() => post<Access>('/api/access-rules', newRule.value))
  if (!error.value) newRule.value = { org: '', team: '', role: 'viewer' }
}

async function removeRule(id: number, name: string) {
  if (!confirm(`Remove the rule for ${name}? Who signed in through it loses access now.`)) return
  await run(() => api<Access>(`/api/access-rules/${id}`, { method: 'DELETE' }))
}

function day(unix: number) {
  return unix ? new Date(unix * 1000).toLocaleDateString() : '—'
}
</script>

<template>
  <div class="stack-lg">
    <h1>Users</h1>
    <p v-if="error" class="alert error">{{ error }}</p>
    <ul class="muted small roles">
      <li v-for="r in roles" :key="r"><strong>{{ r }}</strong>: {{ roleHelp[r] }}.</li>
    </ul>

    <section>
      <h2>GitHub users</h2>
      <p class="muted small">
        A user listed here gets this role, whatever the organization rules below say. There is always at least one admin.
      </p>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr><th>Login</th><th>Role</th><th>Added</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="u in access?.users ?? []" :key="u.login">
              <td>
                <a :href="`https://github.com/${u.login}`" target="_blank" rel="noopener">@{{ u.login }}</a>
                <span v-if="u.login.toLowerCase() === status?.user.toLowerCase()" class="muted small"> (you)</span>
              </td>
              <td>
                <select :value="u.role" :disabled="busy" :aria-label="`Role of ${u.login}`"
                  @change="setRole(u.login, ($event.target as HTMLSelectElement).value)">
                  <option v-for="r in roles" :key="r" :value="r">{{ r }}</option>
                </select>
              </td>
              <td class="muted small">{{ day(u.createdAt) }}<template v-if="u.createdBy"> by {{ u.createdBy }}</template></td>
              <td class="actions"><button class="ghost small danger" :disabled="busy" @click="removeUser(u.login)">Remove</button></td>
            </tr>
          </tbody>
        </table>
      </div>
      <form class="filters add" @submit.prevent="addUser">
        <input v-model="newUser.login" placeholder="GitHub login" aria-label="GitHub login" required />
        <select v-model="newUser.role" aria-label="Role">
          <option v-for="r in roles" :key="r" :value="r">{{ r }}</option>
        </select>
        <button class="primary" :disabled="busy">Add user</button>
      </form>
    </section>

    <section>
      <h2>Organization and team rules</h2>
      <p class="muted small">
        Anyone else who signs in gets the highest role of the rules they match, checked on GitHub at sign-in.
        Public organization membership always works; private membership and teams need the GitHub App's
        <em>Members</em> (read-only) organization permission, on an App installed in that organization.
        Membership changes on GitHub apply at the next sign-in.
      </p>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr><th>Members of</th><th>Role</th><th>Added</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-if="!access?.rules.length">
              <td colspan="4" class="muted center">No rule: only the users above can sign in.</td>
            </tr>
            <tr v-for="r in access?.rules ?? []" :key="r.id">
              <td>
                <strong>{{ r.org }}</strong><template v-if="r.team"> / team <strong>{{ r.team }}</strong></template>
                <span v-else class="muted small"> (whole organization)</span>
              </td>
              <td>{{ r.role }}</td>
              <td class="muted small">{{ day(r.createdAt) }}<template v-if="r.createdBy"> by {{ r.createdBy }}</template></td>
              <td class="actions">
                <button class="ghost small danger" :disabled="busy" @click="removeRule(r.id, r.team ? `${r.org}/${r.team}` : r.org)">Remove</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <form class="filters add" @submit.prevent="addRule">
        <input v-model="newRule.org" placeholder="organization" aria-label="Organization" required />
        <input v-model="newRule.team" placeholder="team slug (optional)" aria-label="Team slug" />
        <select v-model="newRule.role" aria-label="Role">
          <option v-for="r in roles" :key="r" :value="r">{{ r }}</option>
        </select>
        <button class="primary" :disabled="busy">Add rule</button>
      </form>
    </section>
  </div>
</template>

<style scoped>
.roles { margin: 0; padding-left: 18px; }
.add { margin-top: 12px; }
</style>
