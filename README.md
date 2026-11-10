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

> **Status: early.** Jobs run directly on the host as your user, with a separate home directory (see [Security](#security)). Use it for private repos whose workflows you trust.

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
spareci up --job-home=user # give jobs your own HOME and environment, as before
spareci cache ls           # what's in the local cache
spareci down               # after a crash: route jobs back to GitHub, remove stale runners
```

Press `^C` once and spareci routes new jobs back to GitHub and lets running jobs finish. Press it twice and running jobs are aborted.

Route only the jobs you want. A job is sent here when its `runs-on` labels are all ones this machine has: `self-hosted`, the OS (`macOS` or `Linux`), the arch (`ARM64` or `X64`) and `spareci` (change it with `--label`). The `vars.SPARECI_RUNS_ON || ...` form is what gives you the fallback, and spareci sets and clears that variable for you.

## How it works

- **Real runner, single use.** For each queued job, spareci registers a just-in-time runner with GitHub and starts the official [`actions/runner`](https://github.com/actions/runner) in a throwaway copy-on-write clone. The runner takes exactly one job and exits, and its directory is deleted. Logs, status checks, annotations and artifacts behave as on hosted runners.
- **No inbound connections.** spareci polls the GitHub API (with ETags, so idle polling barely touches your rate limit) and works from behind NAT or on a laptop.
- **Local cache.** `actions/cache` and everything built on it (`setup-node`, `setup-go`, `setup-python`, ...) save to and restore from this machine's disk instead of uploading to GitHub. Saving a 1 GB Go build cache takes seconds instead of a minute or more. Caches follow GitHub's rules: a key is immutable once saved, and a pull request can read its base branch's caches but not write them. When the cache passes `--cache-size` (default 10 GB), the least recently used entries are evicted.
- **Shared toolchains.** Tools downloaded by `setup-*` actions are kept between jobs.

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
- If spareci is killed without `^C` (crash, power loss), the routing variable stays set and jobs queue until you run `spareci down` or start it again. A background service with crash-safe routing is planned.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).
