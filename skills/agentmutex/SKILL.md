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
# Tag-deploy to the single shared staging environment. --ttl covers the whole
# build+deploy; run auto-renews at ttl/3 so a slow build never drops the lock.
agentmutex run --ttl 20m --timeout 40m \
  --agent "$AGENT_ID" --reason "deploy tag v1.2.3 (sha abc123) to staging" \
  deploy:staging -- ./deploy.sh v1.2.3
```

- **`--ttl` must cover your worst-case hold** (build + deploy). `run`
  auto-renews at `ttl/3`, so the TTL is really your crash-recovery budget: if
  your agent is killed mid-deploy, others wait out the remaining TTL before
  taking over. For a 5–10 min build, `--ttl 15m`–`20m` is sensible. Too small
  risks a mid-deploy expiry on a CPU-pegged box; the floor is 5s.
- **`--timeout`** bounds queue wait (exit 11). Size it for hold × expected
  queue depth (N agents each holding ~T ⇒ up to N·T), not just one hold.
  Without it you wait forever — dangerous inside an agent tool call that has
  its own timeout and will be killed while merely queued. Prefer launching a
  long deploy in the **background** (`nohup … &` / detached) and polling
  `agentmutex status`, so a turn-ending signal doesn't kill a queued deploy.
- **`--agent "$AGENT_ID"`**: every agent on one box otherwise shows the same
  `user@host` identity, so `status`/`list`/`force-release` can't tell you
  apart. Set a unique id per agent.
- **`--reason`**: record *what* is deploying (tag + sha). It's shown in
  `list`/`status` so humans and queued agents see what's in flight. Always set it.
- **`--max-hold 25m`** (optional): abort with exit 14 if the build wedges and
  holds staging longer than expected, instead of blocking everyone forever.

## Choosing the key: lock the ENVIRONMENT, not the tag

**The single most important rule for deploys.** There is one staging
environment. Two agents deploying *different* tags to staging still collide —
the second build overwrites the first. So they must serialize on the
**environment**, not on the tag or version:

| Deploying… | Key ✅ | NOT ❌ |
|---|---|---|
| any tag to staging | `deploy:staging` | `deploy:tag:v1.2.3` ← different tags wouldn't serialize! |
| any tag to production | `deploy:production` | `deploy:v2` |
| the `api` service's own staging | `deploy:api:staging` | — |

Rules:
- **Key by the shared resource that gets clobbered** (the environment), never
  by the thing you happen to be pushing (the tag/version/sha). Keying by tag
  silently defeats mutual exclusion — different-tag deploys run concurrently.
- If your build+deploy is triggered *asynchronously* by moving a git tag
  (push tag → CI builds for 10 min), the critical section is the **whole
  build**, not the fast tag push. Either wrap a command that blocks until the
  deploy finishes, or hold the lease across the tag push *and* the wait.
- Use the same spelling everyone else uses — check `agentmutex list` first.
- Different environments → different keys, so staging and production deploys
  run in parallel.

Other resources follow the same "lock what gets clobbered" rule:
`service:api:database` for a migration, `repo:frontend/file:auth.ts` for a
shared file, `account:12345` for an account.

## Manual lifecycle (multi-step work)

Prefer `run` — it can't leak this way. Only reach for manual acquire/release
when the critical section spans several separate commands. If you do,
**you MUST check that the acquire succeeded before mutating anything** — a
failed acquire leaves `$TOKEN` empty and, without the guard below, the shell
would deploy *with no lock*, which is the exact clobber you're preventing:

```bash
set -euo pipefail
TOKEN=$(agentmutex acquire --ttl 20m --timeout 40m \
  --agent "$AGENT_ID" --reason "schema migration" service:api:database) || {
    echo "could not acquire lock (someone else is deploying); aborting" >&2
    agentmutex status service:api:database >&2
    exit 1   # DO NOT proceed to mutate — you don't hold the lock
}
trap 'agentmutex release --token "$TOKEN" service:api:database' EXIT
# IMPORTANT: re-read fresh state NOW, after acquiring (see the discipline below)
./migrate.sh && ./verify.sh
# trap releases on exit, even on failure
```

- Save the token — it's the only way to release/renew. Losing it means
  waiting out the TTL or asking a human to `force-release`.
- **Manual `acquire` does NOT auto-renew** (only `run` does). Size `--ttl` to
  ≥ 2× your worst-case duration, or `agentmutex renew --token "$TOKEN" --ttl
  20m <key>` before it expires — otherwise the lease can lapse mid-migration
  and another agent takes over.
- Always release in a `trap … EXIT` so a failure can't strand the lease.
  (Or just use `run`, which does all of this for you.)

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
code — with two exceptions. Codes 10/11 from `run` only ever mean the lease
was never acquired (they can't come from your command, which hadn't started).
Code 14 is always agentmutex's own "lease lost mid-command" signal, never
your command's. Any *other* nonzero code came from your command.

## Don'ts

- Don't write or delete files under `~/.agentmutex` directly — always use the
  CLI (mutations require the on-disk guard protocol).
- Don't use `force-release` to get past a lock; that's a human-approved
  override for crashed agents only (see the `agentmutex-monitoring` skill).
- Don't hold a lease across long non-critical work (LLM brainstorming, code
  review). Acquire late, release early.
- Don't invent new key spellings for a resource that already has one in
  `agentmutex list`.
