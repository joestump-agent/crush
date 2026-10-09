---
id: team
title: Working with other agents
sidebar_label: Team
description: What a team can do with A2A today — consume any third-party agent as a dispatch agent, expose your Crush over TLS, share definition cards — with a certificate recipe, the option lines, and the failures you will meet.
---

# Working with other agents

:::info[Fork feature]
The A2A host, its TCP listener and external agents are additions in the
`joestump-agent/crush` fork.
:::

Every Crush is an [A2A](https://a2a-protocol.org) host and an A2A client.
This page says what that buys a team today, how to set up the certificates
the TCP listener needs, how to read a teammate's agent cards, and what the
errors mean. The wire details are on [A2A protocol](/agents/a2a-protocol).

## What works today

| You want to | Works? | How |
| --- | --- | --- |
| Dispatch work to a third-party A2A agent — a reviewer service your team runs, a vendor's agent, anything that serves an Agent Card | **Yes** | Define it as a `runtime: a2a` dispatch agent; see [External agents](/agents/configuration#external-agents). The main agent dispatches to it like any other, its findings come back labeled untrusted, and it can be canceled but not steered |
| Expose your Crush on the network with TLS and authenticate callers by client certificate | **Yes** | The [TCP listener](#the-tcp-listener) below. An authenticated peer reaches every agent's route |
| Let a teammate fetch your agents' cards | **Not yet** | The host builds one card per agent definition, but its router answers `404` for the well-known card path, so nothing serves them over the wire; see [below](#what-a-teammate-can-reach) |
| Let a teammate's Crush run a task on your Crush | **Not yet** | A TCP caller is authenticated but reaches no run: every route answers `no running agent for context …`. Cross-Crush delegation arrives with the peer registry ([#334](https://github.com/joestump-agent/crush/issues/334)) |
| Point your `--card` at a teammate's Crush | **Not yet** | The card fetch gets `404`, so the dispatch fails with `resolve agent card <url>: HTTP status 404`. Point `--card` at a non-Crush A2A agent instead |

So the one cross-machine workflow that runs end to end today is **consume a
non-Crush A2A agent**. Exposing your own host is groundwork: it lets you check
the certificates, the Host check and the routes before #334 lands, since an
authenticated call gets the same answer it will get for a live route that is
not yours.

## The TCP listener

By default the host serves only a `0600` unix socket under your data
directory, reachable by your own user alone. The TCP listener is opt-in, TLS
only, and authenticates every request before it reads a body: a client
certificate verified against `a2a-client-ca`, or your process's own bearer
token (which never leaves the process, so a remote caller needs a
certificate).

```bash
# crushrc — usually the global one, ~/.config/crush/crushrc
option a2a-listen 0.0.0.0:7443
option a2a-tls-cert ~/.config/crush/certs/a2a.pem
option a2a-tls-key ~/.config/crush/certs/a2a-key.pem
option a2a-client-ca ~/.config/crush/certs/ca.pem
```

```json
{
  "options": {
    "a2a": {
      "listen": "0.0.0.0:7443",
      "tls_cert": "~/.config/crush/certs/a2a.pem",
      "tls_key": "~/.config/crush/certs/a2a-key.pem",
      "client_ca": "~/.config/crush/certs/ca.pem"
    }
  }
}
```

The paths must be absolute or start with `~/`; a relative path fails the load.
`a2a-listen` without both `a2a-tls-cert` and `a2a-tls-key` fails the load
too: plain TCP is never served. The certificate and key must load as a pair,
and the CA file must hold at least one PEM certificate, or the load fails and
names the key. The listener starts with the host — when the TUI starts, not
on the first dispatch — and a change to any of these takes a restart.

### Certificates

Crush does not provision certificates. Any PEM certificate works, ACME
included; what matters is that the server certificate carries the DNS or IP
names peers will dial, and that client certificates chain to the file you name
as `a2a-client-ca`. A private CA for the team is the simplest way to get both.
The recipe below uses `openssl` and produces files Crush loads as written:

```bash
mkdir -p ~/.config/crush/certs && cd ~/.config/crush/certs

# 1. One CA for the team. Keep ca-key.pem offline; every teammate gets ca.pem.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout ca-key.pem -out ca.pem -days 3650 -subj "/CN=Team Crush CA"

# 2. A server certificate for this machine. The SANs are the names and
#    addresses teammates will dial; the Host check accepts exactly these.
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout a2a-key.pem -out a2a.csr -subj "/CN=alice.example.internal"
openssl x509 -req -in a2a.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out a2a.pem -days 825 \
  -extfile <(printf 'subjectAltName=DNS:alice.example.internal,IP:10.0.0.12\nextendedKeyUsage=serverAuth')

# 3. One client certificate per teammate (or per calling agent).
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout bob-key.pem -out bob.csr -subj "/CN=bob"
openssl x509 -req -in bob.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out bob.pem -days 825 -extfile <(printf 'extendedKeyUsage=clientAuth')
```

Then on your side, `a2a-tls-cert` is `a2a.pem`, `a2a-tls-key` is
`a2a-key.pem`, and `a2a-client-ca` is `ca.pem`. Bob keeps `bob.pem`,
`bob-key.pem` and `ca.pem`.

Two things the host derives from the server certificate:

- **The advertised address.** With a specific listen host
  (`alice.example.internal:7443`), the cards advertise that. With a wildcard
  (`0.0.0.0:7443`, `:7443`), they advertise the certificate's first
  non-wildcard DNS SAN, else its first IP SAN, with the bound port. With
  neither, the cards list no HTTPS interface and the log warns.
- **The `Host` check.** A request's `Host` must be the listen address, the
  bound address, the advertised address, or a name that verifies against the
  certificate's SANs — at any port. Anything else is a `400`, which is what a
  DNS-rebinding attempt gets.

### Startup warnings

| Log line | Meaning | Change |
| --- | --- | --- |
| `A2A TCP listener has no address to advertise; cards list no HTTPS interface` | A wildcard listen address and a certificate with no DNS or IP SAN | Listen on a specific host, or add a SAN |
| `A2A TCP listener accepts only this process's bearer token, which never leaves the process; remote clients cannot authenticate without options.a2a.client_ca` | The listener is reachable beyond loopback but has no `a2a-client-ca`, so nobody remote can get past `401` | Set `option a2a-client-ca` |
| `A2A TCP listener not started; serving the unix socket only` | The port is taken or the TLS files did not load | Read the `error`; local dispatch still works |
| `A2A host listening on TCP` with `addr`, `url`, `mutual_tls` | Success | — |

## What a teammate can reach

The host builds one Agent Card per enabled agent definition — `coder`,
`plan`, `task`, `worker`, and any agent the teammate added — and one per
running dispatch, and gives each a route at `/agents/<id>`. Each route mounts
the card's well-known handler, but the host's router answers `404` for every
sub-path of a route, so `/agents/<id>/.well-known/agent-card.json` is not
reachable over the wire today, on TCP or on the socket. The cards exist in
the host's listing for the peer registry to publish; until then there is no
way to read a teammate's cards, and a `--card` pointed at a Crush fails with
`HTTP status 404`.

What an authenticated teammate can do is send A2A JSON-RPC to a definition
route. Every TCP request must carry a verified client certificate (or the
host's own token) and a JSON content type:

```bash
curl --cacert ca.pem --cert bob.pem --key bob-key.pem \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","contextId":"probe","parts":[{"text":"hello"}]}}}' \
  https://alice.example.internal:7443/agents/worker
```

The answer is today's boundary — a task that is immediately rejected:

```json
{"jsonrpc":"2.0","id":"1","result":{"task":{"id":"…","contextId":"probe",
  "status":{"state":"TASK_STATE_REJECTED","message":{"role":"ROLE_AGENT",
    "parts":[{"text":"no running agent for context probe; task sessions are not continuable"}]}}}}}
```

That rejection is the success case for the groundwork: it proves the
certificates, the `Host` check and the route all line up, and it is the same
answer an unknown context gets. Even a valid credential cannot message, steer
or cancel a run on someone else's host. `GET /agents`, the live index of
running dispatches, is served on the socket only.

| Request over TCP | Answer |
| --- | --- |
| Any, without a client certificate (when `a2a-client-ca` is set) | The TLS handshake fails: `tls: client didn't provide a certificate` in the host's log |
| Any, without `Content-Type: application/json` | `415` |
| Any, with a `Host` that is neither the listen address nor a certificate SAN | `400` |
| `GET` or `POST /agents/<definition id>/.well-known/agent-card.json` | `404` |
| `POST /agents/<definition id>` with `SendMessage` | A `TASK_STATE_REJECTED` task: `no running agent for context …` |
| `GET /agents` | `404` |
| An unknown id | `404` |

When the cards become fetchable, each definition card carries the
definition's `name` and `description`, two JSON-RPC interfaces (the
`crush-a2a` socket label first, the HTTPS address second), the eight Crush
extensions, the `crush-bearer` and `crush-mtls` security schemes and an empty
`skills` list; the fields are on [A2A protocol](/agents/a2a-protocol#agent-card).

## Consuming a teammate's non-Crush agent

When the agent at the other end is not a Crush — a reviewer bot, a hosted
model agent, anything that serves a card — define it as an external dispatch
agent and the main agent can hand it work:

```bash
agent add reviewer --role dispatch --runtime a2a \
  --card https://reviewer.example.net/.well-known/agent-card.json \
  --bearer '$REVIEWER_TOKEN' --idle-timeout 2m --timeout 30m
```

Then "dispatch the reviewer to look over `pkg/x`" calls
`dispatch_agent {"agent": "reviewer", …}`. What the remote needs:

- a card at the URL, served **without** a credential (the token is sent only
  to the JSON-RPC calls, and only to the card's own origin);
- a `JSONRPC` interface on that same origin, protocol version `1.x`;
- when you pass `--bearer`, an HTTP `bearer` security scheme on the card;
- a certificate your system trusts. Crush uses the system trust store for
  external cards, so a team CA has to be installed there (`security
  add-trusted-cert` on macOS, `update-ca-certificates` on Debian-family
  Linux); otherwise the dispatch fails with
  `TLS certificate verification failed`.

The run is bounded by `--idle-timeout` (5 minutes if unset, and
`options.todo_enforcement.inactivity_timeout` before that) and
`--timeout`. Its findings arrive labeled `UNTRUSTED:`, it cannot ask you
questions, it cannot be steered, and `apply_dispatch`/`dismiss_dispatch` refuse
it because it wrote nothing to your disk. The full trust model and every
failure string are under
[External agents](/agents/configuration#external-agents).

## Security summary

- **Unix socket.** `0600` in a `0700` directory under your data directory;
  only your OS user can connect, and every call still carries the per-process
  bearer token. Other users on the machine cannot reach your dispatches.
- **TCP.** TLS 1.2 or newer, always. A caller is never treated as the local
  user: it needs a verified client certificate or the bearer token, checked
  before the body is read. Identity is `ca-sha256:<CA fingerprint>/<subject>`,
  so two CAs issuing the same subject do not share tasks. Bodies are capped at
  32 MiB, the handshake and headers at 30 seconds, idle connections at two
  minutes. Rejections are logged with the peer's address, never the
  credential.
- **Browsers.** Cross-origin requests are `403`, non-JSON bodies `415`, and a
  wrong `Host` `400`, on both listeners.
- **Local runs stay local.** No TCP caller can run, message, steer or cancel
  a run on your host until #334.
- **External agents.** Your token goes to one origin and is scrubbed from
  what comes back; the output is untrusted text with control characters
  removed; input and permission requests from the remote are refused.

Troubleshooting entries for every error on this page are on
[Multi-agent troubleshooting](/agents/troubleshooting#the-tcp-listener).
