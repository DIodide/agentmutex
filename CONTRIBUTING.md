# Contributing to agentmutex

Thanks for helping out! agentmutex is deliberately small; the bar for adding
surface area is high, and the bar for correctness is higher.

## Development

```bash
make build     # build ./agentmutex
make test      # full suite, including multi-process stress tests
make lint      # go vet + gofmt check
```

Go ≥ 1.25. Dependency policy: the locking core is standard library only; the
single third-party dependency is `modernc.org/sqlite` (pure Go, no cgo) for
the history audit log — chosen so `CGO_ENABLED=0` cross-compilation keeps
working. Please don't add further dependencies without discussion.

## What the tests must protect

The core invariant is **mutual exclusion under real concurrency**.
`integration_test.go` builds the actual binary and hammers one key with
8 concurrent processes doing deliberately racy read-modify-writes; any lost
update fails the build. If you touch `internal/mutex`, run the suite
repeatedly (`go test -count=5 ./...`) — races are shy.

Other guarded properties:

- FIFO ordering of waiters (`TestFIFOOrder`)
- Crash recovery via TTL expiry (`TestExpiryTakeover`)
- Token-checked release/renew and the documented exit codes
- Locks are always released by `run`, even when the child fails

## Ground rules for changes

- **No daemon.** Every operation must remain an inline CLI command over
  on-disk state.
- **Exit codes and JSON output are API.** Agents branch on them; changing
  them is a breaking change and needs a version bump and CHANGELOG entry.
- **Never delete lock directories or guard files** at runtime — see
  docs/DESIGN.md ("Deliberate non-features") for why.
- New behavior needs a test that fails without it.

## Reporting issues

Include your OS/filesystem, `agentmutex version`, and if it's a locking bug,
the contents of the relevant `~/.agentmutex/locks/<key>/` directory.
