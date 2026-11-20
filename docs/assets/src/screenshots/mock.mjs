// Mock Pitlane API for UI screenshots. node mock.mjs [port]
import http from 'node:http'

const PORT = Number(process.argv[2] ?? 18080)
const now = () => Math.floor(Date.now() / 1000)
const T0 = now()

// Deterministic PRNG.
let seed = 42
const rnd = () => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff)

const repos = [
  ['acme', 'web-app', 'ci.yml', ['build', 'test (node 22)', 'lint', 'e2e']],
  ['acme', 'api', 'ci.yml', ['go test', 'build image', 'integration']],
  ['acme', 'infra', 'deploy.yml', ['terraform plan', 'docker build']],
  ['romain', 'dotfiles', 'check.yml', ['shellcheck']],
]
const events = ['push', 'pull_request', 'pull_request', 'push', 'schedule', 'workflow_dispatch']
const refs = ['refs/heads/main', 'refs/pull/412/merge', 'refs/heads/feat/cache-warmup', 'refs/pull/97/merge', 'refs/tags/v1.4.0']

function usage(wall) {
  const mem = (400 + rnd() * 2600) * 1048576
  const c = (frac) => ({
    memPeak: mem * frac, cpuUsec: wall * 1e6 * (0.6 + rnd() * 2.4) * frac, cpuSomeUsec: 0, ioFullUsec: 0, memFullUsec: 0,
    cpuPressurePct: rnd() * 18, ioPressurePct: rnd() * 4, memPressurePct: rnd() * 0.4,
  })
  return { wallMs: wall * 1000, memPeak: mem, runner: c(0.35), slot: c(0.65) }
}

const jobs = []
let id = 5000
for (let i = 0; i < 900; i++) {
  const [owner, repo, wf, names] = repos[Math.floor(rnd() * (i % 7 === 0 ? 4 : 3))]
  const ago = Math.floor((i / 900) ** 1.4 * 30 * 86400) + 600
  const queuedAt = T0 - ago
  const wait = 1 + Math.floor(rnd() * (rnd() < 0.1 ? 40 : 6))
  const run = 30 + Math.floor(rnd() * (names.length > 3 ? 420 : 200))
  const r = rnd()
  const ref = refs[Math.floor(rnd() * refs.length)]
  jobs.push({
    id: id--, targetId: owner === 'romain' ? 3 : 1, jobId: String(90000 + i), runId: 1200000 + i, owner, repo,
    workflowRef: wf, jobName: names[Math.floor(rnd() * names.length)], event: events[Math.floor(rnd() * events.length)],
    runnerName: `pitlane-${(0x1a2b3c + i).toString(16)}`, slot: Math.floor(rnd() * 6), queuedAt, startedAt: queuedAt + wait,
    finishedAt: queuedAt + wait + run, result: r < 0.86 ? 'Succeeded' : r < 0.95 ? 'Failed' : 'Canceled',
    workflowFullRef: `${owner}/${repo}/.github/workflows/${wf}@${ref}`, ref, usage: usage(run),
  })
}
// Two running, one queued.
const running = [
  { slot: 1, name: 'e2e', repo: 'web-app', ref: 'refs/pull/412/merge', ev: 'pull_request', start: 214 },
  { slot: 3, name: 'build image', repo: 'api', ref: 'refs/heads/main', ev: 'push', start: 48 },
]
running.forEach((r, k) => {
  const owner = 'acme'
  jobs.unshift({
    id: 6000 + k, targetId: 1, jobId: '99' + k, runId: 1300000 + k, owner, repo: r.repo, workflowRef: 'ci.yml', jobName: r.name,
    event: r.ev, runnerName: `pitlane-run${k}`, slot: r.slot, queuedAt: T0 - r.start - 2, startedAt: T0 - r.start, finishedAt: 0,
    result: '', workflowFullRef: `${owner}/${r.repo}/.github/workflows/ci.yml@${r.ref}`, ref: r.ref, usage: null,
  })
})
jobs.unshift({
  id: 6010, targetId: 1, jobId: '9910', runId: 1300010, owner: 'acme', repo: 'web-app', workflowRef: 'ci.yml',
  jobName: 'test (node 22)', event: 'pull_request', runnerName: '', slot: -1, queuedAt: T0 - 9, startedAt: 0, finishedAt: 0,
  result: '', workflowFullRef: 'acme/web-app/.github/workflows/ci.yml@refs/pull/412/merge', ref: 'refs/pull/412/merge', usage: null,
})

