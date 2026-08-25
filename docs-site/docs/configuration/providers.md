---
id: providers
title: Providers and models
sidebar_position: 4
description: Custom OpenAI- and Anthropic-compatible providers, Bedrock, Vertex AI, local models, and the Catwalk catalog.
---

# Providers and models

Crush ships with a catalog of providers and models from
[Catwalk](https://github.com/charmbracelet/catwalk). This page covers adding
your own.

## Provider types

| Type | Use for |
| --- | --- |
| `openai` | Proxying or routing requests **through OpenAI** |
| `openai-compat` | Non-OpenAI providers with an OpenAI-compatible API |
| `anthropic` | Anthropic and Anthropic-compatible APIs |
| `google-vertex` | Google Cloud Vertex AI |
| `ollama`, `lmstudio`, `llamacpp`, `omlx`, `litellm` | [Local models](#local-models) — these auto-discover |

:::note
Pick the right OpenAI type. `openai` is for OpenAI itself (or a proxy in front
of it); `openai-compat` is for everything else that merely speaks the same
protocol. Choosing wrong degrades the experience.
:::

## OpenAI-compatible APIs

```bash
provider add deepseek --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "$DEEPSEEK_API_KEY"

model add deepseek/deepseek-chat \
  --name "Deepseek V3" \
  --context-window 64000 \
  --default-max-tokens 5000 \
  --price-input 0.27 \
  --price-output 1.1 \
  --price-cache-create 1.1 \
  --price-cache-hit 0.07
```

## Anthropic-compatible APIs

```bash
provider add custom-anthropic \
  --type anthropic \
  --base-url "https://api.anthropic.com/v1" \
  --api-key "$ANTHROPIC_API_KEY" \
  --extra-header anthropic-version 2023-06-01

model add custom-anthropic/claude-sonnet-4-20250514 \
  --name "Claude Sonnet 4" \
  --context-window 200000 \
  --default-max-tokens 50000 \
  --can-reason true \
  --supports-images true \
  --price-input 3 \
  --price-output 15 \
  --price-cache-create 3.75 \
  --price-cache-hit 0.3
```

## Amazon Bedrock

Crush currently supports running Anthropic models through Bedrock, with caching
disabled. A Bedrock provider appears as soon as Crush can find AWS credentials.

**API key.** Set `AWS_BEARER_TOKEN_BEDROCK` to a Bedrock API key. Simplest
option; never expires mid-session.

**AWS credential chain.** Configure AWS the usual way (`aws configure`,
`aws configure sso`). Crush picks up whatever the SDK resolves, including
`AWS_PROFILE`, static access keys, or an SSO session. Select a profile with
`AWS_PROFILE=myprofile crush`, or set it in the top-level
[`env`](/configuration/json#top-level-env) block.

SSO sessions expire. `aws_auth_refresh` gives Crush a command to run when
Bedrock returns a credential error — it refreshes, then retries the request in
place, with no duplicate messages and no manual restart:

```json
{
  "$schema": "https://charm.land/crush.json",
  "env": {
    "AWS_PROFILE": "my-sso-profile"
  },
  "providers": {
    "bedrock": {
      "aws_auth_refresh": "aws sso login --profile my-sso-profile"
    },
    "bedrock-europe": {
      "aws_auth_refresh": "aws sso login --profile my-eu-sso-profile"
    }
  }
}
```

## Vertex AI

Vertex AI appears in the provider list when `VERTEXAI_PROJECT` and
`VERTEXAI_LOCATION` are set. Authenticate with gcloud:

```bash
gcloud auth application-default login
```

Then register models:

```bash
# crushrc — authentication still comes from gcloud and the VERTEXAI_* env vars.
provider add vertexai --type google-vertex

model add vertexai/claude-sonnet-4@20250514 \
  --name "VertexAI Sonnet 4" \
  --context-window 200000 \
  --default-max-tokens 50000 \
  --can-reason true \
  --supports-images true \
  --price-input 3 \
  --price-output 15 \
  --price-cache-create 3.75 \
  --price-cache-hit 0.3
```

## Local models

Crush auto-discovers models from local providers. Add a provider whose `type` is
`llamacpp`, `omlx`, `lmstudio`, `litellm`, or `ollama` and leave the model list
out — Crush populates it.

```bash
# Piece of cake.
provider add ollama \
  --name Ollama \
  --type ollama \
  --base-url "http://localhost:11434/v1/"
```

For llama.cpp (`llama-server`), point at the server's base URL:

```bash
provider add llamacpp \
  --name "llama.cpp" \
  --type llamacpp \
  --base-url "http://localhost:2222"
```

### Manual model configuration

You can still list models explicitly. **User-defined models always take
precedence over discovered ones**, and any field you set is never overwritten by
auto-discovery.

Auto-discovery runs when the model list is empty for any `openai-compat`
provider, or whenever `--discover-models true` is set — in which case discovered
models are *merged* with your hand-configured ones and your explicit fields win
on conflicts.

```bash
provider add ollama \
  --name Ollama \
  --type ollama \
  --base-url "http://localhost:11434/v1/" \
  --discover-models true

model add ollama/qwen3:30b \
  --name "Qwen 3 30B" \
  --context-window 256000 \
  --default-max-tokens 20000
```

## System prompt prefixes

Some providers need a preamble prepended to the system prompt:

```bash
provider add my-proxy --type openai-compat \
  --system-prompt-prefix "You are operating behind an internal gateway."
```

## Provider auto-updates

By default Crush checks Catwalk for the latest provider and model list, so new
providers and metadata changes land automatically.

Disable it in your `crushrc`:

```bash
option provider-auto-update false
```

…or via the environment:

```bash
export CRUSH_DISABLE_PROVIDER_AUTO_UPDATE=1
```

### Updating manually

```bash
# Update providers remotely from Catwalk.
crush update-providers

# Update providers from a custom Catwalk base URL.
crush update-providers https://example.com/

# Update providers from a local file.
crush update-providers /path/to/local-providers.json

# Reset providers to the version embedded in this Crush build.
crush update-providers embedded

crush update-providers --help
```

## Disabling the built-in catalog

To run only providers you have declared yourself:

```bash
option default-providers false
```
