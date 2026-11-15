import { fileURLToPath } from 'node:url'
import path from 'node:path'
import type { Plugin } from 'vite'
import { defineConfig, type MarkdownRenderer } from 'vitepress'

// The Markdown in ../docs is the source of truth: it must read well on GitHub
// and on this site. Links that leave docs/ (../deploy/compose.yaml) or point at
// files VitePress does not publish (assets/src/gen_assets.py) are rewritten to
// GitHub, and raw-HTML images become relative imports so Vite bundles them.

const repo = 'https://github.com/TuroYT/pitlane'
const docsDir = fileURLToPath(new URL('../../docs', import.meta.url))

const pageExt = /\.(md|html)$/i
const isExternal = (href: string) => /^([a-z][a-z0-9+.-]*:|\/\/|#)/i.test(href)

function githubLinks(md: MarkdownRenderer) {
  const original = md.renderer.rules.link_open
  md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    const href = token.attrGet('href')
    if (href && !isExternal(href) && !href.startsWith('/')) {
      const [file, hash = ''] = href.split('#')
      const target = path.posix.normalize(path.posix.join(path.posix.dirname(env.relativePath), file))
      if (target.startsWith('../') || (file && !pageExt.test(file))) {
        const repoPath = path.posix.normalize(path.posix.join('docs', target))
        token.attrSet('href', `${repo}/${file.endsWith('/') ? 'tree' : 'blob'}/main/${repoPath}${hash ? `#${hash}` : ''}`)
        token.attrSet('target', '_blank')
        token.attrSet('rel', 'noreferrer')
      }
    }
    return original ? original(tokens, idx, options, env, self) : self.renderToken(tokens, idx, options)
  }

  // Inline code such as `${{ inputs.label }}` is GitHub Actions syntax, not a
  // Vue interpolation (fenced code blocks are already left alone).
  const codeInline = md.renderer.rules.code_inline!
  md.renderer.rules.code_inline = (tokens, idx, options, env, self) => {
    tokens[idx].attrSet('v-pre', '')
    return codeInline(tokens, idx, options, env, self)
  }

  // <img src="assets/logo.svg"> works on GitHub; Vue only bundles "./assets/…".
  const relSrc = /\b(src|srcset)="(?![a-z][a-z0-9+.-]*:|\/|\.|#)([^"]+)"/gi
  for (const rule of ['html_block', 'html_inline'] as const) {
    const render = md.renderer.rules[rule]!
    md.renderer.rules[rule] = (tokens, idx, options, env, self) => {
      tokens[idx].content = tokens[idx].content.replace(relSrc, '$1="./$2"')
      return render(tokens, idx, options, env, self)
    }
  }
}

// The compiled pages import `vue` and friends, but live outside this package:
// resolve their bare imports as if they came from site/, where node_modules is.
function resolveFromSite(): Plugin {
  const siteImporter = fileURLToPath(new URL('./config.mts', import.meta.url))
  return {
    name: 'pitlane:resolve-from-site',
    enforce: 'pre',
    resolveId(source, importer, options) {
      if (!importer?.startsWith(docsDir) || !/^[@a-z]/i.test(source)) return null
      return this.resolve(source, siteImporter, { ...options, skipSelf: true })
    },
  }
}

export default defineConfig({
  title: 'Pitlane',
  description:
    'Ephemeral self-hosted GitHub Actions runners with caches that stay warm. One binary, a web UI, no Kubernetes.',
  lang: 'en-US',
  base: '/pitlane/',
  srcDir: docsDir,
  // Design sources (FigCraft, generators) are not pages.
  srcExclude: ['assets/src/**'],
  cleanUrls: true,
  lastUpdated: true,
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/pitlane/logo.svg' }],
    ['meta', { name: 'theme-color', content: '#E5482D' }],
    ['meta', { property: 'og:title', content: 'Pitlane' }],
    ['meta', { property: 'og:description', content: 'Ephemeral self-hosted GitHub Actions runners with warm caches.' }],
  ],
  markdown: {
    config: githubLinks,
  },
  vite: {
    // logo.svg is served from site/public (a symlink to docs/logo.svg).
    publicDir: fileURLToPath(new URL('../public', import.meta.url)),
    server: { fs: { allow: [fileURLToPath(new URL('../..', import.meta.url))] } },
    plugins: [resolveFromSite()],
  },
  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'pitlane',
    nav: [
      { text: 'Guide', link: '/getting-started', activeMatch: '^/(getting-started|configuration|how-it-works|security|troubleshooting|faq|support|e2e)' },
      { text: 'Design notes', link: '/design/actions-cache', activeMatch: '^/design/' },
      { text: 'Brand', link: '/brand' },
      { text: 'Releases', link: `${repo}/releases` },
    ],
    sidebar: [
      {
        text: 'Introduction',
        items: [{ text: 'Why Pitlane', link: '/why' }],
      },
      {
        text: 'Guide',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'Configuration', link: '/configuration' },
          { text: 'How it works', link: '/how-it-works' },
          { text: 'Security', link: '/security' },
          { text: 'Troubleshooting', link: '/troubleshooting' },
          { text: 'FAQ', link: '/faq' },
          { text: 'Getting help', link: '/support' },
          { text: 'E2E tests', link: '/e2e' },
        ],
      },
      {
        text: 'Design notes',
        items: [{ text: 'Actions cache', link: '/design/actions-cache' }],
      },
      {
        text: 'Project',
        items: [{ text: 'Brand', link: '/brand' }],
      },
    ],
    socialLinks: [{ icon: 'github', link: repo }],
    search: { provider: 'local' },
    editLink: {
      pattern: `${repo}/edit/main/docs/:path`,
      text: 'Edit this page on GitHub',
    },
    outline: { level: [2, 3] },
    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Pitlane contributors',
    },
  },
})
