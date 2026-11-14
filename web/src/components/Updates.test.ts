import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import Updates from './Updates.vue'

const state = {
  pitlane: { current: '0.1.0', latest: 'v0.1.0', updateAvailable: false, capability: { mode: 'systemd', canSelfUpdate: true } },
  runner: { image: 'ghcr.io/turoyt/pitlane-runner:v0.1.0', version: '2.338.2', latest: 'v2.338.2', status: 'current', pulling: false },
  settings: { autoUpdate: false, window: { start: 3, end: 5 }, autoPull: true },
}

describe('Updates', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
  })

  it('loads the update state without error (settings copied out of the reactive state)', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(state), { status: 200 })))
    const w = mount(Updates)
    await flushPromises()
    expect(w.find('.alert.error').exists()).toBe(false)
    expect(w.text()).toContain('0.1.0')
    expect(w.text()).not.toContain('structuredClone')
    w.unmount()
  })
})
