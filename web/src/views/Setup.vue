<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import { post, ApiError } from '../api'
import { status } from '../session'

const emit = defineEmits<{ refresh: [] }>()
const route = useRoute()

const token = ref('')
const baseUrl = ref(status.value?.baseUrl ?? location.origin)
const org = ref('')
const appName = ref('')
const busy = ref(false)
const error = ref(
  route.query.error === 'conversion'
    ? 'GitHub did not accept the App creation. Please try again.'
    : route.query.error === 'state'
      ? 'The setup link expired. Please try again.'
      : route.query.error === 'store'
        ? 'Pitlane could not save the GitHub App it just created (see its logs). Delete that App on GitHub, then try again.'
        : '',
)

const step = computed(() => {
  const s = status.value
  if (!s) return 'loading'
  if (s.mode === 'none' && !s.setupUnlocked) return 'unlock'
  if (s.mode === 'none') return 'create'
  return 'install'
})
const installUrl = computed(() => `https://github.com/apps/${status.value?.app.slug}/installations/new`)
const isLocalhost = computed(() => /\/\/(localhost|127\.)/.test(baseUrl.value))

async function unlock() {
  busy.value = true
  error.value = ''
  try {
    await post('/api/setup/unlock', { token: token.value })
    emit('refresh')
  } catch (e) {
    error.value = e instanceof ApiError && e.status === 403 ? 'This setup token is not valid.' : String(e)
  } finally {
    busy.value = false
  }
}

async function createApp() {
  busy.value = true
  error.value = ''
  try {
    const res = await post<{ action: string; manifest: string }>('/api/setup/manifest', {
      baseUrl: baseUrl.value,
      org: org.value.trim(),
      name: appName.value.trim(),
    })
    // The manifest flow requires a real form POST to github.com.
    const form = document.createElement('form')
    form.method = 'post'
    form.action = res.action
    const input = document.createElement('input')
    input.type = 'hidden'
    input.name = 'manifest'
    input.value = res.manifest
    form.appendChild(input)
    document.body.appendChild(form)
    form.submit()
  } catch (e) {
    error.value = String(e instanceof Error ? e.message : e)
    busy.value = false
  }
}
</script>

<template>
  <div class="center-page">
    <div class="card narrow">
      <h1 class="brand-big"><span class="logo">P</span> Pitlane setup</h1>

      <ol class="steps">
        <li :class="{ done: step !== 'unlock', current: step === 'unlock' }">Unlock</li>
        <li :class="{ done: step === 'install', current: step === 'create' }">Create the GitHub App</li>
        <li :class="{ current: step === 'install' }">Install &amp; sign in</li>
      </ol>

      <p v-if="error" class="alert error">{{ error }}</p>

      <form v-if="step === 'unlock'" @submit.prevent="unlock" class="stack">
        <p>Enter the one-time setup token. It is printed in Pitlane's logs, or run:</p>
        <pre class="code">pitlane setup-token</pre>
        <input v-model="token" placeholder="Setup token" autofocus required autocomplete="off" />
        <button class="primary" :disabled="busy || !token">Continue</button>
      </form>

      <form v-else-if="step === 'create'" @submit.prevent="createApp" class="stack">
        <p>
          Pitlane creates its own private GitHub App in one click. It uses it to register runners and to sign you in.
          Nothing to copy or paste.
        </p>
        <label>
          Pitlane URL
          <input v-model="baseUrl" required />
          <small>The address you use to open this page. GitHub redirects your browser here, so it does not need to be public.</small>
        </label>
        <p v-if="isLocalhost" class="alert warn">
          You are using a localhost URL. Use the LAN address (e.g. http://pitlane.lan:8080) if you will open Pitlane from another computer.
        </p>
        <label>
          Create the App under an organization <span class="muted">(optional)</span>
          <input v-model="org" placeholder="Leave empty to create it on your personal account" />
        </label>
        <label>
          App name <span class="muted">(optional, must be unique on GitHub)</span>
          <input v-model="appName" placeholder="Pitlane xxxxxx" />
        </label>
        <button class="primary" :disabled="busy">Create GitHub App on github.com →</button>
      </form>

      <div v-else-if="step === 'install'" class="stack">
        <p class="alert ok">
          GitHub App <strong>{{ status?.app.name || status?.app.slug }}</strong> is ready.
        </p>
        <p><strong>1.</strong> Install it on the accounts and repositories that should use Pitlane runners:</p>
        <a class="button" :href="installUrl" target="_blank" rel="noopener">Install the App on GitHub ↗</a>
        <p><strong>2.</strong> Sign in. The first person to sign in becomes the administrator.</p>
        <a class="button primary" href="/auth/login">Sign in with GitHub</a>
      </div>
    </div>
  </div>
</template>
