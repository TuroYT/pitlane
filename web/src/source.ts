import type { Job } from './api'

/** Where a job comes from, with links to GitHub. */
export interface JobSource {
  repo: string
  repoUrl: string
  /** Workflow file name ("ci.yml"). */
  workflow: string
  workflowUrl: string
  /** Set when the job comes from a reusable workflow of another repository: "org/shared/.github/workflows/build.yml@v1". */
  calledWorkflow: string
  ref: { kind: 'branch' | 'pr' | 'tag' | 'ref'; name: string; label: string; url: string } | null
  event: string
  eventLabel: string
  eventIcon: string
  runUrl: string
  /** Redirects to the job page on GitHub (resolved by the server). */
  jobUrl: string
}

const gh = 'https://github.com'
const enc = (s: string) => s.split('/').map(encodeURIComponent).join('/')

const events: Record<string, [string, string]> = {
  push: ['push', 'commit'],
  pull_request: ['pull request', 'pr'],
  pull_request_target: ['pull request target', 'pr'],
  merge_group: ['merge queue', 'pr'],
  workflow_dispatch: ['manual', 'play'],
  repository_dispatch: ['repository dispatch', 'play'],
  schedule: ['schedule', 'clock'],
  release: ['release', 'tag'],
  workflow_run: ['workflow run', 'workflow'],
  workflow_call: ['workflow call', 'workflow'],
}

/** Splits "owner/repo/.github/workflows/ci.yml@refs/heads/main" into path and ref. */
export function splitWorkflowRef(full: string): { path: string; ref: string } {
  const i = full.indexOf('@')
  return i < 0 ? { path: full, ref: '' } : { path: full.slice(0, i), ref: full.slice(i + 1) }
}

export function parseRef(ref: string, repoUrl: string): JobSource['ref'] {
  if (!ref) return null
  let m = ref.match(/^refs\/heads\/(.+)$/)
  if (m) return { kind: 'branch', name: m[1], label: m[1], url: `${repoUrl}/tree/${enc(m[1])}` }
  m = ref.match(/^refs\/pull\/(\d+)\/(merge|head)$/)
  if (m) return { kind: 'pr', name: m[1], label: `#${m[1]}`, url: `${repoUrl}/pull/${m[1]}` }
  m = ref.match(/^refs\/tags\/(.+)$/)
  if (m) return { kind: 'tag', name: m[1], label: m[1], url: `${repoUrl}/releases/tag/${enc(m[1])}` }
  return { kind: 'ref', name: ref, label: ref, url: `${repoUrl}/tree/${enc(ref)}` }
}

export function jobSource(j: Job): JobSource {
  const repo = `${j.owner}/${j.repo}`
  const repoUrl = `${gh}/${enc(repo)}`
  const { path, ref } = splitWorkflowRef(j.workflowFullRef ?? '')
  // "owner/repo/.github/workflows/ci.yml": the workflow lives in another
  // repository when it is a reusable workflow; its ref is then the ref of that
  // workflow, not the branch the run was triggered on.
  const wfRepo = path.split('/').slice(0, 2).join('/')
  const reusable = !!path && wfRepo.toLowerCase() !== repo.toLowerCase()
  const workflow = j.workflowRef || path.split('/').pop() || ''
  let workflowUrl = ''
  if (reusable) workflowUrl = `${gh}/${enc(wfRepo)}/blob/${enc(ref || 'HEAD')}/${enc(path.split('/').slice(2).join('/'))}`
  else if (workflow) workflowUrl = `${repoUrl}/actions/workflows/${encodeURIComponent(workflow)}`
  const [eventLabel, eventIcon] = events[j.event] ?? [j.event.replace(/_/g, ' '), 'event']
  return {
    repo,
    repoUrl,
    workflow,
    workflowUrl,
    calledWorkflow: reusable ? j.workflowFullRef ?? '' : '',
    ref: reusable ? null : parseRef(j.ref || ref, repoUrl),
    event: j.event,
    eventLabel,
    eventIcon,
    runUrl: j.runId ? `${repoUrl}/actions/runs/${j.runId}` : '',
    jobUrl: j.runId && j.runnerName ? `/api/runners/${encodeURIComponent(j.runnerName)}/github` : '',
  }
}

export type JobState = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled' | 'done'

export function jobState(j: Job): JobState {
  if (!j.finishedAt) return j.startedAt ? 'running' : 'queued'
  const r = j.result.toLowerCase()
  if (r === 'succeeded' || r === 'success') return 'succeeded'
  if (r === 'failed' || r === 'failure') return 'failed'
  if (r === 'canceled' || r === 'cancelled') return 'canceled'
  return 'done'
}
