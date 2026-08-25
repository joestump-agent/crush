/*
 * Docusaurus Configuration
 *
 * Builds the Crush documentation site for GitHub Pages on the
 * joestump-agent/crush fork. Docs are served from the site root
 * (`routeBasePath: '/'`) with a hand-written React landing page at `/`, so
 * every docs URL is `/<section>/<page>` with no `/docs` prefix.
 *
 * `url` and `baseUrl` are overridable from the environment so the same build
 * can ship to a different host without editing this file; the defaults are the
 * fork's GitHub Pages address.
 *
 * @joestump-agent 08/25/2026 - Initial config for the MVP docs site.
 */

import type * as Preset from '@docusaurus/preset-classic';
import type {Config} from '@docusaurus/types';
import {themes as prismThemes} from 'prism-react-renderer';

const ORG = 'joestump-agent';
const REPO = 'crush';
const GITHUB_URL = `https://github.com/${ORG}/${REPO}`;
const UPSTREAM_URL = 'https://github.com/charmbracelet/crush';

// `||` rather than `??`: CI exports these as empty strings when unset, and an
// empty `url` fails the build.
const SITE_URL = process.env.DOCS_URL || `https://${ORG}.github.io`;
const BASE_URL = process.env.DOCS_BASE_URL || `/${REPO}/`;

const config: Config = {
  title: 'Crush',
  tagline:
    'Your new coding bestie, now available in your favourite terminal. Your tools, your code, and your workflows, wired into your LLM of choice.',
  favicon: 'img/favicon.svg',

  url: SITE_URL,
  baseUrl: BASE_URL,
  organizationName: ORG,
  projectName: REPO,
  trailingSlash: false,

  future: {
    v4: true,
    faster: true,
  },

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  markdown: {
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: 'docs',
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: `${GITHUB_URL}/edit/main/docs-site/`,
          showLastUpdateTime: true,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
        sitemap: {
          lastmod: 'date',
          changefreq: 'weekly',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/crush-screenshot.png',
    colorMode: {
      defaultMode: 'dark',
      respectPrefersColorScheme: true,
    },
    navbar: {
      // Wordmark only — no logo. `.navbar__title` styles it as uppercase mono,
      // which is the mark. The favicon is still the browser-tab icon.
      title: 'Crush',
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docsSidebar',
          position: 'left',
          label: 'Docs',
        },
        {to: '/configuration/crushrc', label: 'Configuration', position: 'left'},
        {to: '/reference/cli', label: 'Reference', position: 'left'},
        {href: GITHUB_URL, label: 'GitHub', position: 'right'},
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Installation', to: '/getting-started/installation'},
            {label: 'Quickstart', to: '/getting-started/quickstart'},
            {label: 'Configuration', to: '/configuration/crushrc'},
            {label: 'CLI reference', to: '/reference/cli'},
          ],
        },
        {
          title: 'Features',
          items: [
            {label: 'MCP', to: '/features/mcp'},
            {label: 'Channels', to: '/features/channels'},
            {label: 'A2UI', to: '/features/a2ui'},
            {label: 'Skills', to: '/features/skills'},
          ],
        },
        {
          title: 'Project',
          items: [
            {label: 'This fork', href: GITHUB_URL},
            {label: 'Upstream Crush', href: UPSTREAM_URL},
            {label: 'Catwalk', href: 'https://github.com/charmbracelet/catwalk'},
            {label: 'Charm', href: 'https://charm.land'},
          ],
        },
      ],
      copyright: `Crush is a Charm project, licensed FSL-1.1-MIT. Docs for the ${ORG}/${REPO} fork, built with Docusaurus.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'go', 'json', 'lua', 'nix', 'powershell'],
    },
    tableOfContents: {
      minHeadingLevel: 2,
      maxHeadingLevel: 4,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
