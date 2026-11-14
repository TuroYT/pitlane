<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, post, connectLive, disconnectLive, live, type Status } from './api'
import { status } from './session'

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
  { to: '/', label: 'Dashboard' },
  { to: '/targets', label: 'Repositories' },
  { to: '/jobs', label: 'Jobs' },
  { to: '/settings', label: 'Settings' },
]
</script>

<template>
  <div v-if="!ready" class="boot">Loading…</div>
  <RouterView v-else-if="route.meta.bare" @refresh="refresh" />
  <div v-else class="shell">
    <header class="topbar">
      <RouterLink to="/" class="brand"><span class="logo">P</span> Pitlane</RouterLink>
      <nav>
        <RouterLink v-for="n in nav" :key="n.to" :to="n.to" :class="{ active: n.to === '/' ? route.path === '/' : route.path.startsWith(n.to) }">
          {{ n.label }}
        </RouterLink>
      </nav>
      <div class="who">
        <span class="dot" :class="live.connected ? 'ok' : 'off'" :title="live.connected ? 'Live' : 'Reconnecting…'" />
        <span class="muted">@{{ status?.user }}</span>
        <button class="ghost small" @click="logout">Sign out</button>
      </div>
    </header>
    <main class="content">
      <RouterView />
    </main>
    <footer class="foot muted">Pitlane {{ status?.version }} · <a href="https://github.com/TuroYT/pitlane" target="_blank" rel="noopener">GitHub</a></footer>
  </div>
</template>
