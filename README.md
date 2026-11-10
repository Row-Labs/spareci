# spareci

Run your GitHub Actions jobs on a machine you already own. No Kubernetes, no Terraform, no cloud account, no workflow rewrite.

```sh
spareci up --repo owner/name
```

```yaml
# .github/workflows/ci.yml: the only change to your workflow
runs-on: ${{ vars.SPARECI_RUNS_ON || 'macos-15' }}
```

While `spareci up` is running, that job runs on your machine for free. Stop it, and the same job runs on GitHub-hosted runners again. CI never sits waiting on a laptop that's asleep.

> **Status: early.** Jobs run directly on the host as your user (see [Security](#security)). Use it for private repos whose workflows you trust.

## Why

GitHub-hosted runners bill by the minute, and macOS minutes cost about 10x Linux minutes. Your Mac mini, spare laptop or $5 VPS is idle most of the day. The existing ways to use it either cost money (hosted runner vendors) or need real infrastructure (Kubernetes for actions-runner-controller, Terraform and an AWS account for the autoscaling modules). A plain `./config.sh` runner is free, but it's persistent and shared between jobs, and it breaks CI whenever the machine is off.

spareci is the missing middle: one command, ephemeral runners, and automatic fallback.

## Install

Requires macOS or Linux (arm64 or x64), the [`gh` CLI](https://cli.github.com) logged in, and admin rights on the repo (registering runners needs them).

```sh
go install github.com/Row-Labs/spareci/cmd/spareci@latest
```

Prebuilt binaries and Homebrew are coming.

## Use

```sh
cd your-repo
spareci up                 # take jobs for this repo; ^C to stop
spareci up --max 2         # run two jobs at once
spareci up --dry-run       # show which queued jobs would come here, register nothing
spareci status             # runners and queued jobs
spareci cache ls           # what's in the local cache
spareci down               # after a crash: route jobs back to GitHub, remove stale runners
```

Press `^C` once and spareci routes new jobs back to GitHub and lets running jobs finish. Press it twice and running jobs are aborted.

### Laptop mode

On a machine you also work on, tell spareci when it may take jobs:

```sh
spareci up --only-on-ac            # not on battery
spareci up --idle 10m              # only after 10 minutes without keyboard or mouse input
spareci up --hours 18:00-08:00     # only in this local-time window (may cross midnight)
spareci up --only-on-ac --idle 5m --hours 18:00-08:00   # all must hold
```

When a condition stops holding, spareci clears `SPARECI_RUNS_ON` so new jobs go to GitHub-hosted runners, stops runners that haven't picked up a job, and lets running jobs finish. When the conditions hold again, it routes jobs here again. Each change is logged, and `spareci status` shows whether the machine is taking jobs or paused, and why.

A job's `runs-on` is decided when it's queued. Jobs that were queued for this machine just before it paused wait until it resumes; spareci logs how many.

| | macOS | Linux |
|---|---|---|
| `--only-on-ac` | `pmset` | `/sys/class/power_supply`; a machine with no battery counts as on AC |
| `--idle` | `HIDIdleTime` (any keyboard, mouse or trackpad input) | best effort: GNOME's idle monitor, then `xprintidle` (X11), then logind's idle hint. Needs your desktop session, so it may not work from a system service. |
| `--hours` | local clock | local clock |

If this machine can't measure a condition you asked for, `spareci up` says so and exits instead of ignoring it.

Route only the jobs you want. A job is sent here when its `runs-on` labels are all ones this machine has: `self-hosted`, the OS (`macOS` or `Linux`), the arch (`ARM64` or `X64`) and `spareci` (change it with `--label`). The `vars.SPARECI_RUNS_ON || ...` form is what gives you the fallback, and spareci sets and clears that variable for you.

## How it works

- **Real runner, single use.** For each queued job, spareci registers a just-in-time runner with GitHub and starts the official [`actions/runner`](https://github.com/actions/runner) in a throwaway copy-on-write clone. The runner takes exactly one job and exits, and its directory is deleted. Logs, status checks, annotations and artifacts behave as on hosted runners.
- **No inbound connections.** spareci polls the GitHub API (with ETags, so idle polling barely touches your rate limit) and works from behind NAT or on a laptop.
- **Local cache.** `actions/cache` and everything built on it (`setup-node`, `setup-go`, `setup-python`, ...) save to and restore from this machine's disk instead of uploading to GitHub. Saving a 1 GB Go build cache takes seconds instead of a minute or more. Caches follow GitHub's rules: a key is immutable once saved, and a pull request can read its base branch's caches but not write them. When the cache passes `--cache-size` (default 10 GB), the least recently used entries are evicted.
- **Shared toolchains.** Tools downloaded by `setup-*` actions are kept between jobs.

See [docs/how-it-works.md](docs/how-it-works.md) for the details, including how the local cache is wired in.

## Security

Today every job runs as your user on the host, with access to whatever your user can reach. That's the same trust model as running the workflow's commands yourself, so:

- Use spareci only with **private repos** whose workflows and contributors you trust.
- **Never with public repos.** A fork's pull request could run code on your machine.

Per-job VM isolation (Tart on macOS, containers or microVMs on Linux) is the next major milestone, and public-repo support will wait for it.

## Limits

- Jobs share your home directory, so tools' own caches (`~/go`, `~/.npm`, `~/.cargo`, ...) are your personal ones, and `setup-*` actions will cache them. A separate job home is part of the isolation work.
- One repo per `spareci up` process.
- Jobs using `container:` or Docker container actions can't reach the local cache yet; they fall back to GitHub's cache.
- If spareci is killed without `^C` (crash, power loss), the routing variable stays set and jobs queue until you run `spareci down` or start it again. A background service with crash-safe routing is planned.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
