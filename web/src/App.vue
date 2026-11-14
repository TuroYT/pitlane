<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, post, connectLive, disconnectLive, live, type Status } from './api'
import { status } from './session'
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
    if (path === '/login') await router.replace('/')
  }
  ready.value = true
}

onMounted(async () => {
  await router.isReady()
  await refresh()
})
watch(() => route.path, (p) => {
  if (ready.value && p !== '/setup' && p !== '/login' && !status.value?.signedIn) refresh()
})

async function logout() {
  await post('/api/logout', {})
  disconnectLive()
  status.value = null
  await router.replace('/login')
  await refresh()
}

const nav = [
  { to: '/', label: 'Dashboard', icon: 'dashboard' },
  { to: '/targets', label: 'Repositories', icon: 'repo' },
  { to: '/jobs', label: 'Jobs', icon: 'jobs' },
  { to: '/settings', label: 'Settings', icon: 'settings' },
]
</script>

<template>
  <div v-if="!ready" class="boot">Loading…</div>
  <RouterView v-else-if="route.meta.bare" @refresh="refresh" />
  <div v-else class="shell">
    <header class="header">
      <div class="header-top">
        <RouterLink to="/" class="brand"><BrandMark :size="28" /> Pitlane</RouterLink>
        <div class="who">
          <span class="live" :title="live.connected ? 'Live updates connected' : 'Reconnecting…'">
            <span class="dot" :class="live.connected ? 'ok' : 'off'" />{{ live.connected ? 'Live' : 'Reconnecting…' }}
          </span>
          <span class="login muted">@{{ status?.user }}</span>
          <button class="small" @click="logout">Sign out</button>
        </div>
      </div>
      <nav class="underline-nav" aria-label="Main">
        <RouterLink v-for="n in nav" :key="n.to" :to="n.to"
          :class="{ active: n.to === '/' ? route.path === '/' : route.path.startsWith(n.to) || (n.to === '/jobs' && route.path.startsWith('/logs')) }"
          :aria-current="(n.to === '/' ? route.path === '/' : route.path.startsWith(n.to)) ? 'page' : undefined">
          <Icon :name="n.icon" />{{ n.label }}
        </RouterLink>
      </nav>
    </header>
    <main class="content">
      <RouterView />
    </main>
    <footer class="foot muted">
      <span>Pitlane {{ status?.version }}</span><span>·</span>
      <a href="https://github.com/TuroYT/pitlane" target="_blank" rel="noopener">Source</a><span>·</span>
      <a href="https://github.com/TuroYT/pitlane/tree/main/docs" target="_blank" rel="noopener">Docs</a>
    </footer>
  </div>
</template>
