"""Generate Pitlane README visuals (hero + how-it-works), light and dark.

usage: python3 docs/assets/src/gen_assets.py docs/assets   (needs fontTools)

Text is outlined with Adwaita Sans (SIL OFL 1.1, Inter-based) so the SVGs render
identically on GitHub, whatever fonts the viewer has.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from textpath import outline  # noqa: E402

OUT = sys.argv[1]

THEMES = {
    'light': dict(bg='#ffffff', card='#f6f8fa', border='#d1d9e0', fg='#1f2328', muted='#59636e',
                  accent='#E5482D', lane='#E5482D', soft='#fbe3dd', ok='#1a7f37', track='#d1d9e0'),
    'dark': dict(bg='#0d1117', card='#151b23', border='#3d444d', fg='#f0f6fc', muted='#9198a1',
                 accent='#E5482D', lane='#F0684F', soft='#3a1d17', ok='#3fb950', track='#3d444d'),
}

MARK = 'M9 6.5h9.5a7 7 0 0 1 0 14H15v5H9zM15 11.5v4h3.5a2 2 0 0 0 0-4z'
CUT = 'M4 10h11M4 14h7'


def text(s, x, y, size, weight=450, fill='#000', anchor='start', tracking=0.0):
    w, _ = outline(s, size, weight, tracking)
    if anchor == 'middle':
        x -= w / 2
    elif anchor == 'end':
        x -= w
    _, d = outline(s, size, weight, tracking, x0=x, baseline=y)
    return f'<path fill="{fill}" d="{d}"/>'


def mark(x, y, size):
    k = size / 32
    return (f'<g transform="translate({x} {y}) scale({k:.4f})"><mask id="pl-cut"><rect width="32" height="32" fill="#fff"/>'
            f'<path d="{CUT}" stroke="#000" stroke-width="2"/></mask><rect width="32" height="32" rx="7" fill="#E5482D"/>'
            f'<path fill="#fff" fill-rule="evenodd" mask="url(#pl-cut)" d="{MARK}"/></g>')


def svg(w, h, body, label):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}" role="img" '
            f'aria-label="{label}">{body}</svg>\n')


def hero(t):
    W, H = 1280, 320
    b = [f'<rect x="0.5" y="0.5" width="{W-1}" height="{H-1}" rx="16" fill="{t["card"]}" stroke="{t["border"]}"/>']
    b.append(mark(72, 64, 96))
    b.append(text('pitlane', 196, 140, 104, 640, t['fg'], tracking=-0.03))
    b.append(text('Ephemeral self-hosted GitHub Actions runners', 74, 222, 30, 500, t['fg'], tracking=-0.01))
    b.append(text('with caches that stay warm. One binary, a web UI, no Kubernetes.', 74, 262, 24, 420, t['muted']))

    # Right: the pit lane. The track runs along the top; the pit lane leaves it,
    # passes the pit boxes (slots) and rejoins it.
    x0, x1, ty = 860, 1216, 84
    b.append(f'<path d="M{x0-20} {ty}H{x1}" stroke="{t["track"]}" stroke-width="6" stroke-linecap="round"/>')
    b.append(f'<path d="M{x0+10} {ty}C{x0+50} {ty} {x0+50} 140 {x0+90} 140H{x1-80}C{x1-40} 140 {x1-40} {ty} {x1} {ty}" '
             f'fill="none" stroke="{t["lane"]}" stroke-width="5" stroke-linecap="round"/>')
    # chevrons on the track: traffic flowing
    for cx in (x0 + 110, x0 + 230):
        b.append(f'<path d="M{cx} {ty-9}l9 9-9 9" fill="none" stroke="{t["muted"]}" stroke-width="3" '
                 f'stroke-linecap="round" stroke-linejoin="round"/>')
    bx, bw, gap = 958, 52, 16
    for i in range(4):
        x = bx + i * (bw + gap)
        busy = i == 1
        b.append(f'<rect x="{x}" y="164" width="{bw}" height="72" rx="8" fill="{t["bg"]}" stroke="{t["lane"] if busy else t["border"]}" stroke-width="{2 if busy else 1.5}"/>')
        b.append(f'<circle cx="{x+bw/2}" cy="186" r="6" fill="{t["accent"]}"/>')
        b.append(text(f'#{i}', x + bw / 2, 224, 18, 600, t['muted'], 'middle'))
    b.append(text('warm slots', bx + 2 * (bw + gap) - gap / 2, 274, 20, 520, t['muted'], 'middle'))
    return svg(W, H, ''.join(b), 'Pitlane: ephemeral self-hosted GitHub Actions runners with caches that stay warm')


def box(t, x, y, w, h, stroke=None, fill=None, sw=1.5):
    return (f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{fill or t["bg"]}" '
            f'stroke="{stroke or t["border"]}" stroke-width="{sw}"/>')


def arrow(t, x1, y1, x2, y2, color, dashed=False, both=False):
    dash = ' stroke-dasharray="6 6"' if dashed else ''
    s = f'<path d="M{x1} {y1}L{x2} {y2}" stroke="{color}" stroke-width="2.5"{dash} fill="none"/>'
    import math
    a = math.atan2(y2 - y1, x2 - x1)

    def head(x, y, a):
        l, wd = 12, 6
        p1 = (x - l * math.cos(a) + wd * math.sin(a), y - l * math.sin(a) - wd * math.cos(a))
        p2 = (x - l * math.cos(a) - wd * math.sin(a), y - l * math.sin(a) + wd * math.cos(a))
        return f'<path d="M{x:.1f} {y:.1f}L{p1[0]:.1f} {p1[1]:.1f}L{p2[0]:.1f} {p2[1]:.1f}z" fill="{color}"/>'
    s += head(x2, y2, a)
    if both:
        s += head(x1, y1, a + math.pi)
    return s


def diagram(t):
    W, H = 1280, 560
    b = [f'<rect x="0.5" y="0.5" width="{W-1}" height="{H-1}" rx="16" fill="{t["card"]}" stroke="{t["border"]}"/>']

    # GitHub
    b.append(box(t, 40, 196, 250, 132))
    b.append(text('GitHub Actions', 64, 240, 22, 620, t['fg']))
    b.append(text('assigns jobs for', 64, 274, 18, 450, t['muted']))
    b.append(text('runs-on: pitlane', 64, 302, 18, 560, t['fg']))

    # long-poll arrow
    b.append(arrow(t, 296, 262, 452, 262, t['muted'], both=True))
    b.append(text('long-poll', 374, 240, 18, 560, t['fg'], 'middle'))
    b.append(text('scale set API', 374, 294, 17, 450, t['muted'], 'middle'))
    b.append(text('no webhook,', 374, 352, 17, 450, t['muted'], 'middle'))
    b.append(text('no public URL', 374, 376, 17, 450, t['muted'], 'middle'))

    # pitlane
    b.append(box(t, 460, 176, 260, 172, stroke=t['lane'], sw=2))
    b.append(mark(484, 200, 36))
    b.append(text('pitlane', 530, 228, 28, 640, t['fg'], tracking=-0.02))
    b.append(text('one Go binary', 484, 270, 18, 450, t['muted']))
    b.append(text('web UI, SQLite', 484, 298, 18, 450, t['muted']))
    b.append(text('picks a warm slot', 484, 326, 18, 450, t['muted']))

    # slots
    sx, sw_ = 790, 450
    ys = [36, 158, 280]
    for i, y in enumerate(ys):
        b.append(arrow(t, 726, 262, sx - 8, y + 50, t['lane']))
        b.append(box(t, sx, y, sw_, 100))
        b.append(text(f'slot #{i}', sx + 20, y + 32, 19, 620, t['fg']))
        b.append(text('Docker-in-Docker, warm cache', sx + 20 + 82, y + 32, 18, 450, t['muted']))
        # runner pill
        b.append(f'<rect x="{sx+20}" y="{y+50}" width="{sw_-40}" height="34" rx="17" fill="{t["soft"]}"/>')
        b.append(text('fresh runner: one job, then deleted', sx + 40, y + 74, 18, 520, t['fg']))
    # shared caches
    by = 420
    for x in (sx + 70, sx + 210, sx + 350):
        b.append(f'<path d="M{x} {ys[-1]+100}V{by}" stroke="{t["border"]}" stroke-width="2" stroke-dasharray="4 5"/>')
    b.append(box(t, sx, by, sw_, 100, fill=t['bg']))
    b.append(text('kept between jobs', sx + 20, by + 32, 19, 620, t['fg']))
    b.append(text('images and build cache (per slot)', sx + 20, by + 60, 18, 450, t['muted']))
    b.append(text('toolcache, package caches, Docker Hub mirror', sx + 20, by + 86, 18, 450, t['muted']))

    # legend under the left part
    b.append(text('After each job, containers, networks, volumes', 40, 470, 18, 450, t['muted']))
    b.append(text('and the workspace are wiped. Images and caches stay.', 40, 498, 18, 450, t['muted']))
    return svg(W, H, ''.join(b), 'How Pitlane works: GitHub long-poll, pitlane, warm Docker-in-Docker slots running one ephemeral runner per job, shared caches')


for name, t in THEMES.items():
    open(os.path.join(OUT, f'hero-{name}.svg'), 'w').write(hero(t))
    open(os.path.join(OUT, f'how-it-works-{name}.svg'), 'w').write(diagram(t))
print('ok')
