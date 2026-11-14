# Brand

Pitlane's identity is small on purpose: one mark, one accent color, the system
font everywhere in the product. The product's job is fast CI, and the brand
should look as quick and plain as a pit board.

## Logo

<img src="assets/logo.svg" width="48" height="48" alt="Pitlane logo">

The mark is a bold **P** split by two speed slits: a car leaving the pit box.
It sits in a signal-orange tile with 7/32 rounded corners, and is drawn on a
32 × 32 grid so it stays sharp as a 16 px favicon.

| File | Use |
| --- | --- |
| [`logo.svg`](logo.svg), [`assets/logo.svg`](assets/logo.svg) | Default: orange tile, white P. Works on light and dark backgrounds. |
| [`assets/logo-mono-black.svg`](assets/logo-mono-black.svg) | Single color, no tile, on light backgrounds. |
| [`assets/logo-mono-white.svg`](assets/logo-mono-white.svg) | Single color, no tile, on dark backgrounds or photos. |
| [`web/public/favicon.svg`](../web/public/favicon.svg) | Web UI favicon (same as `logo.svg`). |
| [`assets/hero-light.svg`](assets/hero-light.svg), [`assets/hero-dark.svg`](assets/hero-dark.svg) | README banner. |

- Keep clear space of at least a quarter of the tile around the mark.
- Minimum size: 16 px for the tile, 20 px for the single-color versions.
- Do not recolor the tile, add effects, outline the P or rotate the mark.
- The wordmark is **pitlane**, lowercase, set in an Inter-style grotesque at
  semibold weight with slight negative tracking (−0.03 em). In running text the
  name is written **Pitlane**.

## Colors

| Token | Hex | Use |
| --- | --- | --- |
| Signal orange | `#E5482D` | The logo tile. Large text and graphics only (3.96:1 on white). |
| Orange emphasis | `#CF3F22` | Primary buttons with white text (4.79:1), active tab underline in light mode. |
| Orange bright | `#F0684F` | Accents on dark backgrounds (6.13:1 on `#0d1117`). |

Everything else is neutral and functional, in the spirit of GitHub's own UI:

| Role | Light | Dark |
| --- | --- | --- |
| Canvas | `#ffffff` | `#0d1117` |
| Subtle canvas | `#f6f8fa` | `#151b23` |
| Border | `#d1d9e0` | `#3d444d` |
| Text | `#1f2328` | `#f0f6fc` |
| Muted text | `#59636e` | `#9198a1` |
| Link | `#0969da` | `#4493f8` |
| Success | `#1a7f37` | `#3fb950` |
| Failure | `#d1242f` | `#f85149` |
| In progress | `#bf8700` | `#d29922` |
| Warning | `#9a6700` | `#d29922` |
| Canceled / idle | `#818b98` | `#9198a1` |

Orange is the brand, never a status: success is green, failure is red, a job in
progress is amber, like a workflow run on GitHub. In charts, Pitlane is orange
and GitHub-hosted runners are gray; percentiles are blue (p50) and purple (p90).

## Typography

- **Product UI:** the system stack (`-apple-system, BlinkMacSystemFont, "Segoe UI",
  "Noto Sans", Helvetica, Arial, sans-serif`) at 14 px, monospace stack for
  runner names, labels and code. No web fonts: the UI is embedded in a binary.
- **Marketing visuals** (README banner, diagrams): text is outlined from
  [Adwaita Sans](https://gitlab.gnome.org/GNOME/adwaita-fonts) (SIL OFL 1.1,
  derived from Inter), so the SVGs render the same everywhere.

## Voice

- Lead with speed, and prove it: numbers come from real measurements, with the
  setup they were measured on. Never invent a metric, a user or a quote.
- Plain and concrete. Say what happens ("one job, then the runner is deleted"),
  not what it enables.
- Short sentences, active voice, no hype words (blazing, revolutionary, seamless).
- Honest about limits: Linux runners only, privileged slots, a young project.
- English everywhere: docs, UI text and code comments.