const targets = [
  { id: 1, kind: 'org', owner: 'acme', installationId: 11, enabled: true, minRunners: 1, maxRunners: 6, scaleSetName: 'pitlane', scaleSetId: 1, strict: false, maxJobMinutes: 120 },
  { id: 2, kind: 'repo', owner: 'acme', repo: 'public-docs', installationId: 11, enabled: true, minRunners: 0, maxRunners: 2, scaleSetName: 'pitlane-docs', scaleSetId: 2, strict: true, maxJobMinutes: 30, strictRequired: 'public repository', strictForcedAt: T0 - 86400 * 3 },
  { id: 3, kind: 'repo', owner: 'romain', repo: 'dotfiles', installationId: 12, enabled: false, minRunners: 0, maxRunners: 1, scaleSetName: 'pitlane', scaleSetId: 3, strict: false, maxJobMinutes: 0 },
]
function snapshot() {
  const t = now()
  return {
    time: t,
    targets: targets.map((x) => ({
      ...x,
      status: !x.enabled ? 'disabled' : x.id === 2 ? 'error' : 'listening',
      error: x.id === 2 && x.enabled ? 'GitHub API: 502 Bad Gateway while refreshing the scale set (retrying in 30s)' : undefined,
      desired: x.id === 1 ? 3 : 0, waiting: x.id === 1 ? 1 : 0,
      runners: x.id === 1 ? [
        { name: 'pitlane-run0', slot: 1, busy: true, jobName: 'e2e', repo: 'acme/web-app', startedAt: t - 214 },
        { name: 'pitlane-run1', slot: 3, busy: true, jobName: 'build image', repo: 'acme/api', startedAt: t - 48 },
        { name: 'pitlane-idle', slot: 0, busy: false, jobName: '', repo: '', startedAt: 0 },
      ] : [],
    })),
    slots: [
      { index: 0, status: 'ready', owner: 'acme', targetId: 1, runner: 'pitlane-idle', since: t - 900, storageDriver: 'overlay2' },
      { index: 1, status: 'busy', owner: 'acme', targetId: 1, runner: 'pitlane-run0', since: T0 - 214, storageDriver: 'overlay2' },
      { index: 2, status: 'ready', owner: '(strict)', targetId: 0, runner: '', since: t - 3000, storageDriver: 'overlay2' },
      { index: 3, status: 'busy', owner: 'acme', targetId: 1, runner: 'pitlane-run1', since: T0 - 48, storageDriver: 'overlay2' },
      { index: 4, status: 'cleaning', owner: 'acme', targetId: 0, runner: '', since: t - 5, storageDriver: 'overlay2' },
      { index: 5, status: 'ready', owner: '', targetId: 0, runner: '', since: t - 7200, storageDriver: 'overlay2' },
    ],
    host: { cpus: 16, cpuPercent: 41, memTotal: 64 * 1073741824, memAvailable: 22 * 1073741824, diskTotal: 1000 * 1073741824, diskFree: 140 * 1073741824 },
  }
}

const compare = [
  ['acme/web-app', 'ci.yml', 'e2e', 612, 40, 188, 52],
  ['acme/web-app', 'ci.yml', 'build', 344, 60, 71, 60],
  ['acme/web-app', 'ci.yml', 'test (node 22)', 290, 60, 96, 58],
  ['acme/api', 'ci.yml', 'build image', 421, 31, 64, 44],
  ['acme/api', 'ci.yml', 'go test', 183, 55, 52, 60],
  ['acme/api', 'ci.yml', 'integration', 505, 20, 161, 25],
  ['acme/infra', 'deploy.yml', 'docker build', 266, 12, 49, 14],
  ['acme/web-app', 'ci.yml', 'lint', 58, 60, 21, 60],
  ['acme/infra', 'deploy.yml', 'terraform plan', 95, 9, 0, 0],
].map(([repo, workflow, job, hostedMedian, hostedCount, pitlaneMedian, pitlaneCount]) => ({ repo, workflow, job, hostedMedian, hostedCount, pitlaneMedian, pitlaneCount }))

