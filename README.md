# spareci

Run your GitHub Actions jobs on a machine you already own. No Kubernetes, no Terraform, no cloud account, no workflow rewrite.

```sh
spareci up --repo owner/name
```

```yaml
# .github/workflows/ci.yml: the only change to your workflow
runs-on: ${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}
```

`spareci init` makes that change for the jobs you pick and opens a pull request. While `spareci up` is running, those jobs run on your machine for free: macOS jobs on a Mac, and Linux jobs on a Linux machine or [on a Mac with a container runtime](#which-jobs-come-here). Stop it, and they run on GitHub-hosted runners again. CI never sits waiting on a laptop that's asleep.

> **Status: early.** Jobs run directly on the host as your user, with a separate home directory (see [Security](#security)). Use it for private repos whose workflows you trust.

## Why

GitHub-hosted runners bill by the minute, and macOS minutes cost about 10x Linux minutes. Your Mac mini, spare laptop or $5 VPS is idle most of the day. The existing ways to use it either cost money (hosted runner vendors) or need real infrastructure (Kubernetes for actions-runner-controller, Terraform and an AWS account for the autoscaling modules). A plain `./config.sh` runner is free, but it's persistent and shared between jobs, and it breaks CI whenever the machine is off.

spareci is the missing middle: one command, ephemeral runners, and automatic fallback.

## What would it save?

Before installing anything, see what your Actions minutes cost and how much of that this machine could take:

```sh
spareci estimate                          # the repo in the current directory, last 30 days
spareci estimate --repo you/app --repo you/api --days 90
spareci estimate --org your-org --json    # every repo the token can see, as JSON
```

```
you/app · Oct 11 – Nov 10 (30 days) · 225 runs, 806 jobs

  RUNNER                   JOBS   MINUTES           COST  THIS MACHINE
  macOS standard            526     2,091           $130  yes
  Linux 2-core              279       969          $5.81  no
  Self-hosted                 1         7          $0.00  already
  Total, GitHub-hosted              3,060           $135

≈ $135/month on GitHub-hosted runners.
This machine (macOS ARM64) could take 2,091 of those minutes: ≈ $130/month saved.
```

It reads your runs and jobs through the GitHub API and changes nothing. Each job is priced the way GitHub bills it: rounded up to the whole minute, at the list price for its runner type, with standard runners free in public repos. Prices come from [GitHub's pricing page](https://docs.github.com/en/billing/reference/actions-runner-pricing) and are dated in `internal/estimate/prices.go`. "This machine" means same OS and CPU architecture. Your plan's included usage ($12 to $300 a month) is not subtracted, and larger runners are priced only when their label gives the size (`ubuntu-8-core`). It makes one API request per run, so `--org` over a busy org can take a few minutes; it stops early rather than use up your hourly rate limit.

## Install

Requires macOS or Linux (arm64 or x64), the [`gh` CLI](https://cli.github.com) logged in, and admin rights on the repo (registering runners needs them).

> These commands work once the repo is public. Until then, build from a clone (see [CONTRIBUTING.md](CONTRIBUTING.md)).

Homebrew (macOS and Linux). Homebrew only loads casks from taps you've trusted, so trust the spareci tap first:

```sh
brew trust row-labs/spareci
brew install row-labs/spareci/spareci
```

Or download a prebuilt binary from the [latest release](https://github.com/Row-Labs/spareci/releases/latest). Pick `darwin` or `linux` and `arm64` or `amd64`:

```sh
curl -fsSL https://github.com/Row-Labs/spareci/releases/latest/download/spareci_darwin_arm64.tar.gz | tar xz spareci
sudo mv spareci /usr/local/bin/
```

Or with Go:

```sh
go install github.com/Row-Labs/spareci/cmd/spareci@latest
```

Check it with `spareci version`.

## Set up a repo

```sh
cd your-repo
spareci init
```

`init` lists every job in `.github/workflows`, recommends the ones this machine can take (same OS and architecture; macOS jobs need a Mac; on a Mac with a running container runtime, Linux jobs too), and asks which to convert. It rewrites only those jobs' `runs-on` lines, shows the diff, then commits to a new branch, pushes it and opens a pull request. Nothing changes for anyone until spareci is running.

```sh
spareci init --dry-run                          # show the diff, change nothing
spareci init --all-compatible                   # no prompt: every recommended job
spareci init --jobs ci:build,release:macos      # no prompt: these jobs (workflow file name:job id)
spareci init --upgrade                          # no prompt: move SPARECI_RUNS_ON lines to the per-OS variables
```

Without a terminal, pass `--jobs`, `--all-compatible` or `--upgrade`. The pull request goes into the branch you're on (`--base` to change it), which must match what's on GitHub.

What it converts:

| `runs-on` | Becomes |
|---|---|
| `macos-15` | `${{ vars.SPARECI_RUNS_ON_MACOS \|\| 'macos-15' }}` |
| `ubuntu-24.04` | `${{ vars.SPARECI_RUNS_ON_LINUX \|\| 'ubuntu-24.04' }}` |
| `ubuntu-24.04-arm` | `${{ vars.SPARECI_RUNS_ON_LINUX_ARM \|\| 'ubuntu-24.04-arm' }}` |
| `[self-hosted, macOS, ARM64]` | `${{ vars.SPARECI_RUNS_ON_MACOS \|\| fromJSON('["self-hosted","macOS","ARM64"]') }}` |

The variable is the one for the runner the job falls back to (see [Which jobs come here](#which-jobs-come-here)); a label spareci doesn't know gets this machine's own. A list keeps every label in its fallback, since a self-hosted runner has to carry them all. A self-hosted job is only offered if this machine has every label it asks for. `init` leaves alone, and says why: `runs-on` expressions (including `${{ matrix.os }}`; edit those by hand), runner groups, YAML aliases, reusable-workflow calls, jobs that are already converted and the watchdog workflow. Jobs with `container:` or `services:` need a Linux machine.

## Use

```sh
cd your-repo
spareci up                 # take jobs for this repo; ^C to stop
spareci up --max 2         # run two jobs at once
spareci up --max 3 --max-linux 1   # at most one of them a Linux job
spareci up --dry-run       # show which queued jobs would come here, register nothing
spareci status             # runners and queued jobs
spareci up --job-home=user # give jobs your own HOME and environment, as before
spareci up --git-mirror=false  # no local git mirror; checkouts fetch everything
spareci cache ls           # what's in the local cache
spareci down               # after a crash: route jobs back to GitHub, remove stale runners
spareci estimate           # what your Actions minutes cost, and what this machine could save
spareci stats              # what the jobs this machine ran would have cost on GitHub
```

Press `^C` once and spareci routes new jobs back to GitHub, lets running jobs finish, and then sends any job still queued for this machine to GitHub-hosted runners. Press it twice and running jobs are aborted.

### What did it save?

Every job `spareci up` runs is recorded, along with the GitHub-hosted runner it would have used otherwise:

```sh
spareci stats
spareci stats --json
```

```
spareci saved you $1.77 · 6 jobs · 28 min since Oct 20, 2026

                         JOBS   MINUTES      SAVED
  This week                 4        16      $1.03
  This month                5        21      $1.34
  All time                  6        28      $1.77

  BY REPO
  acme/app                  4        23      $1.77
  acme/site                 1         3 free (public)
  acme/api                  1         2      $0.00

  INSTEAD OF
  macOS 5-core M2 Pro       1        10      $1.02
  macOS standard            2        12     $0.744
  Linux 2-core              2         4     $0.006
  Self-hosted               1         2      $0.00
```

Once the machine has run more than one kind of job, a `RAN ON` section splits them by backend: `macos-arm64` natively, `linux-arm64` in a container, and so on. Each record keeps its backend.

The runner a job would have used is its fallback, the `'macos-15'` in `${{ vars.SPARECI_RUNS_ON_MACOS || 'macos-15' }}`, read from the workflow file at the run's commit. Jobs are priced exactly as `spareci estimate` prices them: GitHub's timestamps, rounded up to the whole minute, at list price, free for standard runners in public repos. A job whose fallback spareci can't read (a `matrix.os` expression, a reusable workflow, a custom runner label) isn't priced or guessed; `stats` counts it separately and `--json` says why. Weeks start on Monday and months on the 1st, in local time. Each finished job also logs a line like `saved $0.434: 7 min on macOS standard; $1.77 in all over 6 jobs`. Amounts under $1 are shown to a tenth of a cent, so small Linux savings still add up visibly.

The record is a local file, `~/Library/Caches/spareci/stats/jobs.jsonl` (`~/.cache/spareci/stats/` on Linux), one line per job, shared by every repo on the machine. It stays on this machine.

### Laptop mode

On a machine you also work on, tell spareci when it may take jobs:

```sh
spareci up --only-on-ac            # not on battery
spareci up --idle 10m              # only after 10 minutes without keyboard or mouse input
spareci up --hours 18:00-08:00     # only in this local-time window (may cross midnight)
spareci up --only-on-ac --idle 5m --hours 18:00-08:00   # all must hold
```

When a condition stops holding, spareci pauses. It does what `^C` does, but keeps running:

- It clears the routing variables (and the heartbeat), so new jobs go to GitHub-hosted runners.
- It stops runners that haven't picked up a job. Running jobs finish.
- Jobs already queued for this machine are sent to GitHub-hosted runners, as described [below](#when-this-machine-goes-away). A run with a job still in progress waits until that job ends, so pausing never throws away work.

When the conditions hold again, it routes jobs here again. Each change is logged, and `spareci status` shows whether the machine is taking jobs or paused, and why. Flags work with `spareci install` too.

| | macOS | Linux |
|---|---|---|
| `--only-on-ac` | `pmset` | `/sys/class/power_supply`; a machine with no battery counts as on AC |
| `--idle` | `HIDIdleTime` (any keyboard, mouse or trackpad input) | best effort: GNOME's idle monitor, then `xprintidle` (X11), then logind's idle hint. Needs your desktop session, so it may not work from a systemd user service. |
| `--hours` | local clock | local clock |

If this machine can't measure a condition you asked for, `spareci up` says so and exits instead of ignoring it.

### Which jobs come here

Each kind of job has its own repo variable, picked by the GitHub-hosted runner the job falls back to. `spareci up` sets a variable only while this machine can run that kind of job, and clears it when it stops:

| Variable | Set to | Jobs | Set by |
|---|---|---|---|
| `SPARECI_RUNS_ON_MACOS` | `spareci-macos` | `macos-*` | a Mac |
| `SPARECI_RUNS_ON_LINUX` | `spareci-linux` | `ubuntu-*` (x64) | an x64 Linux machine; a Mac with an x64 container backend, or with `--linux-x64-on-arm` |
| `SPARECI_RUNS_ON_LINUX_ARM` | `spareci-linux-arm` | `ubuntu-*-arm` | an arm64 Linux machine; an Apple Silicon Mac with a container runtime |

On a Mac, Linux jobs run in a fresh container each (OrbStack, Docker Desktop or Colima), from an image spareci builds locally the first time (a few minutes). `--linux-cpus` and `--linux-memory` limit each job's container. Jobs get the container runtime's Docker socket (`--linux-docker=none` to refuse), so a job can control the runtime: see [Linux jobs in containers](docs/linux-containers.md) for that trade-off and what the image has compared with GitHub's. Without a running container runtime, the Mac sets only `SPARECI_RUNS_ON_MACOS`, and Linux jobs stay on GitHub. `--max` limits all jobs at once; `--max-macos` and `--max-linux` limit each kind. Run spareci on one machine per repo for now: a second machine for the same repo shares the variables, and when one stops it briefly turns routing off for the other (see [how it works](docs/how-it-works.md#routing)).

`ubuntu-24.04` means x64. Most jobs don't care, and an arm64 container runs natively on Apple Silicon, so `--linux-x64-on-arm` lets arm64 containers take those jobs instead of x64 ones under Rosetta. On this repo's CI that made the build about 2.5x faster than x64 under Rosetta, and faster than GitHub-hosted ([measured](docs/linux-containers.md#architecture)). Leave it off for jobs that download x64 binaries.

A job is sent to a backend when its `runs-on` labels are all ones that backend's runners have: `self-hosted`, the OS (`macOS` or `Linux`), the arch (`ARM64` or `X64`) and its routing label (change the `spareci` prefix with `--label`). The `vars.SPARECI_RUNS_ON_* || ...` form is what gives you the fallback, and spareci sets and clears those variables for you.

**Upgrading from 0.1.** spareci 0.1 used one variable, `SPARECI_RUNS_ON` (set to `spareci`), for every job, so a Linux job opted in on a Mac ran on macOS. spareci still sets `SPARECI_RUNS_ON` for the machine's own OS and architecture, so existing workflows keep working, but it's deprecated and will stop being set in a later release. Run `spareci init` (or `spareci init --upgrade`) to move those lines to their per-OS variable, and `spareci watchdog` to regenerate the watchdog, which clears every routing variable.

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
  cd your-repo
  spareci watchdog --setup   # then commit .github/workflows/spareci-watchdog.yml to your default branch
  ```

  `--setup` opens GitHub in your browser twice: once to create a private GitHub App with a single permission (Variables: read and write), once to install it on this repo. It stores the app's client ID and key as repo secrets. Each watchdog run mints a token from them that lasts an hour, so there's nothing to renew. For a repo in an organization, the app belongs to the organization, which needs an org owner (or app manager) to create it.

  While spareci is up it refreshes a heartbeat variable every minute. When the heartbeat is more than 5 minutes old, the watchdog clears the routing variables and re-runs jobs that were queued for this machine on GitHub-hosted runners. Expect that within about 15 to 20 minutes of the machine going away, longer if GitHub is slow to start scheduled workflows. While spareci is healthy, the watchdog's runs are skipped and use no Actions minutes. When the machine comes back, spareci turns routing on again by itself.

  The app is needed because a workflow's own `GITHUB_TOKEN` can't change repo variables. If you'd rather not create an app, use a fine-grained personal access token instead (it expires, so set a reminder):

  ```sh
  spareci watchdog > .github/workflows/spareci-watchdog.yml
  gh secret set SPARECI_TOKEN   # paste a fine-grained token for this repo with "Variables: read and write"
  ```

Sending a job back means cancelling its workflow run and re-running it, because GitHub reads `vars` once per run attempt. Usually only the cancelled jobs are re-run. If an earlier job in the run already finished on this machine, the whole run is re-run on GitHub, since that job's caches are on this machine where a GitHub-hosted job can't restore them. See [docs/how-it-works.md](docs/how-it-works.md#routing).

## How it works

- **Real runner, single use.** For each queued job, spareci registers a just-in-time runner with GitHub and starts the official [`actions/runner`](https://github.com/actions/runner) in a throwaway copy-on-write clone. The runner takes exactly one job and exits, and its directory is deleted. Logs, status checks, annotations and artifacts behave as on hosted runners.
- **No inbound connections.** spareci polls the GitHub API (with ETags, so idle polling barely touches your rate limit) and works from behind NAT or on a laptop.
- **Local cache.** `actions/cache` and everything built on it (`setup-node`, `setup-go`, `setup-python`, ...) save to and restore from this machine's disk instead of uploading to GitHub. Saving a 1 GB Go build cache takes seconds instead of a minute or more. Caches follow GitHub's rules: a key is immutable once saved, and a pull request can read its base branch's caches but not write them. When the cache passes `--cache-size` (default 10 GB), the least recently used entries are evicted.
- **Shared toolchains.** Tools downloaded by `setup-*` actions are kept between jobs.
- **Faster checkouts.** spareci keeps a mirror of the repo on this machine and updates it before each job, so `actions/checkout` downloads only what the mirror doesn't have yet. It still fetches from github.com with the job's own token, so nothing changes for auth, pushes, submodules or LFS. On microsoft/vscode a checkout went from 13s to 5.5s. Turn it off with `--git-mirror=false`.
- **Docker.** Jobs use the host's Docker, so image layers persist between jobs. `docker/build-push-action` with `cache-to: type=gha` saves its layers to the local cache too.

See [docs/how-it-works.md](docs/how-it-works.md) for the details, including how the local cache is wired in.

## Security

Every job runs as your user on the host. By default it gets its own home directory and a cleaned environment (see [What a job can see](#what-a-job-can-see)), but that keeps jobs out of your personal tool state, not out of your account: a job can still read anything your user can read. That's the same trust model as running the workflow's commands yourself, so:

- Use spareci only with **private repos** whose workflows and contributors you trust.
- **Never with public repos.** A fork's pull request could run code on your machine.

Per-job VM and container isolation is the next major milestone, and public-repo support waits for it. See [docs/isolation.md](docs/isolation.md) for the design.

## What a job can see

By default (`--job-home=spareci`) each repo's jobs get a persistent home of their own, `~/Library/Caches/spareci/home/<owner>/<repo>` (`~/.cache/spareci/home/...` on Linux). It's shared by that repo's jobs, so tool caches stay warm between runs, and it's separate from yours.

| | Separate from you | Notes |
|---|---|---|
| `HOME` and everything tools keep under it: `~/go`, Go build cache, `~/.npm`, `~/.cargo`, `~/.rustup`, pip cache, `~/.gitconfig`, `~/.npmrc`, `~/.docker/config.json` | Yes | Tools find their paths from `HOME`, so `setup-go` and friends cache the job home, not yours. |
| Your shell environment (`GOPATH`, `CARGO_HOME`, `XDG_*`, `GH_TOKEN`, `SSH_AUTH_SOCK`, ...) | Yes | Jobs get only locale, `TMPDIR`, proxy and CA settings, `DEVELOPER_DIR`, `DOCKER_HOST` and `PATH`. |
| Tools installed in your home (rustup, nvm, pyenv, `~/go/bin`, `~/.local/bin`) | Yes | Those `PATH` entries are removed. Install toolchains in the workflow (`setup-node`, `dtolnay/rust-toolchain`, ...) as on hosted runners. Homebrew and other system-wide installs still work. |
| git's system config | Yes | `GIT_CONFIG_NOSYSTEM=1`, which also turns off the macOS keychain credential helper Xcode's and Homebrew's git enable. `actions/checkout` uses the job's own token. |
| macOS keychain | Partly | The job home has no keychain search list, so the default lookups find nothing. The login keychain is still unlocked in your session and readable by path. |
| SSH keys and `~/.ssh/config` | No | `ssh` finds your home from the user database, not `HOME`. |
| Xcode: DerivedData, simulators, signing identities | No | Xcode also ignores `HOME`. Pass `-derivedDataPath` to keep builds out of `~/Library/Developer`. |
| Everything else your user can read or write | No | Files, Docker daemon, local network. |

`--job-home=user` restores the old behaviour: your own `HOME` and your full environment. `--job-home=/some/path` uses that directory instead. To reset a repo's job home, delete its directory while spareci is stopped.

## Limits

- Jobs in the same repo share their job home, and so do concurrent jobs (`--max 2`). Whatever one job leaves there, the next one sees.
- One repo per `spareci up` process.
- Jobs using `container:` or Docker container actions can't reach the local cache yet; they fall back to GitHub's cache.
- BuildKit's `type=gha` cache uses the local cache on macOS only (through `host.docker.internal`). On Linux it uses GitHub's cache service, as without spareci.
- Without the watchdog workflow, if spareci dies without `^C` (crash, power loss), jobs queue for this machine until it starts again or you run `spareci down`. `spareci install` restarts it after a crash, but can't help when the machine itself is off.
- Sending a job back to GitHub cancels its whole run first, because GitHub has no per-job cancel. `--max-wait` therefore waits until no job in that run is running. If any job in the run already finished here, the whole run is re-run on GitHub, which repeats that work.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Releases are cut as described in [docs/releasing.md](docs/releasing.md).

## License

[MIT](LICENSE)
