# UI screenshots

Reproducible screenshots of the web UI, from a deterministic mocked API (no real Pitlane needed).

```sh
cd docs/assets/src/screenshots && npm ci
node mock.mjs 18080 &                                   # mocked /api on :18080
(cd ../../../../web && PITLANE_URL=http://localhost:18080 pnpm dev --port 5199) &
export CHROME_PATH=/path/to/chrome                      # any Chromium (e.g. from `npx playwright install chromium`)
node readme.mjs ../..                                   # docs/assets/dashboard-{light,dark}.png
node shoot.mjs /tmp/shots                               # every page, light/dark, 1440 and 375 px
```
