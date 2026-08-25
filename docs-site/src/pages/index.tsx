/*
 * Home Page
 *
 * The landing page: Charm-styled hero, the terminal screenshot, and the
 * feature tiles that route into the docs. Everything here is styled from the
 * Charm tokens in `src/css/custom.css` — there are no literal colours in this
 * file and no CSS modules, so the palette stays in one place.
 *
 * @joestump-agent 08/25/2026 - Initial MVP home page.
 */

import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import useDocusaurusContext from '@docusaurus/useDocusaurusContext';
import Layout from '@theme/Layout';
import type {ReactNode} from 'react';

import FeatureTiles from '@site/src/components/FeatureTiles';

function Hero(): ReactNode {
  return (
    <header className="charmHero">
      <div className="charmHero__inner">
        <span className="charmHero__eyebrow">
          A fork of Charm&apos;s Crush
        </span>
        <h1 className="charmHero__title">Crush</h1>
        <p className="charmHero__tagline">
          Your new coding bestie, now available in your favourite terminal.
        </p>
        <p className="charmHero__subtitle">
          Your tools, your code, and your workflows, wired into your LLM of
          choice.
        </p>
        <div className="charmHero__actions">
          <Link className="charmButton charmButton--primary" to="/getting-started/installation">
            Install Crush
          </Link>
          <Link className="charmButton charmButton--ghost" to="/getting-started/quickstart">
            Read the docs
          </Link>
        </div>
        <div className="charmHero__install">
          <span aria-hidden="true">$</span>
          <span>brew install charmbracelet/tap/crush</span>
        </div>
      </div>
      <div className="charmShot">
        <img
          src={useBaseUrl('/img/crush-screenshot.png')}
          alt="Crush running in a terminal: a tool-call transcript on the left, and a sidebar showing the session, model, modified files, LSPs, MCP servers, and skills on the right."
          width={1332}
          height={839}
        />
      </div>
    </header>
  );
}

function Features(): ReactNode {
  return (
    <section className="charmSection charmSection--sunken">
      <div className="charmSection__inner">
        <span className="charmSection__eyebrow">What&apos;s in the box</span>
        <h2 className="charmSection__title">Everything, wired in</h2>
        <p className="charmSection__lede">
          Crush is a terminal-first coding agent built on the Charm stack. Each
          tile below goes straight to the docs for that feature.
        </p>
        <FeatureTiles />
      </div>
    </section>
  );
}

function Fork(): ReactNode {
  return (
    <section className="charmSection">
      <div className="charmSection__inner">
        <span className="charmSection__eyebrow">About this build</span>
        <h2 className="charmSection__title">This is a fork</h2>
        <div className="charmSplit">
          <div>
            <p className="charmSection__lede">
              These docs cover the{' '}
              <a href="https://github.com/joestump-agent/crush">
                joestump-agent/crush
              </a>{' '}
              fork, which tracks{' '}
              <a href="https://github.com/charmbracelet/crush">
                charmbracelet/crush
              </a>{' '}
              upstream and adds a handful of features on top. Where a page
              documents something the fork adds, it says so.
            </p>
            <p className="charmSection__lede">
              Crush itself is a{' '}
              <a href="https://charm.land">Charm</a> project, licensed{' '}
              <a href="https://github.com/charmbracelet/crush/raw/main/LICENSE.md">
                FSL-1.1-MIT
              </a>
              . Bugs in upstream behaviour belong upstream; bugs in the additions
              below belong here.
            </p>
          </div>
          <div className="charmNote">
            <h3>Fork additions</h3>
            <ul>
              <li>
                <Link to="/features/sidekick">Sidekick</Link> — a second agent
                in the sidebar, plus a pushed A2UI dashboard
              </li>
              <li>
                <Link to="/features/channels">Channels</Link> — MCP servers that
                push events into your session
              </li>
              <li>
                <Link to="/features/a2ui">A2UI rendering</Link> via{' '}
                <a href="https://github.com/joestump-agent/a2tea">a2tea</a>
              </li>
              <li>
                <Link to="/features/scheduled-tasks">Scheduled tasks</Link> —
                cron-driven prompts
              </li>
              <li>
                <Link to="/features/semantic-search">Semantic search</Link> — a
                local vector index over the repo
              </li>
              <li>
                <Link to="/reference/tools#shell-and-job-control">Background jobs</Link>,
                click-to-copy, clipboard image paste, and more
              </li>
            </ul>
          </div>
        </div>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  const {siteConfig} = useDocusaurusContext();
  return (
    <Layout title={siteConfig.title} description={siteConfig.tagline}>
      <Hero />
      <main>
        <Features />
        <Fork />
      </main>
    </Layout>
  );
}
