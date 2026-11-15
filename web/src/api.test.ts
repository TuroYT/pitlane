import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, LIVE_RETRY_MS, ago, api, bytes, cacheSummary, connectLive, disconnectLive, duration, hasRole, live, onUnauthorized, post, targetName } from './api'

function reply(status: number, body: unknown, statusText = '') {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), { status, statusText })
}

describe('api', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  beforeEach(() => {
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    onUnauthorized(() => {})
  })
  afterEach(() => vi.unstubAllGlobals())

  it('sends the X-Pitlane header on every call', async () => {
    fetchMock.mockResolvedValue(reply(200, { ok: true }))
    await api('/api/state')
    const [path, init] = fetchMock.mock.calls[0]
    expect(path).toBe('/api/state')
    expect(init.headers['X-Pitlane']).toBe('1')
    expect(init.headers['Content-Type']).toBeUndefined()
  })

  it('sends JSON bodies with their content type and keeps caller headers', async () => {
    fetchMock.mockResolvedValue(reply(200, { id: 3 }))
    expect(await post('/api/targets', { owner: 'acme' }, 'PATCH')).toEqual({ id: 3 })
    const [, init] = fetchMock.mock.calls[0]
    expect(init.method).toBe('PATCH')
    expect(init.body).toBe('{"owner":"acme"}')
    expect(init.headers).toMatchObject({ 'X-Pitlane': '1', 'Content-Type': 'application/json' })

    fetchMock.mockResolvedValue(reply(200, {}))
    await api('/x', { headers: { Accept: 'text/plain' } })
    expect(fetchMock.mock.calls[1][1].headers).toMatchObject({ 'X-Pitlane': '1', Accept: 'text/plain' })
  })

  it('throws the server error message', async () => {
    fetchMock.mockResolvedValue(reply(400, { error: 'invalid organization name' }))
    const err = (await api('/api/targets').catch((e: unknown) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(400)
    expect(err.message).toBe('invalid organization name')
  })

  it('falls back to the status text when the error is not JSON', async () => {
    fetchMock.mockResolvedValue(reply(502, '<html>bad gateway</html>', 'Bad Gateway'))
    await expect(api('/api/state')).rejects.toMatchObject({ status: 502, message: 'Bad Gateway' })
  })

  it('reports a lost session once per 401 and still rejects', async () => {
    const lost = vi.fn()
    onUnauthorized(lost)
    fetchMock.mockResolvedValue(reply(401, { error: 'not signed in' }))
    await expect(api('/api/jobs')).rejects.toMatchObject({ status: 401 })
    expect(lost).toHaveBeenCalledTimes(1)

    fetchMock.mockResolvedValue(reply(403, { error: 'missing X-Pitlane header' }))
    await expect(api('/api/targets')).rejects.toMatchObject({ status: 403 })
    expect(lost).toHaveBeenCalledTimes(1)
  })
})

// A minimal EventSource the tests drive.
class FakeEventSource {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 2
  static last: FakeEventSource | null = null
  readyState = FakeEventSource.CONNECTING
  onopen: (() => void) | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  constructor(public url: string) {
    FakeEventSource.last = this
  }
  close() {
    this.readyState = FakeEventSource.CLOSED
  }
}

describe('live updates', () => {
  let fetchMock: ReturnType<typeof vi.fn>
  beforeEach(() => {
    vi.useFakeTimers()
    fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    vi.stubGlobal('EventSource', FakeEventSource)
    FakeEventSource.last = null
  })
  afterEach(() => {
    disconnectLive()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('stores snapshots and the connection state', () => {
    connectLive()
    const es = FakeEventSource.last!
    expect(es.url).toBe('/api/events')
    es.onopen!()
    expect(live.connected).toBe(true)
    es.onmessage!({ data: '{"time":7,"targets":[],"slots":[]}' })
    expect(live.snap?.time).toBe(7)
    es.onerror!() // network error: the browser reconnects by itself
    expect(live.connected).toBe(false)
    connectLive()
    expect(FakeEventSource.last).toBe(es)
  })

  it('sends the user to the login page when the stream closes on an expired session', async () => {
    const lost = vi.fn()
    onUnauthorized(lost)
    fetchMock.mockResolvedValue(reply(200, { signedIn: false }))
    connectLive()
    const es = FakeEventSource.last!
    es.readyState = FakeEventSource.CLOSED
    es.onerror!()
    await vi.waitFor(() => expect(lost).toHaveBeenCalledTimes(1))
    expect(fetchMock.mock.calls[0][0]).toBe('/api/status')
    await vi.advanceTimersByTimeAsync(LIVE_RETRY_MS * 2)
    expect(FakeEventSource.last).toBe(es) // not reopened
  })

  it('reopens a stream closed for another reason', async () => {
    const lost = vi.fn()
    onUnauthorized(lost)
    fetchMock.mockResolvedValue(reply(200, { signedIn: true }))
    connectLive()
    const es = FakeEventSource.last!
    es.readyState = FakeEventSource.CLOSED
    es.onerror!()
    await vi.advanceTimersByTimeAsync(LIVE_RETRY_MS)
    expect(lost).not.toHaveBeenCalled()
    expect(FakeEventSource.last).not.toBe(es)
  })

  it('does not reopen after disconnectLive', async () => {
    fetchMock.mockRejectedValue(new TypeError('Failed to fetch'))
    connectLive()
    const es = FakeEventSource.last!
    es.readyState = FakeEventSource.CLOSED
    es.onerror!()
    await vi.advanceTimersByTimeAsync(0)
    disconnectLive()
    await vi.advanceTimersByTimeAsync(LIVE_RETRY_MS * 2)
    expect(FakeEventSource.last).toBe(es)
  })
})

describe('formatters', () => {
  it('duration', () => {
    expect(duration(-1)).toBe('—')
    expect(duration(NaN)).toBe('—')
    expect(duration(0)).toBe('0s')
    expect(duration(59.4)).toBe('59s')
    expect(duration(60)).toBe('1m')
    expect(duration(95)).toBe('1m 35s')
    expect(duration(3600)).toBe('1h 0m')
    expect(duration(3 * 3600 + 25 * 60 + 9)).toBe('3h 25m')
  })

  it('ago', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(1_000_000 * 1000))
    expect(ago(0)).toBe('—')
    expect(ago(1_000_000 - 90)).toBe('1m 30s ago')
    expect(ago(1_000_000 - 30 * 3600)).toBe('30h 0m ago')
    expect(ago(1_000_000 - 3 * 86400 - 60)).toBe('3 days ago')
    vi.useRealTimers()
  })

  it('bytes', () => {
    expect(bytes(0)).toBe('0 B')
    expect(bytes(512)).toBe('512 B')
    expect(bytes(2048)).toBe('2 KB')
    expect(bytes(5 * 1024 ** 2)).toBe('5 MB')
    expect(bytes(1.5 * 1024 ** 3)).toBe('1.5 GB')
    expect(bytes(3 * 1024 ** 5)).toBe('3072.0 TB')
  })

  it('targetName', () => {
    expect(targetName({ kind: 'repo', owner: 'acme', repo: 'api' })).toBe('acme/api')
    expect(targetName({ kind: 'org', owner: 'acme' })).toBe('acme (organization)')
  })
})

describe('cacheSummary', () => {
  it('summarizes the actions cache', () => {
    expect(cacheSummary({ entries: 2, bytes: 3 * 1024 ** 3, limitBytes: 0, hits: 3, misses: 1, saves: 2, evictions: 0 }))
      .toBe('3.0 GB in 2 entries. Since start: 3 restored, 1 missed (75% hits), 2 saved, 0 evicted.')
    expect(cacheSummary({ entries: 1, bytes: 0, limitBytes: 0, hits: 0, misses: 0, saves: 0, evictions: 0 }))
      .toBe('0 B in 1 entry. Since start: 0 restored, 0 missed, 0 saved, 0 evicted.')
  })
})

describe('hasRole', () => {
  it('orders viewer < operator < admin', () => {
    expect(hasRole('admin', 'operator')).toBe(true)
    expect(hasRole('operator', 'operator')).toBe(true)
    expect(hasRole('operator', 'admin')).toBe(false)
    expect(hasRole('viewer', 'operator')).toBe(false)
    expect(hasRole('viewer', 'viewer')).toBe(true)
  })
  it('grants nothing without a known role', () => {
    expect(hasRole('', 'viewer')).toBe(false)
    expect(hasRole(undefined, 'viewer')).toBe(false)
    expect(hasRole('root', 'viewer')).toBe(false)
  })
})
