// node shoot.mjs <outdir> [base] [pages...]
import { chromium } from 'playwright-core'
import fs from 'node:fs'

const out = process.argv[2]
const base = process.argv[3] ?? 'http://localhost:5199'
const only = process.argv.slice(4)
fs.mkdirSync(out, { recursive: true })

const signedOut = { version: 'v1.0.0', mode: 'app', needsSetup: false, setupUnlocked: true, signedIn: false, user: '', role: '', baseUrl: 'http://pitlane.lan:8080', app: { slug: 'pitlane-demo', name: 'Pitlane demo', htmlUrl: '', owner: 'acme' } }
const pages = [
  ['dashboard', '/', { compare: true }],
  ['targets', '/targets'],
  ['jobs', '/jobs', { compare: true }],
  ['logs', '/logs/pitlane-run0'],
  ['settings', '/settings'],
  ['users', '/users'],
  ['audit', '/audit'],
  ['login', '/login?error=forbidden&login=mallory', { status: signedOut }],
  ['setup', '/setup', { status: { ...signedOut, mode: 'none', needsSetup: true, setupUnlocked: false } }],
  ['setup-create', '/setup', { status: { ...signedOut, mode: 'none', needsSetup: true, setupUnlocked: true, baseUrl: 'http://localhost:8080' } }],
].filter(([n]) => !only.length || only.includes(n))

const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH })
for (const scheme of ['light', 'dark']) {
  for (const [vw, tag] of [[1440, 'desktop'], [375, 'mobile']]) {
    const ctx = await browser.newContext({ viewport: { width: vw, height: 900 }, colorScheme: scheme, deviceScaleFactor: tag === 'mobile' ? 2 : 1, timezoneId: 'Europe/Paris', locale: 'en-GB' })
    for (const [name, path, opt = {}] of pages) {
      const page = await ctx.newPage()
      const errors = []
      page.on('pageerror', (e) => errors.push(e.message))
      page.on('console', (m) => m.type() === 'error' && errors.push(m.text()))
      if (opt.status) await page.route('**/api/status', (r) => r.fulfill({ json: opt.status }))
      await page.goto(base + path)
      await page.waitForTimeout(1500)
      if (opt.compare) {
        const b = page.getByRole('button', { name: 'Compare' }).first()
        if (await b.count()) { await b.click(); await page.waitForTimeout(900) }
      }
      const over = await page.evaluate(() => {
        const W = innerWidth
        const bad = []
        for (const el of document.querySelectorAll('body *')) {
          const r = el.getBoundingClientRect()
          if (r.width && r.right > W + 1) {
            let p = el.parentElement, clipped = false
            while (p) { const o = getComputedStyle(p).overflowX; if (o === 'auto' || o === 'scroll' || o === 'hidden') { clipped = true; break } p = p.parentElement }
            if (!clipped) bad.push(`${el.tagName.toLowerCase()}.${[...el.classList].join('.')} right=${Math.round(r.right)}`)
          }
        }
        return { scroll: document.documentElement.scrollWidth, bad: bad.slice(0, 8) }
      })
      await page.evaluate(() => scrollTo(0, 0))
      const h = await page.evaluate(() => document.documentElement.scrollHeight)
      await page.setViewportSize({ width: vw, height: Math.max(900, h) })
      await page.waitForTimeout(400)
      await page.screenshot({ path: `${out}/${name}-${scheme}-${tag}.png` })
      if (over.scroll > vw || over.bad.length || errors.length) console.log(name, scheme, tag, JSON.stringify(over), errors.join(' | '))
      await page.close()
    }
    await ctx.close()
  }
}
await browser.close()
console.log('done')
