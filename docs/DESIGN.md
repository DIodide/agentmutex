# agentmutex design

This document specifies the on-disk protocol. The promise: **mutual exclusion
per semantic key across any number of concurrent processes on one filesystem,
with no daemon**, surviving crashed holders and crashed waiters.

## Why polling is the right primitive here

Agent tasks are inference-bound. Between two actions an agent spends seconds
to minutes inside an LLM call; the critical sections being protected (builds,
deploys, migrations) run for minutes. Against that timescale a 1-second disk
poll is invisible — so the design deliberately trades "instant" wakeups for
the reliability of having **no daemon, no sockets, no notification channel**.
Every operation is a short-lived CLI process over files. If the machine is up
and the filesystem works, agentmutex works.

## State layout

```
$AGENTMUTEX_DIR (default ~/.agentmutex)
└── locks/
    └── <encoded-key>/          # key percent-encoded to be filesystem-safe
        ├── guard               # flock(2) target; empty; never deleted
        ├── holder.json         # current lease, absent when free
        └── queue/
            └── <%020d-arrival-unixnano>-<token>.json
```

Keys are percent-encoded (bytes outside `[A-Za-z0-9._-]` become `%XX`) so the
mapping is reversible and safe on every platform, including Windows where `:`
is illegal in filenames.

## The two atomic primitives

Everything reduces to two well-understood filesystem guarantees:

1. **Per-key guard (`flock`)** — every *mutation* (acquire, release, renew,
   prune) runs while holding an exclusive `flock(2)` on `guard`. Critical
   sections are single read-modify-write sequences lasting milliseconds. The
   kernel drops the lock when the process exits, *however* it exits, so a
   SIGKILLed CLI cannot wedge the store. On Windows (no flock) the guard is
   an `O_EXCL` marker file with a 60s staleness reclaim; reclaim is
   *rename-based* (exactly one contender wins the rename) and release
   verifies marker identity, so a raced reclaim can neither delete a live
   marker nor be deleted by the stale holder it displaced.

2. **Atomic document replace (rename)** — `holder.json` is written to a temp
   file and `rename(2)`d into place. Readers (`status`, `list`, monitoring
   scripts) take no guard and still never observe a torn document.

## Lease lifecycle

```
acquire:                       release:                renew:
  guard {                       guard {                 guard {
    h = read holder               h = read holder         h = read holder
    if h live: blocked            if !h: ERR 13           if !h: ERR 13
    if queue head != me:          if h.token != t:        if h.token != t:
        blocked                       ERR 12                  ERR 12
    write holder (rename)         remove holder.json      h.expires = now+ttl
    remove my queue entry       }                         write holder (rename)
  }                                                     }
```

- A lease is **live** iff `now < expires_at`. An expired lease is displaced
  in-place by the next legitimate acquirer (no separate reaper needed).
- The **token** is 128 random bits, returned to the acquirer and stored in
  `holder.json`. Release and renew require it. This is the file-based
  equivalent of the Redis `GET == token THEN DEL` Lua script from the
  pattern: a lock can only be cleared by its owner (or explicitly by a human
  via `force-release`). `status`/`list` redact tokens so other agents aren't
  handed the credential to misuse — but note this is accident prevention,
  not a security boundary: any local process can read the state files.
- Renewing an expired-but-not-yet-displaced lease succeeds — better to keep a
  live agent's lease than to invite a collision window.

## Pessimistic waiting: the FIFO queue

`acquire` without `--no-wait` is Strategy A (pessimistic orchestration):

1. Try once. If the key is free and no fresh waiter is ahead, done.
2. Otherwise **enqueue**: create `queue/<arrival-nanos>-<token>.json`.
   Creating a uniquely-named file is atomic; no guard needed.
3. Poll every `--poll` (default 1s, jittered +0–25% so waiters don't march
   in lockstep; bounded to 10ms–10s so heartbeats always outpace the
   staleness window). Each poll:
   - `TryAcquire` under the guard. It succeeds only if the key is free *and*
     the oldest **fresh** queue entry is ours → FIFO fairness.
   - **Heartbeat**: touch our queue entry's mtime, proving we're alive.
4. On success/timeout/interrupt, remove our queue entry.

A waiter whose entry mtime is older than 30s (`WaiterStaleAfter`) stopped
heartbeating — it was killed mid-wait. Stale entries are skipped by everyone
and cleaned by `prune`, so a dead waiter never blocks the queue.

Note the asymmetry, and why it's sound: *waiter* liveness is mtime-based
(waiters actively poll, so silence for 30s means death), while *holder*
liveness is TTL-based (holders are busy doing minutes-long work and merely
renew occasionally). Each liveness signal matches its role's natural cadence.

## Crash matrix

| Who dies | What happens |
|---|---|
| Holder, mid-work | Lease expires at TTL; next fresh queue head displaces it. `run` auto-renews at TTL/3 (which is why `run` requires `--ttl ≥ 5s`), so only SIGKILL of the wrapper leads here. |
| Holder doesn't die but *loses the lease* (host suspended past TTL, or a human force-release) | `run` detects it on the next renew: by default it terminates the command (SIGTERM, then SIGKILL after 10s) and exits 14, because continuing would mutate concurrently with the new holder. `--on-lease-loss continue` opts out. |
| Waiter, mid-wait | Its queue entry goes stale in 30s and is skipped/pruned. |
| CLI, holding the guard | Kernel releases the flock at process exit. (Windows fallback: 60s staleness reclaim.) |
| CLI, mid-write of holder.json | The temp file is orphaned; the rename never happened, so state is unchanged. |

## Deliberate non-features

- **No reentrancy** — a second acquire of the same key by the same agent
  queues like anyone else. Re-entrant leases hide double-mutation bugs, which
  are exactly what agents commit.
- **No lock directories are ever deleted** — another process may hold an open
  fd on `guard`; deleting and recreating it would let two processes hold "the"
  guard simultaneously. Empty directories are free; `prune` removes only
  expired leases and stale queue entries.
- **No cross-host story** — see README non-goals.
- **No fencing tokens for external systems** — agentmutex serializes
  *cooperating agents*. If your deploy target itself needs protection from
  zombie processes, it needs its own fencing; a fenced sequence number could
  be added to the lease document later.
