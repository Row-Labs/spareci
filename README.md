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

Press `^C` once and spareci routes new jobs back to GitHub, lets running jobs finish, and then sends any job still queued for this machine to GitHub-hosted runners. Press it twice and running jobs are aborted.

### Laptop mode

On a machine you also work on, tell spareci when it may take jobs:

```sh
spareci up --only-on-ac            # not on battery
spareci up --idle 10m              # only after 10 minutes without keyboard or mouse input
spareci up --hours 18:00-08:00     # only in this local-time window (may cross midnight)
spareci up --only-on-ac --idle 5m --hours 18:00-08:00   # all must hold
```

When a condition stops holding, spareci pauses. It does what `^C` does, but keeps running:

- It clears `SPARECI_RUNS_ON` (and the heartbeat), so new jobs go to GitHub-hosted runners.
- It stops runners that haven't picked up a job. Running jobs finish.
- Jobs already queued for this machine are sent to GitHub-hosted runners, as described [below](#when-this-machine-goes-away). A run with a job still in progress waits until that job ends, so pausing never throws away work.

When the conditions hold again, it routes jobs here again. Each change is logged, and `spareci status` shows whether the machine is taking jobs or paused, and why. Flags work with `spareci install` too.

| | macOS | Linux |
|---|---|---|
| `--only-on-ac` | `pmset` | `/sys/class/power_supply`; a machine with no battery counts as on AC |
| `--idle` | `HIDIdleTime` (any keyboard, mouse or trackpad input) | best effort: GNOME's idle monitor, then `xprintidle` (X11), then logind's idle hint. Needs your desktop session, so it may not work from a systemd user service. |
| `--hours` | local clock | local clock |

If this machine can't measure a condition you asked for, `spareci up` says so and exits instead of ignoring it.

Route only the jobs you want. A job is sent here when its `runs-on` labels are all ones this machine has: `self-hosted`, the OS (`macOS` or `Linux`), the arch (`ARM64` or `X64`) and `spareci` (change it with `--label`). The `vars.SPARECI_RUNS_ON || ...` form is what gives you the fallback, and spareci sets and clears that variable for you.

### Run it in the background

```sh
spareci install            # start `spareci up` for this repo now and at every login
spareci install --max 2    # flags after install are passed to `up`
spareci logs -f            # follow its log
spareci uninstall          # stop it, route jobs back to GitHub, remove the service
```

On macOS this is a launchd agent in `~/Library/LaunchAgents`, on Linux a systemd user unit in `~/.config/systemd/user`. Either one restarts spareci within 10 seconds if it crashes or is killed. On a Linux server, run `loginctl enable-linger $USER` so it keeps running while you're logged out.

### When this machine goes away

A job never waits on this machine for long:

- **Busy machine.** A job routed here that waits more than `--max-wait` (default 10 minutes) is sent to GitHub-hosted runners. Turn it off with `--max-wait 0`.
- **Dead machine** (crash, `kill -9`, power cut, a laptop that went to sleep). Nothing on the machine can clean up, so a small workflow on GitHub does it. Add it once:

  ```sh
  spareci watchdog > .github/workflows/spareci-watchdog.yml   # commit it to your default branch
  gh secret set SPARECI_TOKEN   # paste a fine-grained token for this repo with "Variables: read and write"
  ```

  While spareci is up it refreshes a heartbeat variable every minute. When the heartbeat is more than 5 minutes old, the watchdog clears the routing variable and re-runs jobs that were queued for this machine on GitHub-hosted runners. Expect that within about 15 to 20 minutes of the machine going away, longer if GitHub is slow to start scheduled workflows. While spareci is healthy, the watchdog's runs are skipped and use no Actions minutes.

  The token is needed because a workflow's own `GITHUB_TOKEN` can't change repo variables. When the machine comes back, spareci turns routing on again by itself.

Sending a job back means cancelling its workflow run and re-running it, because GitHub reads `vars` once per run attempt. Usually only the cancelled jobs are re-run. If an earlier job in the run already finished on this machine, the whole run is re-run on GitHub, since that job's caches are on this machine where a GitHub-hosted job can't restore them. See [docs/how-it-works.md](docs/how-it-works.md#routing).

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
- Without the watchdog workflow, if spareci dies without `^C` (crash, power loss), jobs queue for this machine until it starts again or you run `spareci down`. `spareci install` restarts it after a crash, but can't help when the machine itself is off.
- Sending a job back to GitHub cancels its whole run first, because GitHub has no per-job cancel. `--max-wait` therefore waits until no job in that run is running. If any job in the run already finished here, the whole run is re-run on GitHub, which repeats that work.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
