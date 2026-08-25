---
id: providers-and-api-keys
title: Providers and API keys
sidebar_position: 3
description: Every provider Crush knows about out of the box and the environment variable that authenticates it.
---

# Providers and API keys

The fastest way to authenticate is the model picker: press <kbd>ctrl+l</kbd>,
choose a provider, and paste a key. Crush stores it for you.

If you would rather not paste, set the provider's environment variable before
launching and Crush picks it up automatically.

## Environment variables

| Environment variable | Provider |
| --- | --- |
| `HYPER_API_KEY` | [Charm Hyper](https://hyper.charm.land) |
| `ANTHROPIC_API_KEY` | Anthropic |
| `OPENAI_API_KEY` | OpenAI |
| `VERCEL_API_KEY` | Vercel AI Gateway |
| `GEMINI_API_KEY` | Google Gemini |
| `ZAI_API_KEY` | Z.ai |
| `MINIMAX_API_KEY` | MiniMax |
| `SYNTHETIC_API_KEY` | Synthetic |
| `HF_TOKEN` | Hugging Face Inference |
| `CEREBRAS_API_KEY` | Cerebras |
| `OPENROUTER_API_KEY` | OpenRouter |
| `IONET_API_KEY` | io.net |
| `ALIBABA_SINGAPORE_API_KEY` | Alibaba (Singapore) |
| `ALIBABA_US_API_KEY` | Alibaba (United States) |
| `GROQ_API_KEY` | Groq |
| `AVIAN_API_KEY` | Avian |
| `OPENCODE_API_KEY` | OpenCode Zen & Go |
| `MOONSHOT_API_KEY` | Moonshot |
| `VERTEXAI_PROJECT`, `VERTEXAI_LOCATION` | Google Cloud Vertex AI (Gemini) |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` | Amazon Bedrock (Claude) |
| `AWS_PROFILE` | Amazon Bedrock (custom profile) |
| `AWS_BEARER_TOKEN_BEDROCK` | Amazon Bedrock (API key) |
| `AZURE_OPENAI_API_ENDPOINT` | Azure OpenAI |
| `AZURE_OPENAI_API_KEY` | Azure OpenAI (optional when using Entra ID) |
| `AZURE_OPENAI_API_VERSION` | Azure OpenAI |

Crush supports nearly any other provider too — see
[Custom providers](/configuration/providers) and
[Local models](/configuration/providers#local-models).

## Hyper

[Hyper](https://hyper.charm.land) is Charm's official Crush provider. It is
subscription-based with a free tier, optimised for Crush, privacy focused with
zero data retention, and designed to comply with GDPR.

```bash
crush login hyper     # opens the browser auth flow
crush logout hyper
```

See the [CLI reference](/reference/cli#login).

## Where the model list comes from

Crush's default provider and model listing is managed in
[Catwalk](https://github.com/charmbracelet/catwalk), a community-supported open
source repository of Crush-compatible models. If a provider you want is missing,
or a model's metadata is stale, contribute there.

Crush checks Catwalk for updates automatically. To turn that off — air-gapped
machines, restricted networks — see
[Provider auto-updates](/configuration/providers#provider-auto-updates).

List what your build currently knows about:

```bash
crush models
```

## Setting the model in config

```bash
# crushrc
model large anthropic/claude-sonnet-4-20250514 --think
model small anthropic/claude-3-5-haiku-20241022

# Print the current selection.
echo "coding with: $(model large)"
```

`model large` and `model small` also take sampling flags —
`--reasoning-effort`, `--max-tokens`, `--temperature`, `--top-p`, `--top-k`,
`--frequency-penalty`, `--presence-penalty`, and `--provider-options`. See the
[config command reference](/configuration/command-reference#model).

## Keeping keys out of your config

`crushrc` is Bash, so pull secrets from wherever they live rather than pasting
them:

```bash
provider add anthropic --api-key "$(op read 'op://Private/anthropic/key')"
provider add openai    --api-key "$(pass show openai/api-key)"
provider add groq      --api-key "${GROQ_API_KEY:?set GROQ_API_KEY}"
```

:::warning
`crushrc` and `crush.json` are **trusted code**. `crushrc` runs in a full shell
and any `$(...)` in `crush.json` runs at load time, before the UI appears. Don't
launch Crush in a directory whose config you haven't reviewed.
:::
