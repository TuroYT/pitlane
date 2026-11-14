"""Build a FigCraft command script holding every Pitlane brand asset.

usage:
  python3 docs/assets/src/build_figcraft.py . /tmp/pitlane-fc script.json
  figcraft-cli run script.json --out docs/assets/src/pitlane-brand.figcraft
"""
import json
import os
import re
import sys

REPO, WORK, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
A = os.path.join(REPO, 'docs/assets')
os.makedirs(WORK, exist_ok=True)
steps = []


def cmd(command, params=None, as_=None):
    s = {'command': command, 'params': params or {}}
    if as_:
        s['as'] = as_
    steps.append(s)


def text(x, y, s, size=14, color='#59636e', weight=None, name=None):
    props = {'fontSize': size, 'fill': color}
    if weight:
        props['fontStyle'] = weight
    cmd('node.create', {'type': 'text', 'x': x, 'y': y, 'text': s, 'name': name or s[:40], 'props': props, 'select': False})


def rect(x, y, w, h, fill, name, radius=0, stroke=None):
    props = {'fill': fill, 'cornerRadius': radius}
    if stroke:
        props.update({'stroke': stroke, 'strokeWeight': 1})
    cmd('node.create', {'type': 'rectangle', 'x': x, 'y': y, 'width': w, 'height': h, 'name': name, 'props': props, 'select': False})


def imp(path, x, y):
    cmd('file.import', {'path': path, 'x': x, 'y': y})


def scaled(src, size, name):
    """Copy an SVG with explicit width/height so it imports at that size."""
    s = open(src).read()
    s = re.sub(r'<svg ', f'<svg width="{size}" height="{size}" ', s, count=1)
    p = os.path.join(WORK, name)
    open(p, 'w').write(s)
    return p


def page(name, first=False):
    if first:
        cmd('page.rename', {'name': name})
    else:
        cmd('page.add', {'name': name}, as_=f'page-{name}')
        cmd('page.select', {'id': {'$ref': f'page-{name}'}})


cmd('file.rename', {'name': 'Pitlane brand'})

# ------------------------------------------------------------------ logo
page('Logo', first=True)
text(0, -72, 'Pitlane logo', 32, '#1f2328', 'Semi Bold')
text(0, -28, 'A P split by two speed slits: a car leaving the pit box. 32 × 32 grid, tile radius 7/32. See docs/brand.md.', 14)
imp(scaled(f'{A}/logo.svg', 256, 'logo-256.svg'), 0, 24)
x = 300
for s in (128, 64, 32, 16):
    imp(scaled(f'{A}/logo.svg', s, f'logo-{s}.svg'), x, 24 + (128 - s))
    text(x, 168, f'{s} px', 12)
    x += s + 40
rect(0, 320, 300, 200, '#ffffff', 'On light', 12, '#d1d9e0')
imp(scaled(f'{A}/logo-mono-black.svg', 120, 'mono-black.svg'), 90, 360)
text(0, 530, 'Single color, light backgrounds (logo-mono-black.svg)', 12)
rect(340, 320, 300, 200, '#0d1117', 'On dark', 12)
imp(scaled(f'{A}/logo-mono-white.svg', 120, 'mono-white.svg'), 430, 360)
text(340, 530, 'Single color, dark backgrounds (logo-mono-white.svg)', 12)
rect(680, 320, 300, 200, '#0d1117', 'Tile on dark', 12)
imp(scaled(f'{A}/logo.svg', 120, 'logo-120.svg'), 770, 360)
text(680, 530, 'Default tile on dark', 12)
rect(0, 580, 520, 140, '#ffffff', 'Wordmark on light', 12, '#d1d9e0')
imp(f'{A}/logo-wordmark-light.svg', 40, 622)
rect(560, 580, 520, 140, '#0d1117', 'Wordmark on dark', 12)
imp(f'{A}/logo-wordmark-dark.svg', 600, 622)
text(0, 730, 'Wordmark: "pitlane", lowercase, Adwaita Sans (SIL OFL, Inter-based) semibold, tracking -3%, outlined.', 12)

# ------------------------------------------------------------------ colors
page('Colors')
brand = [('Signal orange', '#E5482D', 'Logo tile, large text and graphics only'),
         ('Orange emphasis', '#CF3F22', 'Primary buttons with white text (4.79:1)'),
         ('Orange bright', '#F0684F', 'Accents on dark backgrounds (6.13:1)')]
