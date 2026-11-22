<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, post, connectLive, disconnectLive, live, onUnauthorized, roleHelp, type Role, type Status } from './api'
import { can, status } from './session'
import BrandMark from './components/BrandMark.vue'
import Icon from './components/Icon.vue'

const route = useRoute()
const router = useRouter()
const ready = ref(false)

async function refresh() {
  status.value = await api<Status>('/api/status')
  const path = route.path
  if (status.value.needsSetup || status.value.mode === 'none') {
    if (path !== '/setup') await router.replace('/setup')
  } else if (!status.value.signedIn) {
    if (path !== '/login' && path !== '/setup') await router.replace('/login')
  } else {
    connectLive()
    if (path === '/login' || !allowed(route.meta.role)) await router.replace('/')
  }
  ready.value = true
}

// Pages the role cannot use (the API refuses them anyway) lead home.
function allowed(role: unknown) {
  return !role || can(role as Role)
}

// The session is gone (expired, user removed): back to the login page.
onUnauthorized(() => {
  disconnectLive()
  if (status.value) status.value = { ...status.value, signedIn: false }
  if (route.path !== '/login' && route.path !== '/setup') void router.replace('/login')
})

onMounted(async () => {
  await router.isReady()
  await refresh()
})
watch(() => route.path, (p) => {
  if (ready.value && p !== '/setup' && p !== '/login' && !status.value?.signedIn) refresh()
  else if (ready.value && status.value?.signedIn && !allowed(route.meta.role)) void router.replace('/')
})

async function logout() {
  await post('/api/logout', {})
  disconnectLive()
  status.value = null
  await router.replace('/login')
  await refresh()
}

// `short` is the rail label when the full one does not fit under an icon.
const allNav: { to: string; label: string; short?: string; icon: string; role?: Role }[] = [
  { to: '/', label: 'Dashboard', icon: 'dashboard' },
  { to: '/targets', label: 'Repositories', icon: 'repo' },
  { to: '/jobs', label: 'Jobs', icon: 'jobs' },
  { to: '/settings', label: 'Settings', icon: 'settings', role: 'operator' },
  { to: '/users', label: 'Users', icon: 'users', role: 'admin' },
  { to: '/audit', label: 'Audit log', short: 'Audit', icon: 'audit', role: 'admin' },
]
const nav = computed(() => allNav.filter((n) => !n.role || can(n.role)))

// A job's log page belongs to Jobs.
function isActive(to: string) {
  const p = route.path
  if (to === '/') return p === '/'
  return p.startsWith(to) || (to === '/jobs' && p.startsWith('/logs'))
}

// On a narrow screen the tab bar scrolls: keep the current tab in view.
const navEl = ref<HTMLElement>()
watch(() => [route.path, ready.value], () => nextTick(() => {
  const a = navEl.value?.querySelector<HTMLElement>('[aria-current="page"]')
  a?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
}))
const role = computed(() => status.value?.role || '')
</script>

<template>
  <div v-if="!ready" class="boot" role="status">Loading…</div>
  <RouterView v-else-if="route.meta.bare" @refresh="refresh" />
  <div v-else class="shell">
    <a class="skip" href="#main">Skip to content</a>
    <aside class="rail">
      <RouterLink to="/" class="rail-brand" aria-label="Pitlane"><BrandMark :size="36" /></RouterLink>
      <nav ref="navEl" class="rail-nav" aria-label="Main">
        <RouterLink v-for="n in nav" :key="n.to" :to="n.to" :class="{ active: isActive(n.to) }"
          :aria-label="n.label" :aria-current="isActive(n.to) ? 'page' : undefined">
          <span class="rail-icon"><Icon :name="n.icon" :size="20" /></span>{{ n.short ?? n.label }}
        </RouterLink>
      </nav>
    </aside>
    <div class="sheet">
      <header class="topbar">
        <RouterLink to="/" class="topbar-brand"><BrandMark :size="24" /> Pitlane</RouterLink>
        <div class="who">
          <span class="live" role="status" :title="live.connected ? 'Live updates connected' : 'Reconnecting…'">
            <span class="dot" :class="live.connected ? 'ok' : 'off'" />{{ live.connected ? 'Live' : 'Reconnecting…' }}
          </span>
          <span class="login muted">@{{ status?.user }}</span>
          <span v-if="role" class="badge muted" :title="roleHelp[role]">{{ role }}</span>
          <button type="button" class="small" @click="logout">Sign out</button>
        </div>
      </header>
      <main id="main" class="content" tabindex="-1">
        <RouterView />
      </main>
      <footer class="foot muted">
        <span>Pitlane {{ status?.version }}</span><span>·</span>
        <a href="https://github.com/TuroYT/pitlane" target="_blank" rel="noopener">Source</a><span>·</span>
        <a href="https://github.com/TuroYT/pitlane/tree/main/docs" target="_blank" rel="noopener">Docs</a>
      </footer>
    </div>
  </div>
</template>
