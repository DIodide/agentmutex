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
        ├── holder.json         # current lease (mode 0600), absent when free
        └── queue/
            └── <%020d-arrival-unixnano>-<waiter-id>.json
```

The per-key directory is mode 0700 and `holder.json` is 0600, so the lease
token in the holder document is not readable by other users. Queue entry
names and bodies use a public **waiter id**, distinct from the private lease
token, so a waiter's presence never discloses the token it will receive on
winning. (Among processes of the *same* user the filesystem is a shared trust
domain — this is advisory locking, not a security boundary.)

Keys are percent-encoded (bytes outside `[A-Za-z0-9._-]` become `%XX`) so the
mapping is reversible and safe on every platform, including Windows where `:`
is illegal in filenames.

## The two atomic primitives

Everything reduces to two well-understood filesystem guarantees:

1. **Per-key guard (`flock`)** — every *mutation* (acquire, release, renew,
   prune) runs while holding an exclusive `flock(2)` on `guard`. Critical
   sections are single read-modify-write sequences lasting milliseconds. The
   kernel drops the lock when the process exits, *however* it exits, so a
   SIGKILLed CLI cannot wedge the store. The acquire is a bounded `LOCK_NB`
   poll (not a blocking `LOCK_EX`), so a peer wedged inside the guard fails
   the waiter loudly after `guardAcquireMax` rather than hanging every
   command forever. On Windows (no flock) the guard is an `O_EXCL` marker
   file with a 60s staleness reclaim; reclaim is *rename-based* (exactly one
   contender wins the rename) and release verifies marker identity, so a
   raced reclaim can neither delete a live marker nor be deleted by the stale
   holder it displaced.

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
2. Otherwise **enqueue**: create `queue/<arrival-nanos>-<waiter-id>.json`.
   Creating a uniquely-named file is atomic; no guard needed.
3. Poll every `--poll` (default 1s, jittered +0–25% so waiters don't march
   in lockstep; bounded to 10ms–10s so heartbeats always outpace the
   staleness window). Each poll:
   - `TryAcquire` under the guard. It succeeds only if the key is free *and*
     the oldest **fresh** queue entry is ours → FIFO fairness.
   - **Heartbeat**: touch our queue entry's mtime, proving we're alive.
4. On success/timeout/interrupt, remove our queue entry.

Waiter liveness is **PID-based on the local host**: a waiter records its
`host` and `pid`, and a same-host waiter is a live contender as long as its
process exists (`kill(pid, 0)`), bounded by a generous `WaiterPIDGrace`
(default 5m) against PID reuse. This matters for deploys: the agent holding
the lease often runs a CPU-saturating build on the same box, which can starve
a co-located queued agent of scheduling long enough to miss its 1s heartbeat.
Under a pure mtime rule that live-but-starved agent would be wrongly skipped
and **barged by a later arrival — a FIFO violation**. PID-liveness keeps its
slot. Conversely, a same-host waiter whose process is *gone* is dropped
immediately, so a crashed queue head can't block a live agent for the whole
staleness window. Waiters from another host fall back to the mtime heartbeat
(we can't probe a remote PID). Stale/dead entries are cleaned by `prune`.

Note the asymmetry, and why it's sound: *holder* liveness is TTL-based
(holders do minutes-long work and merely renew occasionally), while *waiter*
liveness is process-based (a waiter is a live process polling in a loop).
Each liveness signal matches its role's natural cadence.

### Reclaiming a lease cleared mid-run

If `run`'s lease vanishes while its command is still executing — a human
`force-release`d a lock they thought was stuck, or `prune` removed it after a
starved renew — `run` does not blindly keep deploying unprotected. On the next
renew tick it **re-acquires** the key (bypassing the FIFO queue, since it was
the active holder) to re-establish exclusivity, rotating to a fresh token.
Only if a *different* live holder now owns the key (a genuine collision) does
it treat the lease as lost and terminate the command (exit 14). This keeps the
single staging environment protected through routine operator actions while
still catching real double-deploys.

## Crash matrix

| Who dies | What happens |
|---|---|
| Holder, mid-work | Lease expires at TTL; next fresh queue head displaces it. `run` auto-renews at TTL/3 (which is why `run` requires `--ttl ≥ 5s`), so only SIGKILL of the wrapper leads here. |
| Holder *loses the lease to a competitor* (host suspended past TTL and a waiter takes over, or a human force-release followed by another agent acquiring) | `run` detects it on the next renew (the new holder's token differs). By default it terminates the command's whole process group (SIGTERM, then SIGKILL after 10s) and exits 14, because continuing would mutate concurrently with the new holder. `--on-lease-loss continue` opts out. On Unix a non-interactive child runs in its own process group so backgrounded grandchildren are fenced too; interactive children and Windows fence only the direct child (`run` warns). |
| Lease *cleared with no competitor* (the wrapped command released early via its exported token, or a bare force-release nobody re-acquired) | Not a collision — `run` keeps the command running and does not exit 14. It keeps watching, so a *later* competing acquire is still caught and terminates the run. |
| Renewals *fail past the TTL* (I/O error, ENOSPC, damaged holder file) | Treated as loss once the failures outlast the lease: `run` terminates and exits 14. |
| Waiter, mid-wait | Its queue entry goes stale in 30s and is skipped/pruned. |
| CLI, holding the guard | Kernel releases the flock at process exit. (Windows fallback: 60s staleness reclaim.) |
| CLI, mid-write of holder.json | The temp file is orphaned; the rename never happened, so state is unchanged. |

## The audit trail (history.db)

`release` deletes `holder.json`, so without an audit trail the lock's past is
unrecoverable — bad for debugging agent collisions. Every lease state change
is therefore appended to a SQLite database at `<root>/history.db`:
`acquired`, `renewed`, `released`, `force-released` (with the forcing
process), `expired` (TTL ran out — displaced by a taker or removed by prune),
and `reclaimed` (`run` re-establishing a cleared lease). Events carry a public
`lease_id` correlating one lease's lifecycle; the secret token is never
stored.

Two design rules keep it honest:

1. **Best-effort, never load-bearing.** History writes happen *outside* the
   per-key guard and any failure (full disk, corrupt db) degrades to a
   one-line warning — the lock protocol's correctness never depends on the
   audit log, and recording can't extend a critical section.
2. **Multi-process safe.** WAL mode + a 5s busy timeout let any number of
   concurrent CLI processes append; the driver is pure Go
   (`modernc.org/sqlite`), preserving `CGO_ENABLED=0` cross-compilation.

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
