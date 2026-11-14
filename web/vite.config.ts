/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// During development, API calls are proxied to a running Pitlane (default :8080).
const target = process.env.PITLANE_URL ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': { target, changeOrigin: false },
      '/auth': { target, changeOrigin: false },
      '/setup/callback': { target, changeOrigin: false },
    },
  },
  build: { outDir: 'dist', emptyOutDir: false, chunkSizeWarningLimit: 600 },
  test: { environment: 'happy-dom', include: ['src/**/*.test.ts'], restoreMocks: true },
})
