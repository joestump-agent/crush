---
id: semantic-search
title: Semantic search
sidebar_position: 9
description: A local vector index over your codebase, searched by meaning rather than by exact symbol.
---

# Semantic search

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

## Enabling it

Semantic search is **opt-in**: it is disabled unless an embedding provider is
configured. When it is configured, two tools appear in the agent's palette —
`semantic_search` and `semantic_index`.

### `crush.json`

Add a top-level `embeddings` block:

```json
{
  "embeddings": {
    "base_url": "https://api.openai.com/v1",
    "api_key": "${OPENAI_API_KEY:?set OPENAI_API_KEY}",
    "model": "text-embedding-3-small",
    "dimension": 768
  }
}
```

`api_key` and `base_url` support the same shell expansion as every other
credential field: `$VAR`, `${VAR}`, `${VAR:?message}`, and `$(command)`.

### `crushrc`

```bash
embeddings set --base-url https://api.openai.com/v1 \
               --api-key ${OPENAI_API_KEY:?set OPENAI_API_KEY} \
               --model text-embedding-3-small \
               --dimension 768

embeddings clear   # remove the provider and disable semantic search
```

### Fields and defaults

| Field | Default |
| --- | --- |
| `base_url` | `https://api.openai.com/v1` |
| `model` | `text-embedding-3-small` |
| `dimension` | `768` |
| `api_key` | — |

Any OpenAI-compatible embeddings endpoint works, including a local one.

**The dimension is baked into the table on first creation.** Changing it later
is detected and fails with an actionable error rather than silently mismatching
every insert — a dimension change means reindexing (drop the `chunks` and
`chunk_vectors` tables and run `semantic_index` again).

## The tools

| Tool | Purpose |
| --- | --- |
| `semantic_index` | Build or refresh the index. Unchanged files are skipped, so it is incremental and safe to re-run. |
| `semantic_search` | Query the index by natural-language description. |

**Indexing lifecycle:** the index is built on demand — the agent (or you, via
a prompt) runs `semantic_index` before searching, and re-runs it after large
changes. With no arguments it walks the working directory gitignore-aware and
skips files whose contents and embedding model have not changed since the last
run, so repeat runs only pay for what moved.

`semantic_search` takes:

| Parameter | Meaning |
| --- | --- |
| `query` | A natural-language description of the code or concept to find |
| `limit` | Maximum results (default 10) |

It returns file locations, symbol names, line ranges, and relevance scores —
not full file contents; follow up with `view` for the authoritative text.

In the interactive TUI, both tools render their results as an A2UI surface
under the tool call — a card with per-result location, score, and snippet
for `semantic_search`, and an index summary card for `semantic_index`. The
surface travels in tool-result metadata and never enters the model's
context; the model still receives a compact text digest it can act on.
Headless runs, channel-originated turns, and `disable_a2ui` deployments get
the original plain-text output unchanged.

## Checking state

`crush_info` reports the semantic index alongside LSP and MCP:

```
[semantic_index]
model = text-embedding-3-small (dim 768)
chunks = 1337
```

The section is absent when no embedding provider is configured.

Note that changing the embedding `dimension` against an existing index
disables the tools for that run with a logged, actionable error, since the
vector table cannot be resized in place.
