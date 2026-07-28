# agentmutex

**Semantic mutexes for AI agents. No daemon, no server: just files.**

[![CI](https://github.com/DIodide/agentmutex/actions/workflows/ci.yml/badge.svg)](https://github.com/DIodide/agentmutex/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/DIodide/agentmutex.svg)](https://pkg.go.dev/github.com/DIodide/agentmutex)

Multiple AI agents working on the same machine step on each other: two agents
tag-deploy the same staging environment at the same time and one wipes out the
other's build. Traditional locks don't fit — agent critical sections are
LLM-loop-sized (minutes, not microseconds), and nobody wants to run a Redis or
a daemon just to coordinate a couple of terminals.

`agentmutex` implements the [agentic mutex pattern](https://ninelayer.in/blog/agent-mutex):
before an agent touches a shared resource, it must acquire a **lease on a
semantic key** — a structured name that matches the business entity, like
`deploy:staging` or `account:12345`. Leases have TTLs so crashed agents can't
wedge the system, and only the holder of the lease **token** can release or
renew it.

```bash
# Agent A
agentmutex run deploy:staging -- ./deploy.sh staging

# Agent B, at the same time — blocks until A's deploy finishes, then runs
agentmutex run deploy:staging -- ./deploy.sh staging
```

That's the whole fix for the duelling-deploys problem.

## Install

```bash
go install github.com/DIodide/agentmutex@latest
```

Or from source:

```bash
git clone https://github.com/DIodide/agentmutex
cd agentmutex && make install
```

## Quick start

```bash
# The golden path: acquire → run → auto-renew → release, in one command.
# --ttl covers the whole build+deploy (run auto-renews at ttl/3).
agentmutex run --ttl 20m --reason "deploy v1.2.3 to staging" \
  deploy:staging -- make deploy-staging

# Manual lifecycle, when the work spans multiple commands. ALWAYS guard the
# acquire — a failed lock must abort, never deploy unlocked:
TOKEN=$(agentmutex acquire --ttl 20m --reason "shipping v1.2.3" deploy:staging) || {
  echo "someone else is deploying; aborting" >&2; exit 1; }
trap 'agentmutex release --token "$TOKEN" deploy:staging' EXIT
make build && make deploy-staging

# Who's holding what? (list shows what each environment is deploying + for how long)
agentmutex status deploy:staging
agentmutex list
```

## Design

### Pessimistic orchestration

This is **Strategy A** from the pattern: when Agent A holds `deploy:staging`,
Agent B's `acquire` for the same key waits — it registers in an on-disk FIFO
queue and polls. When A releases (or A's lease expires because it crashed), B
wakes up, reads *fresh* state, and proceeds. Data integrity first; concurrency
only across unrelated keys.

### No daemon

Every operation is an inline CLI command over on-disk state in
`~/.agentmutex`. There is nothing to install as a service, nothing to keep
running, nothing to reach over the network:

- **Mutations are serialized** by a per-key `flock(2)` guard held for
  milliseconds. The kernel releases it automatically if a process dies, so a
  crashed CLI can never wedge the store.
- **Lease documents are atomic**: `holder.json` is replaced via
  temp-file + rename, so readers never see a torn write.
- **Waiting is polling** — and that's a feature, not a compromise. Agent tasks
  are inference-bound: an agent thinks for seconds to minutes between actions,
  so a 1-second disk poll adds ~0% latency while removing every daemon,
  socket, and notification channel from the design. Reliability comes from
  having fewer moving parts.

### Leases, not locks

A lock you can never reclaim is a deadlock waiting for a crash. Every acquire
takes a **TTL** (default 15m). A holder that finishes early releases; a holder
that needs longer renews; a holder that dies silently expires and the next
waiter takes over. `agentmutex run` renews automatically at TTL/3 while your
command runs, so the TTL only matters if the whole process is SIGKILLed.

Only the lease **token** (printed by `acquire`) can release or renew — an
agent can never clobber a lock it doesn't hold, even by accident.

### Semantic keys

Keys are structured namespaces that match the business entity, not the
implementation:

```
deploy:staging
deploy:production
service:api:database
repo:frontend/file:auth.ts
account:12345
customer:acme:onboarding
```

Allowed characters: letters, digits, `. _ - : /` (start with a letter or
digit, max 80 chars). Lock the *entity you're mutating*, at the finest
granularity that still covers the whole mutation.

Keys are case-sensitive, but on case-insensitive filesystems (macOS and
Windows defaults) keys differing only by case share one lock — the safe
direction (extra serialization, never a missed one). Just pick one spelling
per resource.

## Command reference

| Command | Description |
|---|---|
| `acquire <key>` | Acquire a lease. Waits (FIFO) by default; prints the token on stdout. |
| `release <key>` | Release a lease you hold (`--token` or `$AGENTMUTEX_TOKEN`). |
| `renew <key>` | Extend a lease you hold by `--ttl` from now. |
| `run <key> -- <cmd…>` | Acquire, run a command with auto-renew, always release. |
| `status [<key>]` | Show holder + queue for a key (or all keys). `--json` for machines. |
| `list` | List all known locks. |
| `wait <key>` | Block until a key is free. Observational — does **not** acquire. |
| `force-release <key>` | Human override for wedged locks. Dry-run unless `--yes`. |
| `prune` | Remove expired leases and stale queue entries. |
| `version` | Print version. |

Useful flags on `acquire`/`run`: `--ttl 20m`, `--timeout 30m` (give up
waiting), `--no-wait` (try-lock), `--reason "why"`, `--agent name`,
`--quiet`. `acquire` also takes `--json` and `--token-file PATH` (keep the
token out of CI logs). `run` also takes `--on-lease-loss terminate|continue`
(default `terminate`), `--max-hold 25m` (abort a wedged deploy holding the
lock too long), and `--export-token` (pass the token to the child so it can
renew/release — off by default, since env is readable by same-user
processes). `status` takes `--exit-code` for scripting (0 held, 3 free,
4 expired, 5 corrupt/unreadable) and `wait` takes `--json`. Flags go before
positional arguments. Aliases: `lock`/`unlock`/`ls`.

> **All coordinating agents must point at the same store.** Coordination
> happens through `$AGENTMUTEX_DIR` (default `~/.agentmutex`). If two agents
> use different directories — e.g. an ephemeral per-job CI dir — they do
> **not** see each other's locks and both "acquire" successfully: the mutex
> fails *open*. On a shared deploy box, set `AGENTMUTEX_DIR` to one durable,
> shared path for every agent.

Inside `run`, the wrapped command inherits `AGENTMUTEX_LEASE_KEY`,
`AGENTMUTEX_TOKEN`, and `AGENTMUTEX_DIR` for the held lease, so a script can
renew or release early, and a nested `agentmutex acquire`/`run` on the *same*
key fails fast with a self-deadlock message instead of blocking forever.

### Exit codes

Stable — agents branch on these:

| Code | Meaning |
|---|---|
| `0` | Success |
| `2` | Usage error |
| `10` | Lock held / queued, and `--no-wait` was set |
| `11` | Timed out waiting (`--timeout`) |
| `12` | Token does not match the current lease |
| `13` | No lease exists for the key |
| `14` | `run`: lease lost while the command was executing (resource may have been mutated concurrently) |
| `1` | Other errors |

`status --exit-code <key>` instead maps state to an exit code for scripting:
`0` held, `3` free, `4` expired, `5` corrupt/unreadable.

Additionally: `run` **forwards the wrapped command's exit code** once the
command has started (so a child that exits 10 is indistinguishable from
"lock held" by code alone — codes 10/11 from `run` can only occur *before*
the command starts); `127` means the command could not be started; a wait
interrupted by a signal exits `128+signum` (`130` SIGINT, `143` SIGTERM,
`129` SIGHUP).

### Environment

| Variable | Meaning |
|---|---|
| `AGENTMUTEX_DIR` | State directory (default `~/.agentmutex`) |
| `AGENTMUTEX_TOKEN` | Default token for `release` / `renew` |
| `AGENTMUTEX_AGENT` | Default agent name recorded on leases |

## On-disk layout

State is plain JSON you can inspect with `cat` — see
[docs/DESIGN.md](docs/DESIGN.md) for the full protocol:

```
~/.agentmutex/locks/<encoded-key>/
├── guard         # per-key flock target (empty)
├── holder.json   # current lease: token, agent, reason, acquired_at, expires_at
└── queue/        # FIFO waiters: <arrival-nanos>-<waiter-id>.json (mtime = heartbeat)
```

Reading the files directly is supported (that's how you monitor). **Writing
or deleting them directly is not** — always go through the CLI, which holds
the guard.

## Agent skills

The [`skills/`](skills/) directory ships two [Claude Code skills](https://docs.anthropic.com/en/docs/claude-code)
so your agents use the mutex correctly without being told twice:

- [`skills/agentmutex/`](skills/agentmutex/SKILL.md) — when and how to lock:
  key naming, the pessimistic discipline (acquire → **re-read fresh state** →
  act → release), TTL sizing, exit-code handling.
- [`skills/agentmutex-monitoring/`](skills/agentmutex-monitoring/SKILL.md) —
  how to watch locks: `status --json`, `wait`, reading the on-disk state,
  when force-release is (and isn't) appropriate.

Install them for all your projects:

```bash
cp -r skills/agentmutex skills/agentmutex-monitoring ~/.claude/skills/
```

or per-project into `.claude/skills/`. They work as-is for any agent that can
read markdown and run shell commands.

## Scope and non-goals

- **One machine.** Coordination happens through the local filesystem
  (`flock`, mtimes, one clock). Agents on different hosts need a networked
  lock service instead.
- **Advisory, cooperative locking.** Like `flock`, nothing stops a process
  that simply doesn't call `agentmutex`. It is a coordination protocol for
  cooperating agents, not a security boundary.
- **Pessimistic only (v0).** Optimistic version-hash verification and
  sandbox-merge strategies from the pattern are out of scope for now.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The test suite includes a real
multi-process mutual-exclusion stress test (`go test ./...`).

## License

[MIT](LICENSE) © 2026 Ibraheem Amin
