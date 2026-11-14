// Log display: GitHub job logs and runner output are streamed in chunks and
// can be huge, so each line is parsed once, when it is complete, and only the
// last lines are kept.

/** A parsed log line: its GitHub timestamp ("" if none) and its text. */
export interface LogLine {
  ts: string
  body: string
}

/** How many lines the log view keeps (older ones are dropped). */
export const MAX_LOG_LINES = 20_000

// GitHub log lines: "2026-11-14T16:50:33.2074587Z ##[group]Run actions/checkout@v4"
const tsRe = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z ?/
// eslint-disable-next-line no-control-regex -- ANSI escape sequences start with ESC
const ansiRe = /\x1b\[[0-9;]*[A-Za-z]/g

/**
 * Parses one log line: strips ANSI colors, splits the timestamp off and
 * turns GitHub's workflow commands into symbols. Returns null for lines that
 * are not shown (##[endgroup]).
 */
export function parseLogLine(line: string): LogLine | null {
  const l = line.replace(ansiRe, '')
  const ts = l.match(tsRe)?.[0] ?? ''
  let body = l.slice(ts.length)
  if (body.startsWith('##[group]')) body = '▸ ' + body.slice(9)
  else if (body.startsWith('##[endgroup]')) return null
  else if (body.startsWith('##[error]')) body = '✖ ' + body.slice(9)
  else if (body.startsWith('##[warning]')) body = '⚠ ' + body.slice(11)
  return { ts, body }
}

/** Formats a parsed line, with its time of day when timestamps is set. */
export function formatLogLine(l: LogLine, timestamps: boolean): string {
  return (timestamps && l.ts ? l.ts.slice(11, 19) + '  ' : '') + l.body
}

/** Accumulates a streamed log, parsing each line once and keeping the last max lines. */
export class LogBuffer {
  lines: LogLine[] = []
  /** Lines dropped from the start to stay within max. */
  dropped = 0
  /** The incomplete last line, parsed once its newline arrives. */
  partial = ''
  private started = false

  constructor(public readonly max = MAX_LOG_LINES) {}

  push(chunk: string) {
    if (!chunk) return
    if (!this.started) {
      chunk = chunk.replace(/^\uFEFF/, '')
      this.started = true
    }
    const parts = (this.partial + chunk).split('\n')
    this.partial = parts.pop() ?? ''
    for (const p of parts) {
      const l = parseLogLine(p)
      if (l) this.lines.push(l)
    }
    if (this.lines.length > this.max) {
      const n = this.lines.length - this.max
      this.lines.splice(0, n)
      this.dropped += n
    }
  }

  /** The text shown, the incomplete last line included. */
  text(timestamps: boolean): string {
    const out = this.lines.map((l) => formatLogLine(l, timestamps))
    const last = this.partial ? parseLogLine(this.partial) : null
    if (last) out.push(formatLogLine(last, timestamps))
    return out.join('\n')
  }
}
