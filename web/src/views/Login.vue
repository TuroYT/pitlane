<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { status } from '../session'

const route = useRoute()
const error = computed(() => {
  switch (route.query.error) {
    case 'forbidden':
      return `@${route.query.login ?? 'this account'} is not allowed to use this Pitlane. Ask an administrator to add you in Settings.`
    case 'state':
      return 'The sign-in link expired. Please try again.'
    case 'oauth':
      return 'GitHub sign-in failed. Please try again.'
    default:
      return ''
  }
})
</script>

<template>
  <div class="center-page">
    <div class="card narrow stack">
      <h1 class="brand-big"><span class="logo">P</span> Pitlane</h1>
      <p class="muted">Fast, ephemeral self-hosted GitHub Actions runners.</p>
      <p v-if="error" class="alert error">{{ error }}</p>
      <p v-if="status?.mode === 'pat'" class="alert warn">
        Pitlane runs with a personal access token: GitHub sign-in is not available. Start it with
        <code>--insecure-no-auth</code> behind your own authentication, or set up a GitHub App.
      </p>
      <a v-else class="button primary" href="/auth/login">Sign in with GitHub</a>
    </div>
  </div>
</template>
