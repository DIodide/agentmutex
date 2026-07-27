---
name: agentmutex
description: Coordinate with other AI agents through semantic locks before mutating any shared resource — deploys, staging/production environments, shared files, databases, accounts. Use BEFORE running a deploy, migration, release, or any command another agent might run concurrently. Provides the agentmutex CLI discipline - acquire a lease, re-read fresh state, act, release.
---

# Locking shared resources with agentmutex

Other agents may be working on this machine at the same time as you. Before
you mutate a resource they might also touch (deploy an environment, run a
migration, rewrite a shared file, mutate an account), you MUST hold the
agentmutex lease for that resource. This is **pessimistic** locking: if the
lease is taken, you wait — you never proceed without it.

## The golden path (use this 95% of the time)

Wrap the mutating command in `run`. It acquires the lease (waiting FIFO
behind other agents), auto-renews while your command runs, and always
releases — even if the command fails:

```bash
agentmutex run --timeout 30m --reason "deploy v1.2.3" deploy:staging -- ./deploy.sh staging
```

- `--timeout 30m` bounds how long you'll wait in the queue (exit 11 on
  timeout). Pick roughly 2× the longest expected hold. Without it you wait
  forever — fine for scripts, bad inside a shell tool call with its own
  timeout.
- `--reason` tells other agents (and humans) what you're doing. Always set it.
- Everything after `--` runs only once the lease is yours.
- Long waits are normal and healthy: it means another agent is mid-deploy and
  you just avoided destroying their work. Prefer launching this in the
  background (or with a generous tool timeout) over skipping the lock.

## Choosing the key

Lock the **business entity you are mutating**, using structured namespaces:

| Situation | Key |
|---|---|
| Deploying to staging | `deploy:staging` |
| Deploying service `api` to production | `deploy:api:production` |
| Migrating a database | `service:api:database` |
| Editing a shared file | `repo:frontend/file:auth.ts` |
| Acting on a customer account | `account:12345` |

Rules: pick the finest key that still covers your whole mutation; use the
same key spelling as everyone else (check `agentmutex list` for the names
already in use); different resources → different keys, so unrelated work
stays parallel.

## Manual lifecycle (multi-step work)

When the critical section spans several separate commands:

```bash
TOKEN=$(agentmutex acquire --ttl 20m --timeout 30m --reason "schema migration" service:api:database)
# ... IMPORTANT: re-read state NOW, after acquiring — see below ...
./migrate.sh && ./verify.sh
agentmutex release --token "$TOKEN" service:api:database
```

- Save the token — it's the only way to release/renew. Losing it means
  waiting out the TTL or asking a human to `force-release`.
- Size `--ttl` to ≥ 2× your worst-case duration, or `agentmutex renew
  --token "$TOKEN" --ttl 20m <key>` before it expires.
- In shell scripts, release in a `trap ... EXIT` so failures don't strand the
  lease. (Or just use `run`, which does all of this for you.)

## The pessimistic discipline

1. **Acquire before reading critical state.** The whole point of waiting is
   that the state changes while you wait.
2. **After the lease is granted, re-read fresh state before acting.**
   Anything you read or concluded *before* acquiring is stale — the previous
   holder changed the world. Re-check what's deployed, re-read the file,
   re-query the account, then act.
3. **Never mutate on a failed acquire.** Exit codes 10 (held, `--no-wait`)
   and 11 (timeout) mean *do not touch the resource*. Report the holder
   (`agentmutex status <key>`) and either wait longer or tell the user —
   never "just do it anyway".
4. **Release promptly.** Do post-work (summaries, notifications) *after*
   releasing.

## Exit codes you must handle

| Code | Meaning | Your move |
|---|---|---|
| 0 | Lease acquired / command ran | Proceed |
| 10 | Held by another agent (`--no-wait`) | Don't touch the resource; wait or report |
| 11 | Timed out waiting | Don't touch the resource; report the holder |
| 12 | Your token is stale (lease expired & was taken over) | Your work may have been interleaved — stop, `agentmutex status <key>`, tell the user |
| 13 | No lease exists | You already released, or TTL expired long ago |
| 14 | `run`: lease lost mid-command; the command was terminated | The resource may have been mutated concurrently — check its real state before retrying |

Exits 12 and 14 are the serious ones: your lease was lost mid-work and
another agent may have already mutated the resource. Don't blindly retry;
surface it.

Note: once the wrapped command starts, `run` forwards the command's own exit
code — so codes 10/11 from `run` only ever mean the lease was never
acquired, and any other nonzero code came from your command.

## Don'ts

- Don't write or delete files under `~/.agentmutex` directly — always use the
  CLI (mutations require the on-disk guard protocol).
- Don't use `force-release` to get past a lock; that's a human-approved
  override for crashed agents only (see the `agentmutex-monitoring` skill).
- Don't hold a lease across long non-critical work (LLM brainstorming, code
  review). Acquire late, release early.
- Don't invent new key spellings for a resource that already has one in
  `agentmutex list`.
