import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import type { Job } from './api'
import { jobSource, jobState, parseRef, splitWorkflowRef } from './source'
import JobStatus from './components/JobStatus.vue'

const job = (over: Partial<Job> = {}): Job => ({
  id: 1, targetId: 1, jobId: 'j', runId: 42, owner: 'acme', repo: 'api', workflowRef: 'ci.yml', jobName: 'build',
  event: 'push', runnerName: 'pitlane-a', slot: 0, queuedAt: 1, startedAt: 2, finishedAt: 3, result: 'succeeded',
  workflowFullRef: 'acme/api/.github/workflows/ci.yml@refs/heads/main', ...over,
})

describe('job source', () => {
  it('splits workflow references', () => {
    expect(splitWorkflowRef('a/b/.github/workflows/x.yml@refs/tags/v1')).toEqual({ path: 'a/b/.github/workflows/x.yml', ref: 'refs/tags/v1' })
    expect(splitWorkflowRef('a/b/x.yml')).toEqual({ path: 'a/b/x.yml', ref: '' })
  })

  it('parses refs into links', () => {
    const repo = 'https://github.com/acme/api'
    expect(parseRef('refs/heads/feat/x', repo)).toMatchObject({ kind: 'branch', label: 'feat/x', url: `${repo}/tree/feat/x` })
    expect(parseRef('refs/pull/42/merge', repo)).toMatchObject({ kind: 'pr', label: '#42', url: `${repo}/pull/42` })
    expect(parseRef('refs/tags/v1.2', repo)).toMatchObject({ kind: 'tag', url: `${repo}/releases/tag/v1.2` })
    expect(parseRef('', repo)).toBeNull()
  })

  it('links a job to GitHub', () => {
    const s = jobSource(job())
    expect(s.repoUrl).toBe('https://github.com/acme/api')
    expect(s.workflowUrl).toBe('https://github.com/acme/api/actions/workflows/ci.yml')
    expect(s.ref?.label).toBe('main')
    expect(s.runUrl).toBe('https://github.com/acme/api/actions/runs/42')
    expect(s.jobUrl).toBe('/api/runners/pitlane-a/github')
    expect(s.eventLabel).toBe('push')
  })

  it('recognises reusable workflows of another repository', () => {
    const s = jobSource(job({ workflowFullRef: 'acme/shared/.github/workflows/build.yml@v1', event: 'workflow_dispatch' }))
    expect(s.calledWorkflow).toBe('acme/shared/.github/workflows/build.yml@v1')
    expect(s.workflowUrl).toBe('https://github.com/acme/shared/blob/v1/.github/workflows/build.yml')
    expect(s.ref).toBeNull()
    expect(s.eventLabel).toBe('manual')
  })

  it('derives the job state', () => {
    expect(jobState(job({ finishedAt: 0, startedAt: 0 }))).toBe('queued')
    expect(jobState(job({ finishedAt: 0 }))).toBe('running')
    expect(jobState(job({ result: 'Failure' }))).toBe('failed')
    expect(jobState(job({ result: 'cancelled' }))).toBe('canceled')
    expect(jobState(job({ result: 'skipped' }))).toBe('done')
  })

  it('renders the job status', () => {
    const w = mount(JobStatus, { props: { job: job({ result: 'failed' }), label: true } })
    expect(w.classes()).toContain('error')
    expect(w.text()).toBe('Failure')
    expect(w.attributes('title')).toBe('Failure')
  })
})
