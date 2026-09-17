# Egress IP pool — design notes

Why and how `autoclawpi` gives each account its own outbound IP. This document describes
the **design**; it contains no node addresses, subscription details, or deployment values.

---

## The problem

The proxy rotates across many accounts to spread load. Early on, all accounts shared one
outbound path. That turned out to be the single biggest cause of account loss:

```
account A ─┐
account B ─┼─→ one shared exit IP ─→ upstream
account C ─┘
```

From the upstream's point of view this is **one network identity driving N accounts** —
exactly the pattern abuse detection looks for. Two accounts were lost this way, and the
only thing they had in common was the exit IP they shared.

**Conclusion: the unit of isolation is the outbound IP, not the account.**

---

## Design goals

1. **One account → one distinct outbound IP.** No two accounts may share an exit.
2. **The mapping must be stable.** An account's exit must not silently change, because
   the IP is part of what the upstream has already seen.
3. **Failure must not take the service down.** A dead node degrades throughput, not
   availability.
4. **No interference with the operator's own network setup.** The pool must be fully
   separate from any personal proxy/VPN client.
5. **Additive capacity.** Adding capacity must not disturb existing accounts.

---

## Architecture

An **independent proxy instance** is run alongside the service, with its own working
directory, control port, and listen ports:

```
                        ┌──────────────────────────────────────┐
                        │  independent proxy instance          │
   account #1 ─────────▶│  :7901 ─→ url-test group ─→ node ──┐  │
   account #2 ─────────▶│  :7902 ─→ url-test group ─→ node ──┤  │
   account #3 ─────────▶│  :7903 ─→ url-test group ─→ node ──┤  │
        ...             │   ...                              │  │
                        └────────────────────────────────────┼──┘
                                                             ▼
                                                     distinct exit IPs
```

Key properties:

| Property | Value | Why |
|---|---|---|
| Separate process/workdir | Yes | Isolates from any personal proxy client |
| Separate control port | Yes | No shared management surface |
| One listen port per account | `base + slot` | Simple, auditable mapping |
| Bind address | loopback only | The pool must never be reachable from the LAN |
| LAN access | disabled | Defence in depth, even if bind were misconfigured |
| Global mixed port | unused | Nothing should route through the pool by accident |

### Why a port per account

The alternative — one port plus routing rules — makes the account↔exit mapping implicit
and easy to get wrong. A dedicated port makes it explicit and inspectable: **slot N is
always `base + N`**, and the account record stores the URL it uses. You can verify the
mapping by reading two files.

---

## Slot assignment

Accounts and pool entries are joined by **ordinal**, not by address:

```
account.slot = N   →   ip-pool.json.proxies[N-1]   →   http://127.0.0.1:(base+N)
```

Design consequences:

- **The pool file is the single source of truth.** Rotating nodes means rewriting one
  file, not editing every account.
- **`slot = 0` means "no proxy".** Installations without a pool keep working unchanged —
  the feature is additive, never required.
- **Running out of slots is an explicit error**, not a silent fallback to a shared exit.
  Silently degrading isolation is worse than refusing to add the account.

### Slot assignment is per-account, and it persists

The slot is written into the account record when the account is created. It is **not**
recomputed on startup. This matters because recomputation would reshuffle the mapping
whenever the pool changes — see *Capacity changes* below.

---

## Grouping: measure, don't assume

The pool is built by grouping **nodes** into pools, but the grouping key is the
**observed exit IP**, not the node name.

This distinction is not theoretical. Node names in a subscription are labels, not
identities:

- Two differently-named nodes can share one exit IP.
- One node can present different exit IPs over time.
- Nodes with rotating exits change IP between probes.

So the build process is:

```
1. Load candidate nodes from the subscription
2. Probe each one's actual exit IP
3. Group nodes by that measured IP
4. Assign one group per pool slot
```

Anything that cannot be measured is not trusted. Nodes whose exit rotates within a
single probing session are **excluded** — they cannot provide a stable anchor, which
violates goal 2.

> **Note:** a node whose exit changes *between* sessions is a different situation from
> one that changes *within* a session. The former is normal for residential pools and is
> accepted; the latter makes the mapping meaningless and is filtered out.

### Ordering

Groups are ordered by region preference before being assigned to slots, so the
higher-quality regions land on the lower slots. Region preference is a deployment
choice, not a hardcoded rule.

---

## Capacity changes

This is the subtlest part of the design.

**Adding capacity reshuffles slots if the build re-sorts groups.** When the pool grew,
the sort order changed, so slot *N* pointed at a different group — and every account
silently moved to a different exit IP. That breaks goal 2 and, worse, breaks it for
accounts the upstream had already seen on a specific IP.

The rule that follows:

> **Growing the pool is not just "add slots". Existing accounts must be re-bound to
> whichever new slot carries their original exit IP.**

The procedure is a reverse lookup: for each account, find the slot whose group contains
the exit IP the account was using, and re-point the account at that slot. Verify
afterwards that every pre-existing account still resolves to its original exit.

Back up the account store before doing this — the re-bind touches every account.

---

## Failure handling

Two independent failure modes, handled at different layers:

| Failure | Layer | Response |
|---|---|---|
| A node dies (connection refused, timeout) | Account | Short cooldown, skip to next account |
| A request is malformed | Request | **No** cooldown — retry would fail identically |

The distinction is essential. A cooldown triggered by a bad request would remove a
perfectly healthy account from rotation for no reason. Only **network-layer** failures
count as egress failures; request-construction errors, client cancellations, and
upstream business errors do not.

### Why a short cooldown rather than failover-only

Failover alone keeps the service up, but a dead node is then re-tried on **every**
request — each attempt paying the connection timeout before moving on. Measured
impact was roughly two seconds of added latency per request, with the log filling
with identical errors. A short cooldown (minutes, not hours) means the bad path is
attempted once and then skipped until it plausibly recovers.

Cooldown length is a trade-off:

- Too short → the dead node is retried constantly, back to the original problem.
- Too long → a recovered node stays unused, wasting capacity.

A few minutes matches typical transient network recovery without wasting much capacity.

### Redundancy within a group

Groups *can* hold multiple nodes so that a node failure is absorbed by switching to a
sibling node **without changing the exit IP**. This only works when the group was built
from nodes that genuinely share one exit.

In practice most pools end up single-node, because stable distinct exits are the
scarce resource — the design optimises for **isolation first, redundancy second**.
Redundancy is a bonus where it happens to be available, not a guarantee.

---

## Verifying the design

The properties above are checkable, and should be checked rather than assumed:

| Goal | Check |
|---|---|
| One exit per account | Probe every listen port; assert all results are distinct |
| Mapping is stable | Probe, wait, probe again; the set must not change |
| Pool is loopback-only | Inspect the listener table — no non-loopback bind |
| Account↔slot is intact | For each account, resolve slot → port → exit and compare with the recorded value |
| Dead node is cooled down | Point an account at a closed port; assert it is tried once, then skipped |

The distinctness probe is the load-bearing one. Everything else in this document
exists to make that property hold.

---

## Summary of the rules

1. Isolation is per **exit IP**, and the exit IP is the account's identity to upstream.
2. **Measure** exit IPs; never trust node names or region labels.
3. **Stable mapping beats convenience** — never silently re-point an account.
4. **Adding capacity requires re-binding**, not just appending.
5. **Cooldown only on network-layer failures**, and keep it short.
6. **Loopback-only, LAN disabled, dedicated control port** — the pool is private.
7. **No pool is a valid state.** The feature is additive and must degrade to "direct".
