/*
 * Feature Tiles
 *
 * The hero tiles on the home page. Each tile names one major Crush capability
 * and links straight at the usage doc for it — the tile is the entry point to
 * the docs, so every one of these hrefs must resolve to a real page.
 *
 * @joestump-agent 08/25/2026 - Initial set for the MVP docs site.
 */

import Link from '@docusaurus/Link';
import type {ReactNode} from 'react';

type Tile = {
  icon: string;
  title: string;
  body: ReactNode;
  href: string;
  more: string;
};

const TILES: Tile[] = [
  {
    icon: '◆',
    title: 'Any model, mid-session',
    body: (
      <>
        Dozens of providers out of the box, plus anything speaking an OpenAI- or
        Anthropic-compatible API. Switch LLMs mid-session and keep your context.
      </>
    ),
    href: '/configuration/providers',
    more: 'Providers & models',
  },
  {
    icon: '⌘',
    title: 'Bash-native config',
    body: (
      <>
        A <code>crushrc</code> is just Bash with Crush builtins — conditionals,
        includes, and secrets from your password manager. Identical on Windows.
      </>
    ),
    href: '/configuration/crushrc',
    more: 'Configuring Crush',
  },
  {
    icon: '⚙',
    title: 'MCP servers',
    body: (
      <>
        Extend the agent over <code>stdio</code>, <code>http</code>, and{' '}
        <code>sse</code>, with OAuth 2.1, per-server tool filtering, and MCP
        prompts and resources in the palette.
      </>
    ),
    href: '/features/mcp',
    more: 'MCP',
  },
  {
    icon: '⇄',
    title: 'Channels',
    body: (
      <>
        MCP servers push real-time events straight into your session — CI
        failures, webhooks, Signal messages — and Crush acts on them without you
        typing a thing.
      </>
    ),
    href: '/features/channels',
    more: 'Channels',
  },
  {
    icon: '▣',
    title: 'A2UI surfaces',
    body: (
      <>
        Models speak the A2UI protocol and Crush draws it:
        cards, lists, buttons and dashboards rendered inline in the chat instead
        of raw JSON.
      </>
    ),
    href: '/features/a2ui',
    more: 'A2UI',
  },
  {
    icon: '◇',
    title: 'LSP-enhanced',
    body: (
      <>
        Crush reads your code through language servers, just like you do —
        diagnostics, definitions, references, call hierarchies, and safe renames.
      </>
    ),
    href: '/features/lsp',
    more: 'Language servers',
  },
  {
    icon: '⚑',
    title: 'Skills',
    body: (
      <>
        The Agent Skills open standard, with four skills built in and discovery
        from every convention on disk.
      </>
    ),
    href: '/features/skills',
    more: 'Skills',
  },
  {
    icon: '⏱',
    title: 'Scheduled tasks',
    body: (
      <>
        Give the agent a cron expression and it re-runs a prompt on its own —
        one-shot reminders or recurring jobs that survive a restart.
      </>
    ),
    href: '/features/scheduled-tasks',
    more: 'Scheduled tasks',
  },
  {
    icon: '⛨',
    title: 'Hooks & permissions',
    body: (
      <>
        Deterministic control over tool calls: block, rewrite, auto-approve, or
        inject context before anything runs.
      </>
    ),
    href: '/features/hooks',
    more: 'Hooks',
  },
  {
    icon: '⌕',
    title: 'Semantic search',
    body: (
      <>
        A local vector index over your codebase, so the agent can find code by
        what it does rather than by what it is called.
      </>
    ),
    href: '/features/semantic-search',
    more: 'Semantic search',
  },
  {
    icon: '⧉',
    title: 'Shared workspaces',
    body: (
      <>
        Run <code>crush server</code> and point several clients at the same
        working directory — one session list, history, and permission queue.
      </>
    ),
    href: '/features/server-and-workspaces',
    more: 'Server & workspaces',
  },
];

export default function FeatureTiles(): ReactNode {
  return (
    <div className="charmTiles">
      {TILES.map((tile) => (
        <Link key={tile.title} className="charmTile" to={tile.href}>
          <span className="charmTile__icon" aria-hidden="true">
            {tile.icon}
          </span>
          <h3 className="charmTile__title">{tile.title}</h3>
          <p className="charmTile__body">{tile.body}</p>
          <span className="charmTile__more">{tile.more} →</span>
        </Link>
      ))}
    </div>
  );
}
