# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- `list` fits narrow terminals: the wide free-text REASON column moved last,
  KEY/HOLDER/REASON cells are truncated with an ellipsis (~100-column worst
  case), and an expired row shows how long the lease was actually held
  rather than an ever-growing "(stale)" figure. The default agent identity
  is now `user@shorthost` (first DNS label) instead of the full FQDN — the
  full hostname is still recorded in the lease's `host` field, and agents
  should keep using `--json` for parsing.

## [0.3.0] - 2026-08-05

### Added

- **Lock history / audit trail.** Every lease state change — `acquired`,
  `renewed`, `released`, `force-released` (with the forcing process),
  `expired` (displaced or pruned), `reclaimed` — is appended to a SQLite
  database at `$AGENTMUTEX_DIR/history.db`, correlated per lease by a public
  `lease_id` (never the token). New `agentmutex history [<key>]` command
  (alias `log`) with `--limit`, `--since`, `--all` (include renew
  heartbeats), and `--json`. Recording is best-effort by design: a broken
  history database warns but can never block a lock operation, and writes
  happen outside the per-key guard.
- Holders now carry a public `lease_id` in `holder.json` and `status --json`.

### Changed

- First third-party dependency: `modernc.org/sqlite` (pure Go, keeps
  `CGO_ENABLED=0` cross-builds). Binary size grows ~3.8 → ~10.6 MB.

## [0.2.0] - 2026-08-04

Three multi-agent adversarial hunts (60 + 54 + 61 items) drove the hardening;
the third focused on the concurrent tag-deploy use case (one staging
environment, 5–10 min builds). See [ROADMAP.md](ROADMAP.md) for the remaining
capability backlog.

### Added (release engineering)

- Prebuilt binaries for Linux/macOS/Windows (amd64 + arm64) attached to
  GitHub releases via goreleaser, plus an `install.sh` for one-line installs.
- The agent skills are installable as a Claude Code **plugin**: the repo is
  its own plugin marketplace (`claude plugin marketplace add DIodide/agentmutex`).

### Fixed (third pass — deploy scenario)

- **`run` re-establishes protection when its lease is cleared mid-deploy.** If
  a `force-release` or `prune` removes the lock while the wrapped command is
  still running, `run` now re-acquires the key instead of finishing the deploy
  unprotected; it terminates (exit 14) only on a genuine takeover by a
  different live holder. Fixes an unprotected-deploy / double-deploy window.
- **FIFO fairness under build load.** Waiter liveness is now PID-based on the
  local host, so an agent CPU-starved by a co-located build keeps its queue
  slot instead of being marked "stale" and barged by a later arrival. A
  crashed queue head is dropped immediately (by dead PID) rather than blocking
  live agents for the full staleness window.
- A queued waiter no longer loses its FIFO position on a transient
  `TryAcquire` error (e.g. a guard-acquisition timeout) — it retries in place.
- `run` escalates an external SIGTERM/SIGINT to SIGKILL after a grace period,
  so a signal-trapping deploy can't keep `run` alive holding the lock forever.
- The lease token is **no longer exported to the wrapped command by default**
  (env is readable by same-user processes); opt in with `--export-token`.
- `renew --json` redacts the token (was printed unredacted).
- Linux: the deploy child gets `Pdeathsig=SIGKILL`, so an OOM-killed `run`
  doesn't orphan a still-running deploy (partial; Unix elsewhere relies on TTL).

### Added (third pass — deploy scenario)

- `run --max-hold DURATION`: abort a wedged/hung deploy (exit 14) instead of
  holding the single staging lock indefinitely.
- `acquire --token-file PATH`: write the token to a 0600 file instead of
  stdout, keeping it out of CI logs.
- `list`/`status` are deploy-triage-oriented: `list` shows each holder's
  **reason** (what tag/sha is deploying) and **held-for** elapsed time in
  place of the near-constant EXPIRES; `status` shows **held for** and renew
  recency (distinguishing a live auto-renewing deploy from a wedged one) and
  each waiter's reason.
- Skills/README rewritten for the tag-deploy workflow: lock the *environment*
  not the tag, size `--ttl` to build time, guard manual `acquire` against
  deploying unlocked on failure, set a unique `--agent`, and the shared-store
  (fail-open) invariant.

### Fixed (second pass — regressions found by the follow-up hunt)

- **`run` no longer kills a command that releases its lease early.** The
  auto-renew watchdog now distinguishes a *takeover* (someone else holds the
  key → terminate, exit 14) from the lease simply being gone (child
  self-released via its exported token, or a bare force-release with no
  competitor → keep running). It keeps watching, so a later competing acquire
  is still caught.
