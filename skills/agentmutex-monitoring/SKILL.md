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
  acquirer takes over automatically. **Caveat for deploys:** a SIGKILLed
  `run` can leave its deploy child still running (orphaned) while the lease
  shows `expired` — so "expired" does not guarantee the resource is idle.
  Before taking over a crashed deploy, check nothing is still deploying (no
  stray build/deploy process, staging not mid-write).
- `free` — available; a fresh waiter in `waiters` will grab it within ~1s.
- `corrupt` — unparseable state file; needs a human (see force-release).
- `unreadable` — an I/O error (bad permissions, or `holder.json` replaced by
  a directory) blocks reading the lease; a human should `force-release --yes`
  it. `list` surfaces these rather than hiding them.

### Is the in-flight deploy healthy or stuck?

`status <key>` shows **held for** (elapsed) and **renewed** (recency). A
`run`-held deploy auto-renews, so a recent `renewed` means the holder is
alive; a `held for` far beyond the expected build time means the build is
wedged. Rule of thumb while waiting on staging:

```bash
agentmutex status deploy:staging   # look at "held for" vs your build's normal duration
```

- recent `renewed` + reasonable `held for` → healthy build, keep waiting.
- `renewed: never` on a lease that should be a `run` deploy, or `held for`
  ≫ normal build time → likely wedged; a human may need to investigate or the
  holder should have used `run --max-hold`.

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

Caveats: `wait` returns as soon as the lease is gone — which includes an
`expired` (crashed) holder, not just a clean release, and it fires the instant
the holder releases even if a FIFO waiter is about to reclaim staging in the
next second. So "wait says free" means "no active lease right now", not
"staging is idle and yours". If you actually intend to deploy, don't `wait`
then act — that's a race; use `agentmutex run deploy:staging -- …`, which
takes its turn in the queue atomically.

Inference-bound polling: agents don't need push notifications — checking once
per second (the built-in poll) or even once per minute between your other
actions is plenty. A cheap pattern inside an agent loop:

```bash
# Cleanest: let the CLI do the branching (no parsing). Always handle the
# corrupt/unreadable arm (5) so you're not blind to a wedged staging lock.
agentmutex status --exit-code deploy:staging
case $? in
  0) echo held;;
  3) echo free;;
  4) echo expired;;
  5) echo "WEDGED — needs a human (force-release)";;
esac
# Or extract just the state value:
state=$(agentmutex status --json deploy:staging | sed -n 's/.*"state": "\([a-z]*\)".*/\1/p')
```

…do other useful work, and check again on your next turn rather than burning
a tool call spinning.

## Inspecting raw state (read-only!)

Use `$AGENTMUTEX_DIR` — on a shared deploy box the store is almost always set
there, not at `~/.agentmutex`, so hardcoding the home path inspects the wrong
(often empty) directory during an incident:

```bash
D="${AGENTMUTEX_DIR:-$HOME/.agentmutex}"
ls "$D/locks/"                                   # one dir per key (percent-encoded)
cat "$D/locks/deploy%3Astaging/holder.json"      # the lease document
ls -l "$D/locks/deploy%3Astaging/queue/"         # waiters; mtime = last heartbeat
```

A waiter is treated as dead when its process is gone (checked by PID on this
host) or, for a same-host live-but-starved agent, once its heartbeat is very
stale; `agentmutex prune` removes such entries. A live but CPU-starved agent
(e.g. co-located with a heavy build) keeps its FIFO slot, so the queue stays
fair even under load.

**Never** create, edit, or delete files under `~/.agentmutex` yourself —
mutations must go through the CLI, which serializes them with a per-key
guard. Reading is always safe.

## Who held it before? (the lock changelog)

`history` answers "who deployed staging last, when, and how did their lease
end" — invaluable when you find staging in an unexpected state:

```bash
agentmutex history deploy:staging              # newest first: acquired/released/expired/force-released
agentmutex history --since 24h                 # all keys, last day
agentmutex history --json --limit 20 deploy:staging   # machine-readable
agentmutex history --all deploy:staging        # include renew heartbeats (liveness forensics)
```

Reading it: a `released` event is a clean handoff; `expired` means that
holder crashed or was SIGKILLed (their deploy may have been cut short —
check what actually landed); `force-released` records a human override and
which process forced it; `reclaimed` means a `run` had its lease cleared
out from under it and re-protected itself. Events of one lease share a
`lease_id`. History is best-effort: treat it as evidence, not as the lock's
source of truth (that's `status`).

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
