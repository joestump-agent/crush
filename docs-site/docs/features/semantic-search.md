---
id: semantic-search
title: Semantic search
sidebar_position: 9
description: A local vector index over your codebase, searched by meaning rather than by exact symbol.
---

# Semantic search

:::info[Fork feature — not yet wired up]
The storage and search layer has landed in the `joestump-agent/crush` fork, but
the `semantic_search` tool is **not currently registered** in the agent's tool
list and there is no `crushrc` or `crush.json` surface for the embedding
provider yet. This page documents what exists so far. Until it is wired up, use
`grep`, `glob`, and the [LSP tools](/features/lsp).
:::

## What it is

A vector index over the repository, stored in the project's own SQLite database
via the `vec0` virtual table from
[`modernc.org/sqlite/vec`](https://pkg.go.dev/modernc.org/sqlite) — the same
driver Crush already uses, so there is no extra native dependency and no extra
service to run.

It exists to answer the questions `grep` is bad at:

- *Where is authentication handled?*
- *What processes webhook events?*
- *Which code decides whether to retry?*

For an exact symbol name or a string literal, `grep` remains the right tool and
is much cheaper.

## How it works

1. Source files are chunked using the repo's symbol-extraction package, so a
   chunk is a function or type rather than an arbitrary window.
2. Each chunk is embedded through an **OpenAI-compatible** embeddings endpoint.
3. Vectors go into a `chunk_vectors` virtual table alongside the chunk text.
4. A query is embedded the same way and matched by K-nearest-neighbour search.

## Embedding configuration

The embedding client takes four values, with these defaults:

| Field | Default |
| --- | --- |
| `base_url` | `https://api.openai.com/v1` |
| `model` | `text-embedding-3-small` |
| `dimension` | `768` |
| `api_key` | — |

Any OpenAI-compatible embeddings endpoint works, including a local one.

**The dimension is baked into the table on first creation.** Changing it later
is detected and fails with an actionable error rather than silently mismatching
every insert — a dimension change means reindexing.

## The tool

When registered, `semantic_search` takes:

| Parameter | Meaning |
| --- | --- |
| `query` | A natural-language description of the code or concept to find |
| `limit` | Maximum results (default 10) |

It returns an explicit error rather than nothing when no embedding provider is
configured, or when the index is empty.

## Tracking this

Follow [`joestump-agent/crush`](https://github.com/joestump-agent/crush) for
when the tool is registered and the config surface lands.