- **`run` no longer hangs interactive commands.** The child is placed in its
  own process group (for tree-fencing) only when stdin is not a terminal, so
  commands that read the tty (sudo/ssh prompts, editors, REPLs) no longer
  stop on SIGTTIN. Signals are delivered to the group only when we created a
  separate one, never to agentmutex's own group. SIGQUIT is also forwarded.
- **`force-release --yes` is idempotent** — an already-free (or never-existed)
  key exits 0, matching the dry-run path, instead of 13.
- **Self-reentry is detected:** acquiring the same key from inside its own
  `run` fails fast with a self-deadlock message instead of blocking forever.
- `prune` sweeps orphaned `.holder-*.tmp` files and only removes queue
  `.tmp` files older than the staleness window (never an in-flight write).
- `status`/`list` keep a readable holder even when the queue read fails, and
  carry the diagnostic for both `corrupt` and `unreadable` states.
- `sanitizeMeta` also strips C1 controls and Unicode bidi/format characters,
  and truncates by rune (never splitting a multibyte character).
- State root and `locks/` are created 0700 (not 0755); queue entries 0600.
- Docs/skills corrected: queue filename is `<waiter-id>` not `<token>`;
  exit-code tables include `status --exit-code` (3/4/5) and 128+signal
  interrupts; `AGENTMUTEX_DIR` export documented; the agent skill no longer
  mislabels exit 14 as the wrapped command's code; `<command> help` works.
- CI now also runs the suite under `-race`.

### Fixed (first pass)

- **Lease token no longer leaks through queue entries.** Waiter entries now
  use a public waiter id in their filename and body, distinct from the
  private lease token, so a waiter's presence cannot disclose the token it
  will receive. `holder.json` is written mode 0600 under a 0700 tree.
- **`run` fences the command's whole process tree.** On Unix the child runs
  in its own process group and lease-loss termination / forwarded signals
  reach backgrounded grandchildren, not just the direct child (Windows
  terminates the direct child and warns).
- **`run` detects lease loss from persistent renew failures** (I/O errors,
  ENOSPC, a damaged holder file) once renewals fail past the TTL, instead of
  warning forever and exiting 0 while another agent could hold the key.
- **`force-release` clears wedged locks** whose `holder.json` is unreadable
  (e.g. replaced by a directory), and `list`/`status` surface such keys as
  `unreadable` instead of hiding them.
- Release/renew on a never-acquired key no longer create phantom lock
  directories.
- Validation: negative `--timeout`, non-positive `--ttl` (including on
  `renew`), and missing token now exit 2 (usage) instead of being silently
  coerced or exiting 1.
- Agent/reason strings are sanitized (control chars/ANSI stripped, bounded),
  preventing forged status lines and terminal escapes.
- Unix guard acquisition is bounded and non-blocking, so a wedged peer fails
  loudly instead of hanging every command forever.
- `prune` only counts real waiter entries, sweeps orphaned temp files,
  continues past per-key errors, and reports grammatically.
- Interrupted waits exit with the conventional 128+signal code (130/143).
- `-h`/`--help` and `help <command>` route to that command's help.

### Added

- `status --exit-code` scripting mode (0 held, 3 free, 4 expired,
  5 corrupt/unreadable) and `wait --json`.
- `run` exports `AGENTMUTEX_LEASE_KEY` and `AGENTMUTEX_TOKEN` to the wrapped
  command (early release/renew, nested-reentry detection).
- `version` falls back to the module version embedded by `go install`.
- ROADMAP.md.

## [0.1.0] - 2026-07-27

### Added

- Token-based semantic leases with TTLs over on-disk state (`~/.agentmutex`),
  no daemon: `acquire`, `release`, `renew`.
- Pessimistic orchestration: blocking `acquire` with an on-disk FIFO queue,
  heartbeated waiter entries, and stale-waiter skipping.
- `run <key> -- <cmd…>`: acquire → run with auto-renew → always release. If
  the lease is definitively lost mid-command, `run` terminates the command
  and exits 14 by default (`--on-lease-loss continue` opts out).
- Monitoring commands: `status`, `list`, `wait`; `--json` on
  `acquire`/`renew`/`status`/`list`/`prune`. Lease tokens are redacted from
  `status`/`list` output.
- Human overrides: `force-release` (dry-run by default), `prune`.
- Stable exit codes for agent branching (10 held, 11 timeout, 12 not holder,
  13 not held, 14 lease lost mid-run).
- Claude Code skills for agent usage (`skills/agentmutex`) and monitoring
  (`skills/agentmutex-monitoring`).
- Multi-process mutual-exclusion stress tests; CI on Linux/macOS/Windows.

[0.3.0]: https://github.com/DIodide/agentmutex/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/DIodide/agentmutex/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/DIodide/agentmutex/releases/tag/v0.1.0
