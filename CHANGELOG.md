# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/).

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
