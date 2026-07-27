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
- **`run` timing output.** Emit machine-readable wait duration, hold
  duration, and renew count (e.g. a JSON line on stderr or `--metrics`), so
  the calling agent can observe contention.
- **`list --prefix` filtering.** Filter the lock table by key prefix, natural
  given the hierarchical key design.

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
