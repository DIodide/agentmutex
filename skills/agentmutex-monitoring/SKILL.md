---
name: agentmutex-monitoring
description: Observe and monitor agentmutex semantic locks - check who holds a lock, watch for a lock to be released or become ready, inspect the on-disk state under ~/.agentmutex, decide when a stale lock can be safely force-released. Use when waiting on another agent's deploy/task, debugging "lock held" failures, or cleaning up after crashed agents.
---

# Monitoring agentmutex locks

Everything is on disk under `~/.agentmutex` (or `$AGENTMUTEX_DIR`), and the
read-side commands (`status`, `list`, `prune`, plus `acquire`/`renew` for
their own results) take `--json`. Reading state never blocks and never needs
a lock. Lease tokens are redacted from `status`/`list` output — the token
belongs to the holder alone.

## Who is holding what?

```bash
agentmutex list                       # table: KEY, STATE, HOLDER, EXPIRES, WAITERS
agentmutex status deploy:staging      # holder, reason, expiry, queue for one key
agentmutex status --json deploy:staging
```

JSON shape (stable):

```json
{
  "key": "deploy:staging",
  "state": "held",              // "held" | "expired" | "free" | "corrupt" | "unreadable"
  "holder": {
    "agent": "claude-a",
    "pid": 4213,
    "host": "ci-runner-7",
    "reason": "deploy v1.2.3",
    "acquired_at": "2026-07-27T20:00:00Z",
    "expires_at": "2026-07-27T20:15:00Z"
  },
  "waiters": [ { "agent": "claude-b", "fresh": true, "enqueued_at": "..." } ],
  "error": ""                   // populated only when state is corrupt/unreadable
}
```

The holder's `pid`/`host` are what you check before force-releasing (see
below); the lease `token` is redacted from `status`/`list` output.

For scripting, `agentmutex status --exit-code <key>` maps state to an exit
code (0 held, 3 free, 4 expired, 5 corrupt/unreadable) so you can branch
without parsing JSON.

Interpreting `state`:

- `held` — an agent is working; `expires_at` is the worst-case wait if that
  agent crashed this instant (live holders renew, so it can move forward).
- `expired` — the holder stopped renewing (crashed or SIGKILLed). The next
  acquirer takes over automatically; nothing needs cleanup to make progress.
- `free` — available; a fresh waiter in `waiters` will grab it within ~1s.
- `corrupt` — unparseable state file; needs a human (see force-release).
- `unreadable` — an I/O error (bad permissions, or `holder.json` replaced by
  a directory) blocks reading the lease; a human should `force-release --yes`
  it. `list` surfaces these rather than hiding them.

## Waiting for a lock to be released / ready

**If you intend to mutate the resource afterwards, don't watch — acquire.**
Watching and then acting is a race; the pessimistic pattern is
`agentmutex run <key> -- <cmd>` which waits fairly in the queue for you
(see the `agentmutex` skill).

To *observe* without claiming (e.g. "tell me when the deploy finishes"):

```bash
# Block until the key has no active lease (exit 0), with a bound (exit 11):
agentmutex wait --timeout 20m deploy:staging
```

Inference-bound polling: agents don't need push notifications — checking once
per second (the built-in poll) or even once per minute between your other
actions is plenty. A cheap pattern inside an agent loop:

```bash
# Cleanest: let the CLI do the branching (no parsing).
agentmutex status --exit-code deploy:staging; case $? in 0) echo held;; 3) echo free;; 4) echo expired;; esac
# Or extract just the state value:
state=$(agentmutex status --json deploy:staging | sed -n 's/.*"state": "\([a-z]*\)".*/\1/p')
```

…do other useful work, and check again on your next turn rather than burning
a tool call spinning.

## Inspecting raw state (read-only!)

```bash
ls ~/.agentmutex/locks/                                  # one dir per key (percent-encoded)
cat ~/.agentmutex/locks/deploy%3Astaging/holder.json     # the lease document
ls -l ~/.agentmutex/locks/deploy%3Astaging/queue/        # waiters; mtime = last heartbeat
```

A queue entry whose mtime is >30s old is a dead waiter (they heartbeat every
poll); it gets skipped automatically and `agentmutex prune` removes it.

**Never** create, edit, or delete files under `~/.agentmutex` yourself —
mutations must go through the CLI, which serializes them with a per-key
guard. Reading is always safe.

## Cleaning up after crashed agents

Usually: do nothing. Expired leases are displaced automatically and stale
waiters are skipped. For hygiene:

```bash
agentmutex prune          # removes expired leases + stale queue entries; always safe
```

`force-release` is the break-glass override — it discards someone's lease
without their token:

```bash
agentmutex force-release deploy:staging          # dry run: shows what it would do
agentmutex force-release --yes deploy:staging    # actually clears it
```

Only force-release when **all** of these hold:

1. `status` shows the holder's `expires_at` is far away, but
2. you have confirmed the holding process is dead (its `pid` on its `host`),
   and
3. a human has approved it — as an agent, report the situation and ask
   rather than deciding alone. The lock might be protecting a half-finished
   deploy.

If `state` is `corrupt`, `force-release --yes` is also the fix (after
reporting it).
