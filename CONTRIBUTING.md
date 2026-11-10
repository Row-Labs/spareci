# Contributing

## Setup

You need Go (see `go.mod`), Node 20+ for the compat test, and `gh` logged in.

```sh
git clone https://github.com/Row-Labs/spareci && cd spareci
npm ci --prefix test/compat
go test ./...
go build -o bin/spareci ./cmd/spareci
```

`go test -short ./...` skips the Node compat test.

## Trying a change end to end

Use a private test repo you admin. Run spareci against it, and push a workflow whose `runs-on` is `${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}`:

```sh
./bin/spareci up --repo you/test-repo -v
```

This repo's own CI is the reference workflow. Point `--repo` at your fork of spareci and push. Runner logs go to `~/Library/Caches/spareci/logs/` (`~/.cache/spareci/logs/` on Linux).

## Guidelines

- No new dependencies without a good reason; spareci is a single static binary.
- Behavior that touches GitHub (what jobs see, what a cache lookup returns) should match GitHub's own semantics. Say where they differ, in code comments and in `docs/how-it-works.md`.
- Anything that can't work (unsupported runner version, cache patch failing) should fall back to GitHub's behavior with a warning, not break the job.
- Tests for protocol changes go in `internal/cache/server_test.go`, and in `test/compat` if the real client is involved.