const settings = {
  slots: 6, runnerImage: 'ghcr.io/turoyt/pitlane-runner:v1.0.0', dindImage: 'docker:28-dind', cpuShares: 0, memoryMb: 0,
  cacheScope: 'owner', mirror: true, qemu: false, keepBuildGb: 20, minFreeDiskGb: 50, metricsTokenSet: true, historyDays: 30,
  baseUrl: 'http://pitlane.lan:8080', actionsCache: true, actionsCacheGb: 50, actionsCacheOwnerPct: 50,
  actionsCacheStats: { entries: 312, bytes: 18.4 * 1073741824, limitBytes: 50 * 1073741824, hits: 1840, misses: 212, saves: 260, evictions: 14 },
}
const updates = {
  pitlane: {
    current: 'v1.0.0-rc.1', latest: 'v1.0.0', latestUrl: 'https://github.com/TuroYT/pitlane/releases/tag/v1.0.0', latestPublished: T0 - 86400,
    updateAvailable: true, capability: { mode: 'systemd', canSelfUpdate: true, executable: '/usr/local/bin/pitlane' }, checkedAt: T0 - 600,
    attestation: true, lastUpdate: 'v0.2.0 → v1.0.0-rc.1, 3 days ago',
  },
  runner: {
    image: 'ghcr.io/turoyt/pitlane-runner:v1.0.0', digest: 'sha256:4f1c9e2a7b3d', version: '2.338.2', latest: 'v2.339.0',
    latestUrl: 'https://github.com/actions/runner/releases/tag/v2.339.0', status: 'outdated', pulledAt: T0 - 86400 * 2, pulling: false,
  },
  settings: { autoUpdate: true, window: { start: 3, end: 5 }, autoPull: true },
}
const access = {
  users: [
    { login: 'romain', role: 'admin', createdAt: T0 - 86400 * 40, createdBy: '' },
    { login: 'alice', role: 'operator', createdAt: T0 - 86400 * 12, createdBy: 'romain' },
    { login: 'bob-the-reviewer', role: 'viewer', createdAt: T0 - 86400 * 2, createdBy: 'romain' },
  ],
  rules: [
    { id: 1, org: 'acme', team: '', role: 'viewer', createdAt: T0 - 86400 * 20, createdBy: 'romain' },
    { id: 2, org: 'acme', team: 'platform', role: 'operator', createdAt: T0 - 86400 * 5, createdBy: 'romain' },
  ],
}
const audit = Array.from({ length: 73 }, (_, i) => {
  const a = [
    ['target.update', 'acme/web-app', 'maxRunners 4 → 6'],
    ['settings.update', 'settings', 'slots 4 → 6, actionsCacheGb 20 → 50'],
    ['user.role', '@alice', 'viewer → operator'],
    ['update.apply', 'pitlane', 'v0.2.0 → v1.0.0-rc.1'],
    ['rule.add', 'acme/platform', 'operator'],
    ['target.enable', 'acme/public-docs', 'strict (public repository)'],
  ][i % 6]
  return { id: 1000 - i, at: T0 - i * 5400 - 300, actor: i % 4 === 2 ? 'alice' : 'romain', action: a[0], subject: a[1], detail: a[2] }
})
const installations = {
  installUrl: 'https://github.com/apps/pitlane-demo/installations/new',
  installations: [
    { id: 11, account: 'acme', type: 'Organization', repos: [
      { name: 'web-app', fullName: 'acme/web-app', private: true }, { name: 'api', fullName: 'acme/api', private: true },
      { name: 'infra', fullName: 'acme/infra', private: true }, { name: 'public-docs', fullName: 'acme/public-docs', private: false },
      { name: 'design-system', fullName: 'acme/design-system', private: false }, { name: 'mobile', fullName: 'acme/mobile', private: true },
    ] },
    { id: 12, account: 'romain', type: 'User', repos: [{ name: 'dotfiles', fullName: 'romain/dotfiles', private: false }] },
  ],
}
const memory = { windowDays: 7, count: 412, p50: 1.1 * 1073741824, p90: 2.6 * 1073741824, max: 5.3 * 1073741824, memTotal: 64 * 1073741824, usable: 56 * 1073741824, suggestedSlots: 21, slots: 6, memoryLimitMb: 0 }

