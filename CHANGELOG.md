# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

A multi-agent adversarial hunt (60 items) drove a second hardening pass. See
[ROADMAP.md](ROADMAP.md) for the capability backlog that remains.

### Fixed

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
