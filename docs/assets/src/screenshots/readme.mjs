import { chromium } from 'playwright-core'
const out = process.argv[2]
const b = await chromium.launch({ executablePath: process.env.CHROME_PATH })
for (const scheme of ['light', 'dark']) {
  const p = await b.newPage({ viewport: { width: 1440, height: 1235 }, colorScheme: scheme, timezoneId: 'Europe/Paris', locale: 'en-GB' })
  await p.route('**/api/status', async (r) => { const res = await r.fetch(); const j = await res.json(); r.fulfill({ json: { ...j, user: 'demo' } }) })
  // A healthy demo: no target in error.
  await p.goto('http://localhost:5199/'); await p.waitForTimeout(400)
  await p.evaluate(() => {})
  await p.addStyleTag({ content: '.alert.error { display: none !important; }' })
  await p.waitForTimeout(2200)
  const h = await p.evaluate(() => Math.ceil(document.querySelector('.charts').getBoundingClientRect().bottom + 32))
  await p.setViewportSize({ width: 1440, height: h }); await p.waitForTimeout(500)
  await p.mouse.move(1439, h - 1)
  await p.screenshot({ path: `${out}/dashboard-${scheme}.png` })
  await p.close()
}
await b.close()
