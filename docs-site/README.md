# Crush docs site

The Docusaurus source for the Crush documentation site, published to GitHub
Pages for the [`joestump-agent/crush`](https://github.com/joestump-agent/crush)
fork at <https://joestump-agent.github.io/crush/>.

## Running it

```bash
cd docs-site
npm install
npm start          # dev server with hot reload
npm run build      # production build into ./build
npm run serve      # serve the production build
npm run typecheck  # tsc, no emit
```

From the repo root, the Taskfile wraps the same things:

```bash
task docs:dev
task docs:build
task docs:check
```

## Layout

```
docs-site/
├── docs/                    # the documentation, one .md per page
│   ├── introduction.md
│   ├── getting-started/
│   ├── configuration/
│   ├── features/
│   └── reference/
├── sidebars.ts              # hand-authored sidebar; add new pages here
├── docusaurus.config.ts     # site config, navbar, footer
├── src/
│   ├── pages/index.tsx      # the landing page
│   ├── components/          # FeatureTiles — the home page tiles
│   └── css/
│       ├── custom.css       # Infima -> Charm token bridge
│       └── tokens/          # CharmTone palette + semantic tokens
└── static/img/              # screenshot, favicon
```

## Adding a page

1. Write `docs/<section>/<page>.md` with `id`, `title`, `sidebar_position`, and
   `description` frontmatter.
2. Add its id to the right category in `sidebars.ts` — the sidebar is authored
   by hand so the reading order stays deliberate.
3. `npm run build`. The build is **strict**: `onBrokenLinks` and
   `onBrokenAnchors` are both `throw`, so a dead internal link or a link to a
   heading that does not exist fails the build rather than shipping.

Internal links are absolute, site-root-relative, and carry no `/docs` prefix —
`/features/mcp`, not `../features/mcp.md` — because docs are served from the
site root.

### Admonitions

Docusaurus 3 requires the **bracketed** title form. The MDX v1 bare-text form
parses as a plain paragraph and ships as literal `:::` text:

```markdown
:::info[Fork feature]                <!-- correct -->
:::info Fork feature                 <!-- silently renders as text -->
```

Use `:::info[Fork feature]` to mark anything the fork adds that upstream Crush
does not have.

## Styling

There is one rule: **no literal colours outside `src/css/tokens/`.** Everything
else references a token.

- `src/css/tokens/charmtone.css` — the raw CharmTone palette, copied verbatim
  from the Charm design system.
- `src/css/tokens/semantic.css` — role names (`--surface-base`, `--fg-muted`,
  `--intent-danger`, spacing, radius, type). Dark is the canonical Charm
  surface; light re-points the same names.
- `src/css/custom.css` — re-points Infima's `--ifm-*` variables at those
  tokens, so Docusaurus's own components inherit the palette without being
  restyled one by one.

Reach for a semantic token over a raw palette name. Charm's brand faces (PP
Mori, PP Monument) are commercially licensed and not redistributable, so
`--font-sans` and `--font-display` fall back to a system stack; JetBrains Mono
is open source and is served from Google Fonts.

## Deploying

`.github/workflows/docs.yml` builds on every PR touching `docs-site/**` and
deploys to GitHub Pages on push to `main`. The repository's Pages source must
be set to **GitHub Actions** (Settings → Pages → Build and deployment) for the
deploy job to succeed.

`url` and `baseUrl` are overridable via the `DOCS_URL` and `DOCS_BASE_URL`
environment variables, so the same build can ship to a different host without
editing the config.
