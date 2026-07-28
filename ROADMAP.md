# Roadmap

agentmutex v0.1 is deliberately minimal: pessimistic, single-machine,
no-daemon semantic locking. A multi-agent adversarial review surfaced a large
backlog; the correctness bugs are fixed (see CHANGELOG), and the items below
are capability/UX improvements kept intentionally out of v0.1 so the core
stays small. They are recorded here so they are not lost, roughly in priority
order. Contributions welcome — see CONTRIBUTING.md.

## Coordination model

- **Optimistic strategy (Strategy B).** Record a version hash with a lease;
  let agents proceed concurrently and reconcile at write time, re-injecting
  changed state into the agent's context. Complements today's pessimistic
  path for read-heavy workloads.
- **Fencing tokens.** Add a monotonic sequence number to each lease so an
  external system (a deploy target) can reject a stale holder's writes even
  after a lease handoff. Today's token protects the *lock*, not the resource.
- **PID-liveness fast takeover.** The holder records `pid`/`host` but they are
  never consulted. When the holder is on this host and provably dead, a
  waiter could displace the lease immediately instead of waiting out the full
  TTL — a big win for the "crashed agent" case.
- **Atomic multi-key acquire.** `acquire k1 k2 k3` with deadlock-safe global
  ordering, so agents needing several resources don't hand-roll lock
  ordering (and deadlock).
- **Key hierarchy / prefix conflicts.** Today `deploy:staging` and
  `deploy:staging:api` are independent. Optionally treat prefixes as
  conflicting (or at least detect and warn), matching the intuition the
  namespaced keys create.

## Observability

- **Event log / audit trail.** `release` deletes `holder.json`, leaving no
  history. An append-only log of acquire/release/expire events would make
  post-incident debugging of agent collisions possible.
- **`run` timing output / `--json`.** Emit machine-readable wait duration,
  hold duration, renew count, and loss cause, so an orchestrating agent gets
  more than human stderr.
- **`list --prefix` / state filter.** Filter the lock table by key prefix or
  by state ("what is deploying right now"), natural given the hierarchical
  key design.

## Deploy workflow (concurrent tag-deploys)

Surfaced by the deploy-focused hunt; the correctness bugs are fixed (see
CHANGELOG), these are the remaining ergonomics/capabilities:

- **Structured deploy metadata.** Record the git tag/sha (and the wrapped
  command) on the lease as first-class fields so `status`/`list` show exactly
  what each environment is deploying, beyond the free-text `--reason`.
- **Deploy coalescing.** When several agents queue to deploy the *same* tag,
  let later ones detect it was already applied and skip, instead of
  re-deploying serially.
- **Priority lane.** An urgent rollback currently waits behind every routine
  staging deploy in strict FIFO; a priority flag would let it jump ahead.
- **Resumable queue position.** A turn-based agent that can't block
  continuously restarts at the back of the queue each turn; a durable
  reservation would hold its place across invocations.
- **Config/env presets.** Per-project defaults for `--ttl`/`--timeout`/
  `--reason` so every invocation doesn't re-specify deploy-correct values.
- **Half-deploy signal.** `run` releases identically on success and failure;
  a lock-level marker that the last hold ended in a failed/partial deploy
  would warn the next agent that staging may be in a mixed state.

## Clock robustness

Single-machine coordination assumes a roughly monotonic wall clock. A few
edges remain (rare, but worth hardening):

- Queue order is a wall-clock `UnixNano` baked into each waiter's filename; a
  backward clock step (NTP `makestep`, VM snapshot-resume) could let a later
  arrival sort ahead. A shared monotonic counter would remove this.
- Freshness and lease expiry compare `now` against stored wall-clock times; a
  forward step can transiently age the whole queue or expire a live lease
  early (PID-liveness mitigates the waiter side). Monotonic-clock or
  heartbeat-delta comparisons would be more robust.

## Library (`internal/mutex` → public `pkg`)

- Promote the blocking-acquire protocol (enqueue + heartbeat + jittered poll
  + dequeue) into the library so non-CLI Go callers get pessimistic waiting
  without reimplementing it.
- `context.Context` cancellation on the guard/wait paths.
- A typed error taxonomy (sentinels/typed errors) for invalid-key and
  guard-timeout, so callers can branch instead of string-matching.
- A constructor with options instead of exported mutable `Store` fields and
  an injectable clock.
- A non-mutating `Verify(key, token)` (and a CLI `agentmutex check`) so a
  holder can confirm it still owns its lease without renewing.

## Packaging & DX

- `goreleaser` cross-platform binaries and a Homebrew formula, so non-Go
  users can install without a toolchain.
- Shell completions (bash/zsh/fish) and `NO_COLOR`-aware output.
- A `make install-skills` target (and/or an install script) to drop the
  Claude Code skills into `~/.claude/skills`.
- A README comparison to `flock(1)` explaining what agentmutex adds (semantic
  keys, TTL leases, FIFO queue, holder metadata, monitoring).

## Windows hardening

- Use a Job Object to fence the wrapped command's whole process tree on
  lease loss (today only the direct child is terminated on Windows; `run`
  warns). Unix already fences via process groups.
- The Windows guard's stale-marker reclaim is rename-based but still not fully
  atomic against a marker recreated between the staleness check and the
  rename (a narrow window; Unix uses `flock` and is unaffected).

## Known minor gaps

- `run`'s final release can fail if it cannot acquire the per-key guard within
  the bound; today it warns and lets the lease lapse at its TTL rather than
  retrying. Acceptable (TTL cleans up), but a bounded retry would be tidier.
- `signalTree`/`killTree` compute the child's pgid then signal it; a child
  reaped in that window could, in theory, collide with a reused pgid. The
  window is closed in practice (we stop signaling once `Wait` returns), but a
  handle-based API would remove it entirely.
- No migration of pre-hardening on-disk state: leases created by an older
  build keep their original 0644/0755 permissions until next rewritten.