const LOG = [
  '2026-11-15T09:12:01.123Z ##[group]Run actions/checkout@v4',
  '2026-11-15T09:12:01.200Z Syncing repository: acme/web-app',
  '2026-11-15T09:12:02.010Z ##[endgroup]',
  '2026-11-15T09:12:02.300Z ##[group]Run actions/setup-node@v4',
  '2026-11-15T09:12:02.900Z Cache restored from key: node-cache-Linux-x64-pnpm-8f2c (Pitlane actions cache)',
  '2026-11-15T09:12:03.000Z ##[endgroup]',
  '2026-11-15T09:12:03.100Z ##[group]Run pnpm install --frozen-lockfile',
  '2026-11-15T09:12:05.800Z Packages: +812',
  '2026-11-15T09:12:06.400Z Done in 3.3s',
  '2026-11-15T09:12:06.500Z ##[endgroup]',
  '2026-11-15T09:12:06.600Z ##[group]Run pnpm test:e2e',
  '2026-11-15T09:12:09.000Z   ✓ login.spec.ts (4 tests) 2.1s',
  '2026-11-15T09:12:14.000Z   ✓ checkout.spec.ts (11 tests) 4.8s',
  '2026-11-15T09:12:20.000Z   ✓ dashboard.spec.ts (7 tests) 5.9s',
].join('\n') + '\n'

function json(res, body, headers = {}) {
  res.writeHead(200, { 'Content-Type': 'application/json', ...headers })
  res.end(JSON.stringify(body))
}

http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x')
  const p = u.pathname
  if (p === '/api/status') {
    return json(res, { version: 'v1.0.0', mode: 'app', needsSetup: false, setupUnlocked: true, signedIn: true, user: 'romain', role: 'admin', baseUrl: 'http://pitlane.lan:8080', app: { slug: 'pitlane-demo', name: 'Pitlane demo', htmlUrl: '', owner: 'acme' } })
  }
  if (p === '/api/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
    const send = () => res.write(`data: ${JSON.stringify(snapshot())}\n\n`)
    send()
    const t = setInterval(send, 2000)
    req.on('close', () => clearInterval(t))
    return
  }
  if (p === '/api/jobs') {
    let list = jobs
    const repo = u.searchParams.get('repo'), result = u.searchParams.get('result'), ref = u.searchParams.get('ref')
    if (repo) list = list.filter((j) => `${j.owner}/${j.repo}` === repo)
    if (result === 'running') list = list.filter((j) => !j.finishedAt)
    else if (result) list = list.filter((j) => j.result.toLowerCase() === result)
    if (ref) list = list.filter((j) => j.ref.includes(ref))
    const off = Number(u.searchParams.get('offset') ?? 0), lim = Number(u.searchParams.get('limit') ?? 50)
    return json(res, list.slice(off, off + lim), { 'X-Total-Count': String(list.length) })
  }
  if (p === '/api/jobs/repos') return json(res, ['acme/api', 'acme/infra', 'acme/web-app', 'romain/dotfiles'])
  if (p === '/api/jobs/memory') return json(res, memory)
  if (p === '/api/compare') return setTimeout(() => json(res, compare), 300)
  if (p === '/api/settings') return json(res, settings)
  if (p.startsWith('/api/updates')) return json(res, updates)
  if (p === '/api/users' || p.startsWith('/api/users/') || p.startsWith('/api/access-rules')) return json(res, access)
  if (p === '/api/audit') {
    const off = Number(u.searchParams.get('offset') ?? 0), lim = Number(u.searchParams.get('limit') ?? 50)
    return json(res, audit.slice(off, off + lim), { 'X-Total-Count': String(audit.length) })
  }
  if (p === '/api/installations') return json(res, installations)
  if (/^\/api\/runners\/[^/]+\/(job-log|logs)/.test(p)) {
    res.writeHead(200, { 'Content-Type': 'text/plain' })
    return res.end(LOG)
  }
  if (p === '/api/logout') return json(res, {})
  res.writeHead(404, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify({ error: 'not found' }))
}).listen(PORT, () => console.log('mock on', PORT))
