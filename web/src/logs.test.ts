import { describe, expect, it } from 'vitest'
import { LogBuffer, formatLogLine, parseLogLine } from './logs'

describe('parseLogLine', () => {
  it('splits the GitHub timestamp off', () => {
    expect(parseLogLine('2026-11-14T16:50:33.2074587Z hello')).toEqual({ ts: '2026-11-14T16:50:33.2074587Z ', body: 'hello' })
    expect(parseLogLine('2026-11-14T16:50:33Z x')).toEqual({ ts: '2026-11-14T16:50:33Z ', body: 'x' })
    expect(parseLogLine('plain line')).toEqual({ ts: '', body: 'plain line' })
  })

  it('turns workflow commands into symbols and hides endgroup', () => {
    expect(parseLogLine('##[group]Run actions/checkout@v4')?.body).toBe('▸ Run actions/checkout@v4')
    expect(parseLogLine('2026-11-14T16:50:33Z ##[endgroup]')).toBeNull()
    expect(parseLogLine('##[error]boom')?.body).toBe('✖ boom')
    expect(parseLogLine('##[warning]careful')?.body).toBe('⚠ careful')
  })

  it('strips ANSI colors', () => {
    expect(parseLogLine('\x1b[31mred\x1b[0m and \x1b[1;32mgreen\x1b[m')?.body).toBe('red and green')
  })
})

describe('formatLogLine', () => {
  const l = { ts: '2026-11-14T16:50:33.2074587Z ', body: 'hello' }
  it('shows the time of day only when asked', () => {
    expect(formatLogLine(l, false)).toBe('hello')
    expect(formatLogLine(l, true)).toBe('16:50:33  hello')
    expect(formatLogLine({ ts: '', body: 'x' }, true)).toBe('x')
  })
})

describe('LogBuffer', () => {
  it('parses lines split across chunks once they are complete', () => {
    const b = new LogBuffer()
    b.push('﻿2026-11-14T16:50:33Z ##[gro')
    expect(b.lines).toHaveLength(0)
    expect(b.text(false)).toBe('##[gro')
    b.push('up]Build\nstep \x1b[3')
    b.push('2mone\x1b[0m\n##[endgroup]\nlast')
    expect(b.lines.map((l) => l.body)).toEqual(['▸ Build', 'step one'])
    expect(b.text(false)).toBe('▸ Build\nstep one\nlast')
    expect(b.text(true)).toBe('16:50:33  ▸ Build\nstep one\nlast')
  })

  it('removes the byte order mark only at the start', () => {
    const b = new LogBuffer()
    b.push('﻿a\n')
    b.push('﻿b\n')
    expect(b.lines.map((l) => l.body)).toEqual(['a', '﻿b'])
  })

  it('keeps only the last lines and counts the dropped ones', () => {
    const b = new LogBuffer(3)
    b.push('1\n2\n3\n')
    expect(b.dropped).toBe(0)
    b.push('4\n5\n')
    expect(b.lines.map((l) => l.body)).toEqual(['3', '4', '5'])
    expect(b.dropped).toBe(2)
  })

  it('handles a large log in linear time', () => {
    const b = new LogBuffer()
    const chunk = Array.from({ length: 1000 }, (_, i) => `2026-11-14T16:50:33Z line ${i}`).join('\n') + '\n'
    const start = performance.now()
    for (let i = 0; i < 100; i++) b.push(chunk)
    expect(b.lines).toHaveLength(20_000)
    expect(b.dropped).toBe(80_000)
    expect(performance.now() - start).toBeLessThan(5000)
  })
})
