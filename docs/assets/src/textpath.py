"""Outline text with Adwaita Sans (OFL, Inter-based) into SVG path data.

usage: textpath.py TEXT SIZE WEIGHT [TRACKING_EM] [OPSZ]
prints: width d
"""
import functools
import sys

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont

SRC = '/usr/share/fonts/Adwaita/AdwaitaSans-Regular.ttf'


@functools.lru_cache(None)
def font(weight, opsz):
    return instantiateVariableFont(TTFont(SRC), {'wght': weight, 'opsz': opsz})


def outline(text, size, weight=600, tracking=0.0, opsz=32, x0=0.0, baseline=0.0):
    f = font(weight, opsz)
    upm = f['head'].unitsPerEm
    s = size / upm
    gs = f.getGlyphSet()
    cmap = f.getBestCmap()
    hmtx = f['hmtx']
    pen = SVGPathPen(gs, ntos=lambda v: ('%.1f' % v).rstrip('0').rstrip('.'))
    x = x0
    for i, ch in enumerate(text):
        g = cmap[ord(ch)]
        tp = TransformPen(pen, (s, 0, 0, -s, x, baseline))
        gs[g].draw(tp)
        x += hmtx[g][0] * s
        if i < len(text) - 1:
            x += tracking * size
    return x - x0, pen.getCommands()


if __name__ == '__main__':
    text, size, weight = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
    tr = float(sys.argv[4]) if len(sys.argv) > 4 else 0.0
    opsz = float(sys.argv[5]) if len(sys.argv) > 5 else 32
    w, d = outline(text, size, weight, tr, opsz)
    print('%.2f' % w)
    print(d)
