# Integrating autoclawpi with a CPA-style gateway

This document describes how `autoclawpi` fits into a **CPA** (CLI-Proxy-API) style
multi-channel LLM gateway. It covers the architecture only — no configuration from any
specific deployment is reproduced here, and no credentials are included.

---

## Why use a gateway in front of autoclawpi?

`autoclawpi` already provides an OpenAI-compatible endpoint and handles its own
multi-account round-robin. A gateway adds a layer **above** it:

```
┌──────────────┐
│  LLM client  │  (any OpenAI-compatible tool: IDE plugin, agent, CLI, SDK)
└──────┬───────┘
       │  one endpoint, one API key
       ▼
┌──────────────────────────────────────────┐
│  CPA gateway  (e.g. :8317)               │
│  ─ routes by model name / alias          │
│  ─ aggregates many upstream channels     │
│  ─ applies retry, logging, quotas        │
└──────┬───────────────────────────────────┘
       │  channel: "autoclaw"
       ▼
┌──────────────────────────────────────────┐
│  autoclawpi  (:8788)                     │
│  ─ account pool (round-robin)            │
│  ─ per-account egress proxy              │
│  ─ token refresh, cooldown, check-in     │
└──────┬───────────────────────────────────┘
       │  account N → its own egress
       ▼
   upstream inference API
```

**Division of responsibility:**

| Layer | Owns |
|---|---|
| Client | Model choice, prompts |
| **CPA gateway** | Model-name routing, multi-channel aggregation, retries, usage stats |
| **autoclawpi** | Account pool, per-account egress isolation, token lifecycle, check-in |

Each layer stays unaware of the other's internals: the gateway sees a normal
OpenAI-compatible server; `autoclawpi` sees ordinary API clients.

---

## Channel definition (shape, not values)

A channel is registered as an OpenAI-compatible provider. The structure is:

```yaml
openai-compatibility:
  - name: autoclaw                    # channel identifier
    base-url: http://127.0.0.1:8788/v1 # autoclawpi's local endpoint
    priority: 20                       # ordering vs other channels
    api-key-entries:
      - api-key: <the key passed to autoclawpi's --api-key>
    models:
      - name: glm-5.3                  # name autoclawpi accepts
        alias: autoclaw-glm-5.3        # name clients will call
```

### Field notes

| Field | Meaning |
|---|---|
| `name` | Channel label. Appears in gateway logs and model alias prefixes. |
| `base-url` | Must end in `/v1` — autoclawpi exposes `/v1/chat/completions` and `/v1/models`. |
| `api-key-entries` | The key autoclawpi expects in `Authorization: Bearer …`. This is **autoclawpi's own key**, not an upstream credential. |
| `models[].name` | Must exactly match a name autoclawpi accepts (see below). |
| `models[].alias` | The public name clients use. The `autoclaw-` prefix keeps it unambiguous when many channels are aggregated. |

### Model naming

Two independent name layers exist, and mixing them up is the most common
integration error:

```
client calls          gateway routes to          autoclawpi maps to
─────────────         ──────────────────         ──────────────────
autoclaw-glm-5.3  →   channel "autoclaw"     →   glm-5.3
   (alias)              + name "glm-5.3"           (accepted name)
```

- The **alias** is gateway-local. Rename it freely.
- The **accepted name** is fixed by autoclawpi. Sending anything else returns
  `400 {"message":"非法模型"}` from upstream.

Current accepted names (verify against a running instance, since upstream
availability changes):

```
GET http://127.0.0.1:8788/v1/models
Authorization: Bearer <api-key>
```

> **Note:** `glm-5.2` is **not** available on this channel. AutoClaw's upstream
> rejects it as an unknown model. If a client needs `glm-5.2`, route it to a
> different channel that actually serves it.

---

## What autoclawpi handles underneath the channel

Once traffic arrives, `autoclawpi` decides which account serves it:

```
request → pick next usable account (round-robin)
            ├─ skip accounts in cooldown
            ├─ send via that account's OWN egress proxy
            │     (each account is pinned to a distinct outbound IP)
            ├─ on 401  → refresh token, retry once
            ├─ on network error → short cooldown, try next account
            └─ on empty completion (reasoning exhausted the budget)
                  → raise max_tokens, retry once
```

Two properties matter to the gateway:

1. **Failover is internal.** If one account fails, autoclawpi tries the next
   before returning an error. The gateway sees one logical channel, not N accounts.
2. **Egress is per-account.** Accounts are pinned to separate outbound IPs, so a
   single channel can fan out across many network identities without the gateway
   knowing anything about it.

### Error surfacing

autoclawpi translates some upstream errors so the gateway classifies them correctly:

| Upstream | autoclawpi returns | Why |
|---|---|---|
| `403` + code `810002` (high demand) | `429` | A `403` makes many gateways mark the whole channel as payment-required and suspend it. `429` is treated as transient rate limiting. |
| Account banned (`410004`) | `403` + reason | Account is cooled down and skipped; the channel stays up. |

---

## Pitfalls

1. **Don't point the gateway at a shared/global proxy port.** Each account must use
   its own egress; sharing one exit IP across accounts is what triggers upstream
   abuse detection.

2. **Don't reuse the upstream credential as the channel key.** The gateway's
   `api-key` is autoclawpi's own local key. Upstream tokens live inside
   autoclawpi's account store and never leave it.

3. **Restart ordering.** Start `autoclawpi` before (or alongside) the gateway. A
   gateway that starts first will log connection errors until the channel comes up.

4. **Model list drift.** Upstream model availability changes. Re-check
   `/v1/models` before adding aliases; a stale alias produces a confusing
   `非法模型` error at request time rather than at startup.

5. **Don't double-retry.** If both the gateway and autoclawpi retry aggressively,
   a single client request can multiply into many upstream calls. Keep retries at
   one layer.

---

## Minimal end-to-end check

With both services running:

```bash
# 1. autoclawpi is up and has usable accounts
curl -s -H "Authorization: Bearer <api-key>" http://127.0.0.1:8788/healthz

# 2. the channel serves the expected model list
curl -s -H "Authorization: Bearer <api-key>" http://127.0.0.1:8788/v1/models

# 3. the gateway routes the alias through to a real completion
curl -s -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <gateway-key>" \
  -d '{"model":"autoclaw-glm-5.3","messages":[{"role":"user","content":"hi"}]}'
```

If step 3 returns a completion, the chain client → gateway → autoclawpi →
upstream is wired correctly.