neutral = [('Canvas', '#ffffff', '#0d1117'), ('Subtle canvas', '#f6f8fa', '#151b23'), ('Border', '#d1d9e0', '#3d444d'),
           ('Text', '#1f2328', '#f0f6fc'), ('Muted text', '#59636e', '#9198a1'), ('Link', '#0969da', '#4493f8'),
           ('Success', '#1a7f37', '#3fb950'), ('Failure', '#d1242f', '#f85149'), ('In progress', '#bf8700', '#d29922'),
           ('Warning', '#9a6700', '#d29922'), ('Canceled / idle', '#818b98', '#9198a1'),
           ('Chart p50', '#0969da', '#4493f8'), ('Chart p90', '#8250df', '#ab7df8'), ('Chart GitHub-hosted', '#8c959f', '#656c76')]
cmd('variable.createCollection', {'name': 'Pitlane', 'modes': ['Light', 'Dark']})
for n, hx, _ in brand:
    cmd('variable.create', {'collection': 'Pitlane', 'name': f'brand/{n}', 'type': 'color', 'values': {'Light': hx, 'Dark': hx}})
for n, l, d in neutral:
    cmd('variable.create', {'collection': 'Pitlane', 'name': f'ui/{n}', 'type': 'color', 'values': {'Light': l, 'Dark': d}})

text(0, -72, 'Colors', 32, '#1f2328', 'Semi Bold')
text(0, -28, 'Variables: collection "Pitlane", modes Light and Dark. Orange is the brand, never a status.', 14)
for i, (n, hx, use) in enumerate(brand):
    x = i * 260
    rect(x, 0, 240, 120, hx, n, 12)
    text(x, 132, n, 14, '#1f2328', 'Semi Bold')
    text(x, 154, hx, 12)
    text(x, 172, use, 11)
y0 = 240
text(0, y0, 'UI tokens', 20, '#1f2328', 'Semi Bold')
for i, (n, l, d) in enumerate(neutral):
    x = (i % 7) * 180
    y = y0 + 40 + (i // 7) * 170
    rect(x, y, 80, 80, l, f'{n} light', 0, '#d1d9e0')
    rect(x + 80, y, 80, 80, d, f'{n} dark', 0, '#3d444d')
    text(x, y + 90, n, 13, '#1f2328', 'Semi Bold')
    text(x, y + 110, f'{l} / {d}', 11)

# ------------------------------------------------------------------ icons
page('Icons')
text(0, -72, 'UI icons', 32, '#1f2328', 'Semi Bold')
text(0, -28, '16 px grid, 1.5 px stroke, round caps (web/src/components/Icon.vue). Shown at 48 px.', 14)
src = open(os.path.join(REPO, 'web/src/components/Icon.vue')).read()
icons = re.findall(r"^\s+(\w+): '(.+)',$", src, re.M)
for i, (n, body) in enumerate(icons):
    p = os.path.join(WORK, f'icon-{n}.svg')
    open(p, 'w').write(f'<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 16 16" fill="none" '
                       f'stroke="#1f2328" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">'
                       f'{body.replace("currentColor", "#1f2328")}</svg>')
    x, y = (i % 8) * 120, (i // 8) * 120
    imp(p, x, y)
    text(x, y + 60, n, 12)

# ------------------------------------------------------------------ README visuals
page('README visuals')
text(0, -72, 'README visuals', 32, '#1f2328', 'Semi Bold')
text(0, -28, 'Generated by docs/assets/src/gen_assets.py (text outlined). Light and dark, shown with <picture> on GitHub.', 14)
imp(f'{A}/hero-light.svg', 0, 0)
imp(f'{A}/hero-dark.svg', 1340, 0)
imp(f'{A}/how-it-works-light.svg', 0, 400)
imp(f'{A}/how-it-works-dark.svg', 1340, 400)

# ------------------------------------------------------------------ dashboard
page('Dashboard')
text(0, -72, 'Dashboard (demo data)', 32, '#1f2328', 'Semi Bold')
text(0, -28, 'Screenshots of the web UI rendered against a mocked API. Light and dark follow prefers-color-scheme.', 14)
imp(f'{A}/dashboard-light.png', 0, 0)
imp(f'{A}/dashboard-dark.png', 1520, 0)

cmd('page.select', {'index': 0})
json.dump(steps, open(OUT, 'w'), indent=1)
print(len(steps), 'steps,', len(icons), 'icons')
