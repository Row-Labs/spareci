# How spareci works

## Layout

| Package | Job |
|---|---|
| `cmd/spareci` | CLI: flags, signals, wiring |
| `internal/gh` | The few GitHub REST calls needed: queued jobs, JIT runners, repo variables, run history |
| `internal/gate` | Laptop mode: whether the machine takes jobs now (power, user idle time, hours) |
| `internal/pool` | Poll loop: decides how many runners to start, applies `--max-wait`, drains on shutdown |
| `internal/route` | Routing and heartbeat variables, sending queued jobs back to GitHub, the watchdog workflow |
| `internal/ghapp` | `spareci watchdog --setup`: creates the watchdog's GitHub App through the manifest flow and stores its secrets |
| `internal/service` | `spareci install`: launchd agent (macOS), systemd user unit (Linux) |
| `internal/runner` | Installs `actions/runner`, clones it per job, runs it with the job's home and environment, patches the worker for the cache |
| `internal/cache` | Local implementation of GitHub's cache service |
| `internal/gitmirror` | A local mirror of the repo that checkouts borrow objects from |
| `internal/workflows` | `spareci init`: finds jobs' `runs-on`, judges which suit this machine, rewrites the chosen lines in place |
| `internal/estimate` | `spareci estimate`: prices recent jobs at GitHub's list prices |
| `internal/stats` | `spareci stats`: records each job this machine runs and what it would have cost |

## Job lifecycle

1. `pool` polls `GET /repos/{repo}/actions/runs?status=queued|in_progress` and their jobs, keeping queued jobs whose labels are all ones this machine carries.
2. For each job not already covered by a starting runner, it calls `POST /repos/{repo}/actions/runners/generate-jitconfig`. That registers a runner that accepts exactly one job.
3. `runner` clones the cached runner install (APFS `clonefile` on macOS, `--reflink=auto` on Linux, so it's instant and uses no disk), writes `.env`, and runs `run.sh --jitconfig ...`.
4. GitHub assigns a matching job, which is not necessarily the one that triggered the spawn, and that's fine, because the counts balance out. The runner prints `Running job: <name>`, then exits when the job ends.
5. The clone is deleted. If the runner never got a job, its registration is deleted too.

## Routing

On start, spareci sets the repo variable `SPARECI_RUNS_ON` to its label, so `runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}` resolves to this machine. On `^C` it deletes the variable first, so new runs go to GitHub, waits for running jobs, then sends jobs still queued here back to GitHub (below).

**When GitHub reads `vars`.** Once per run attempt, when the attempt is created, and not again when a job becomes ready. Checked on 2026-11-10 with a probe workflow (run 42887218442): with the variable set to a label no runner has, a job that `needs:` a 75-second job still got that label after the variable had been deleted during the wait. After a cancel and `rerun-failed-jobs`, attempt 2 read the deleted variable and ran on `ubuntu-24.04`. `rerun-failed-jobs` includes cancelled jobs. So clearing the variable only affects new runs and new attempts, and a job already queued for this machine stays queued until something re-runs it.

**Sending a queued job back** (`route.Rescue`). GitHub has no per-job cancel and no way to move a queued job to another runner, so:

1. Cancel the run, and force-cancel it if it hasn't completed after a minute (a job held by a runner that went away can block a normal cancel).
2. Delete the routing variable if it's set, re-run, wait for the new attempt to exist, and set the variable again. Runs created in those few seconds also go to GitHub, which is the safe direction.
3. Re-run only the failed and cancelled jobs, unless a job in the run already finished on this machine. Then re-run the whole run: that job saved its caches to the local cache, and a later job re-run on GitHub couldn't restore them. Artifacts don't need this, because they always go through GitHub.
4. Check that the failed-jobs re-run really included every cancelled job. When two queued jobs in a run are cancelled together, GitHub's failed-jobs re-run often includes only one of them: it happened in 4 of 5 such runs on 2026-11-10 (for example 42889578581 and 42894946226). The skipped job is listed under the new attempt number but finished before that attempt started, and the run ends as cancelled even though everything it re-ran passed. If that happens, spareci (and the watchdog) cancels the new attempt and re-runs the whole run.

This happens in three places:

- **`--max-wait`** (default 10m). A job queued here for longer, with no free runner slot for it this poll, is sent back. A run with a job in progress is left alone until that job ends, so cancelling doesn't throw away work.
- **Shutdown** (`^C`, `spareci down`, `spareci uninstall`). Every run with a job queued here is sent back, even if other jobs in it are running.
- **The watchdog workflow**, when the daemon is dead.

### Crash-safe routing

If the daemon dies without `^C`, nothing on the machine can clear the variable, so the safety net has to run somewhere else. Options considered:

| | Hosted minutes | Time to recover | Setup for users |
|---|---|---|---|
| **Scheduled watchdog workflow, skipped by a lease (chosen)** | About 0 while healthy; 1 per outage | About 15 to 20 min, plus GitHub's cron delays | Commit one workflow, run `spareci watchdog --setup` |
| Scheduled watchdog that runs every time | 1 per firing: about 8,600 a month at every 5 minutes, more than the free tier | Same | Same |
| A routing job in front of every run (`needs: route`, hosted) | 1 per workflow run, every run | None | Edit every job in every workflow; about 15s added to every run |
| External service (webhook, Cloudflare Worker) | 0 | Seconds | An account and a deployment, which spareci exists to avoid |
| Only restart on crash (launchd/systemd) | 0 | Seconds after a crash; never if the machine is off or asleep | None |

The chosen design keeps the one-line workflow change and costs nothing while things work. Its price is the recovery delay and a credential that can write variables (below). `spareci install` covers the common crash case in seconds anyway, and the watchdog covers a machine that's off.

**How the lease works.** `spareci watchdog` prints a workflow with 12 cron firings an hour, at minutes 2, 7, ..., 57 (off the top of the hour, when GitHub's scheduler is busiest). GitHub passes the firing's cron string to the run as `github.event.schedule`. Every minute the daemon writes `SPARECI_HEARTBEAT` as `<unix time>|<cron>|<cron>|...|`, listing the firings from 30 minutes ago to 10 minutes ahead. The watchdog job's condition is:

```yaml
if: vars.SPARECI_RUNS_ON && !contains(vars.SPARECI_HEARTBEAT, format('|{0}|', github.event.schedule))
```

so it's skipped, and isn't billed, whenever routing is off or the firing is listed. While the daemon is alive, every firing is listed unless GitHub starts it more than 30 minutes late. When the daemon dies, the list freezes and a firing at most 15 minutes later is unlisted. The job then checks the timestamp itself and only acts if it's at least 5 minutes old. When it acts, it deletes both variables with the app token (or `SPARECI_TOKEN`, see below), then cancels and re-runs every run with a job queued for the routing label, using the rules above. The workflow's `GITHUB_TOKEN` (`actions: write`) is enough for that part. A daemon that comes back, for example a laptop waking up, sets both variables again on its next heartbeat.

`workflow_dispatch` runs always reach the check, which is how to test it by hand. Verified live on 2026-11-10: with a 51-second-old heartbeat the watchdog left routing alone (run 42893719071). After a `kill -9` and a heartbeat more than 5 minutes old, it cleared routing and re-ran a run stranded on the dead machine (42895780440 rescuing 42894946226), which then passed on `ubuntu-24.04`. Those runs were dispatched by hand, because scheduled workflows only run from the default branch. The lease that picks which scheduled firings run is covered by unit tests.

### Watchdog credentials

The workflow's `GITHUB_TOKEN` can cancel and re-run, but GitHub doesn't give it any permission over repo variables, so deleting `SPARECI_RUNS_ON` needs another credential. Options considered:

| | Expires | Hosted parts | Setup | Blast radius if the secret leaks |
|---|---|---|---|---|
| **(a) A private GitHub App per user, made from a manifest (chosen)** | Never; each run mints an hour-long token | None | `spareci watchdog --setup`: two browser clicks | Variables on the repos it's installed on |
| (b) One shared public app plus a token broker | Never | A service that holds the app key and hands out tokens | Install the app | The broker holds a key for every user's repos |
| (c) Fine-grained personal access token (fallback) | On the date the user picked; GitHub emails a warning, but nothing in the repo shows it | None | Make the token in Settings, then `gh secret set SPARECI_TOKEN` | Whatever the user ticked, as the user |

(b) is the smoothest for users, but it means running a service with a key to everyone's repos, which is the thing spareci exists to avoid. (c) works and needs no new code, but a token that expires out of sight is exactly the failure a safety net can't have: the watchdog would fail on the one day it's needed. (a) costs one command and never expires.

**How `--setup` works** (`internal/ghapp`):

1. It listens on `127.0.0.1:<random port>` and opens the browser there. That page POSTs a [manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest) to `github.com/settings/apps/new` (or `/organizations/<org>/settings/apps/new`, since a private app can only be installed on the account that owns it) with a random `state`. The manifest asks for Variables: write (key `actions_variables`) and nothing else, no webhook, not public.
2. The user clicks Create. GitHub redirects to `127.0.0.1:<port>/created?code=...&state=...`. spareci checks `state` and exchanges the code (single use, valid for an hour) at `POST /app-manifests/{code}/conversions` for the app's client ID and private key.
3. It stores them straight away as `SPARECI_APP_CLIENT_ID` and `SPARECI_APP_KEY`, because the key is returned only once. Secret values are sealed to the repo's public key (`GET .../actions/secrets/public-key`, a libsodium sealed box via `golang.org/x/crypto/nacl/box`) as GitHub requires, so the plaintext key goes from GitHub to this process to GitHub and is never written to disk.
4. It redirects the browser to the app's install page with the account and repo preselected, and polls `GET /repos/{repo}/installation` with an app JWT until the user has installed it. GitHub has no API to create an installation, so this click is unavoidable.
5. It mints an installation token and reads the repo's variables with it, so a broken setup fails here rather than during an outage.
6. Run inside the repo, it writes `.github/workflows/spareci-watchdog.yml`.

In the workflow, `actions/create-github-app-token` mints the token only when the heartbeat is stale. That step has `continue-on-error`, and the delete step uses `steps.app.outputs.token || secrets.SPARECI_TOKEN`, so a repo with only a PAT keeps working, and one with both falls back to the PAT if the app was uninstalled. Cancelling and re-running stay on `GITHUB_TOKEN`, so the app never needs Actions permissions.

To remove it: delete the app under the account's Settings → Developer settings → GitHub Apps, then `gh secret delete SPARECI_APP_CLIENT_ID` and `gh secret delete SPARECI_APP_KEY`.

## Laptop mode

`internal/gate` turns `--only-on-ac`, `--idle` and `--hours` into one open/closed answer. Probes (power source, idle time, clock) are plain functions, so the logic is tested with fakes; `probes_darwin.go` and `probes_linux.go` hold the real ones, and the output parsers are tested on every OS.

The pool asks the gate once per poll, before spawning (`internal/pool/gate.go`):

- **Open → closed:** `Router.Stop` deletes the routing variable and the heartbeat. Runners that haven't started a job are cancelled, and none are spawned. Running jobs continue. Each poll while closed, runs with a job queued here are sent back with `Router.Rescue` (not forced), so a run with a job in progress waits for it, like `--max-wait`.
- **Closed → open:** `Router.Start` sets the routing variable and heartbeat again, and spawning resumes.
- **Watchdog:** routing and the heartbeat go off together, exactly as after `^C`. The watchdog's `if:` needs `vars.SPARECI_RUNS_ON`, so it's skipped while paused, and there is nothing for it to rescue. The heartbeat loop only writes while the router is on, so it can't turn routing back on during a pause.
- A failed routing change is retried on the next poll, and runs aren't rescued until routing is really off (a re-run made while it's on would come straight back). A probe that fails mid-run keeps its last reading, with one warning; at start, `spareci up` refuses a condition it can't measure.
- If the gate is closed at start, routing isn't started, and it's stopped instead in case a crash left it on.

The up process writes the gate's state to `<cache dir>/spareci/state/<owner>_<repo>.gate.json`, which `spareci status` reads while that process is alive.

## Background service

`spareci install` writes a launchd agent (`~/Library/LaunchAgents/dev.spareci.<owner>-<repo>.plist`) or a systemd user unit (`~/.config/systemd/user/spareci-<owner>-<repo>.service`) that runs `spareci up --repo owner/name` plus any flags given to `install`. It records the absolute path of the binary and the current `PATH`, so the service finds `gh`. It doesn't record tokens.

- Restarts: launchd `KeepAlive: {SuccessfulExit: false}`, systemd `Restart=on-failure`, each after 10 seconds. A crash or `kill -9` is restarted. A clean stop is not: SIGTERM from `launchctl bootout` or `systemctl stop` is handled like `^C`, and spareci exits 0.
- Logs: stdout and stderr are appended to `<cache dir>/service/spareci-<owner>-<repo>.log`. `spareci logs [-f]` tails it.
- `spareci uninstall` stops the service, deletes the file, and then does what `spareci down` does, in case the service was already dead.

## spareci init

`internal/workflows` parses each workflow with `go.yaml.in/yaml/v3` only to find where each job's `runs-on` key and value sit (line and column). The rewrite is a text edit of those lines, so comments, quoting and everything else in the file stay byte for byte. A double-quoted value stays double-quoted; a block list (`- label` per line) is folded onto the `runs-on:` line. Anything that isn't a plain label or list of labels is skipped with a reason rather than guessed at.

Fit: GitHub-hosted labels map to an OS and architecture (`ubuntu-*-arm` is arm64, `macos-13` and `*-intel`/`*-large` are x64, other `macos-14+` are arm64). Same OS and architecture is recommended; another architecture, a Linux job on a Mac or an unknown label can be chosen by name; Windows, macOS on Linux and container jobs on macOS can't.

## Job home and environment

`runner.ResolveJobHome` turns `--job-home` into a directory, and `runner.JobEnv` builds the runner's environment from spareci's own:

- `spareci` (default): `HOME` is `<cache root>/home/<owner>/<repo>`, created `0700` and kept between jobs. One per repo, so a job in one repo can't leave a `~/.gitconfig` or `~/.npmrc` that changes another repo's jobs.
- `user`: the environment passes through unchanged, as before this existed.
- An absolute path: used as `HOME`, with the same filtering as `spareci`.

With a separate home, only an allowlist of variables reaches the runner (`passEnv` in `internal/runner/home.go`: locale, `TMPDIR`, proxy and CA settings, `DEVELOPER_DIR`, `DOCKER_HOST`, `XDG_RUNTIME_DIR`, `PATH`). `PATH` entries inside the user's home are dropped, since tools there (rustup proxies, nvm shims) would look for their state in the real home. `GIT_CONFIG_NOSYSTEM=1` keeps the system gitconfig's keychain credential helper away from jobs.

No tool-specific variables are set. Go (`GOPATH`, `GOMODCACHE`, `GOCACHE`), npm, pip, cargo, rustup and the XDG defaults all derive from `HOME`, and the inherited overrides are dropped, so `setup-go`'s `go env GOMODCACHE` points into the job home. `TestJobEnvMovesGoState` checks this with the real Go toolchain.

Not covered: `ssh` and Xcode read the home directory from the user database, not `HOME`, and anything the user can read stays readable. See the README's "What a job can see" and [isolation.md](isolation.md).

## Local cache

`@actions/cache` (inside `actions/cache` and every `setup-*` action's `cache:` option) talks to GitHub's cache service v2:

- Twirp JSON RPCs at `$ACTIONS_RESULTS_URL/twirp/github.actions.results.api.v1.CacheService/`:
  - `CreateCacheEntry {key, version}` → `{ok, signedUploadUrl}`
  - `FinalizeCacheEntryUpload {key, version, sizeBytes}` → `{ok, entryId}`
  - `GetCacheEntryDownloadURL {key, restoreKeys, version}` → `{ok, signedDownloadUrl, matchedKey}`
- The archive upload goes to the signed URL through the Azure Blob SDK: one `PUT` up to 128 MB, otherwise `PUT ?comp=block` for each block, then `PUT ?comp=blocklist`.
- The download is a plain `GET` of the signed URL.

`internal/cache` implements those, stores archives under `~/Library/Caches/spareci/actions-cache` (`~/.cache/...` on Linux) and passes every other `/twirp/` call, such as artifacts, through to GitHub's results service.

**Isolation.** Each job gets its own listener on a random localhost port, bound to the job's repo and closed when the job ends. Upload and download URLs are HMAC-signed per listener. Branch scoping comes from the `ac` claim of the job's `ACTIONS_RUNTIME_TOKEN`: the write scope is the job's ref, and the extra read scopes are, for example, a PR's base branch. Lookup order matches GitHub's: own ref first, then exact key, key prefix, and restore-key prefixes, newest first.

**Pointing jobs at it.** The runner's worker sets `ACTIONS_RESULTS_URL` for every step, overwriting the environment. spareci keeps a second copy of the runner install in which the worker's UTF-16 string `ACTIONS_RESULTS_URL` is renamed to `ACTIONS_RESULTS_ORL` (same length). The worker then sets the renamed variable, and the value spareci puts in the runner's environment reaches the job. Other self-hosted cache servers use the same patch. If a runner release changes that string, the patch fails, spareci logs it and jobs use GitHub's cache. The unpatched install is used for `--cache=false`.

## Git mirror

`actions/checkout` starts from an empty directory every job (the runner's work directory is deleted with its clone), so every checkout downloads the commit's whole tree, or the whole history with `fetch-depth: 0`. A persistent machine can do better, as long as no workflow has to change.

**How.** `internal/gitmirror` keeps a bare mirror of the repo's branches and tags at `<cache root>/git/<owner>/<repo>.git`. `spareci up` creates it in the background, and each job's start fetches it in the background too (one fetch at a time; about 0.5s when there's nothing new). The runner takes several seconds to start, so the mirror usually has the job's commit before checkout asks for it. Once the mirror exists, jobs get `GIT_TEMPLATE_DIR` pointing at a git template with:

- `objects/info/alternates` naming the mirror's objects. `git init` copies it into every new repository, so the repository can read the mirror's objects and its fetch tells GitHub it already has them. The fetch still goes to github.com with the job's token, so access control, pushes, submodules and LFS are unchanged, and a commit the mirror doesn't have is simply downloaded.
- `config` with `core.alternateRefsPrefixes` set to the default branch. Without it git offers every branch and tag of the mirror, and its connectivity check after the fetch walks from all of them: on microsoft/vscode (6,400 refs) that check took 6s, more than the download it saved.
- `hooks/post-index-change`, which runs after the checkout writes the index. It copies the borrowed objects into the repository (`git repack -a -d`, local) and removes the alternates line. Afterwards the repository is self-contained, as on a hosted runner, so copying it into a container or a Docker build context works. If the hook doesn't run (a repository that is never checked out, or a global `core.hooksPath`), the repository keeps borrowing from the mirror, which still works on this machine.

Not used with `--job-home=user` (your own git config, which may set `core.hooksPath`, applies there) or `--git-mirror=false`. If the mirror can't be created or fetched, jobs don't get the template, or keep using the existing mirror, and fetch from GitHub as before. The mirror is fetched with the token spareci uses for the API, sent in a header, without your git config.

**Measured** (2026-11-10, Eventric, home connection; `SPARECI_BENCH_REPO=owner/name go test ./internal/gitmirror -run TestCheckoutTiming -v` runs checkout's own git commands with and without the mirror). "Checkout" includes the hook's repack.

| Repo | Fetch | Without mirror (fetch + checkout) | With mirror |
|---|---|---|---|
| cli/cli (12,600 commits) | `fetch-depth: 1` (default) | 1.9–2.2s | 1.0–1.1s |
| cli/cli | `fetch-depth: 0` | 5.5–6.2s | 1.6–2.2s |
| microsoft/vscode (171,000 commits, 98 MB tree) | `fetch-depth: 1` | 12.8–13.6s | 5.3–5.7s |

What remains with the mirror is writing the files and the repack, which grow with the tree, not the network. Creating the mirror took 7s for cli/cli and 2 minutes for vscode (1.7 GB on disk), once.

Live, on Row-Labs/spareci's CI `build` job (a small repo, so little to save): the checkout step's fetch took 0.95s without the mirror (run 42909976175) and 0.49s with it (runs 42909280041 and 42910181600), checkout plus repack 0.04s vs 0.27s, and the whole step 3.5s vs 2.8s.

## Docker

Jobs use the host's Docker daemon, so image layers and BuildKit's own cache persist between jobs on this machine, as on any self-hosted runner. Three things needed fixing:

- **CLI plugins.** `docker buildx` and `docker compose` are plugins in the user's `~/.docker/cli-plugins` (OrbStack and Docker Desktop link them there; Homebrew registers its directory in `cliPluginsExtraDirs`). A separate job home hid them: `docker buildx` was "unknown command". Each job start links them into the job home's `.docker/cli-plugins` (`runner.LinkDockerPlugins`). The user's `config.json`, with its registry credentials, is not copied.
- **Docker context.** The job home has no docker contexts, so the CLI uses `/var/run/docker.sock`. OrbStack and Docker Desktop create it; Colima doesn't. If `DOCKER_HOST` is unset, `spareci up` reads the user's current context's endpoint and gives it to jobs as `DOCKER_HOST` (`runner.DockerHost`).
- **BuildKit's `type=gha` cache** (`docker/build-push-action` with `cache-from/cache-to: type=gha`) uses the same cache service API as `actions/cache`, but failed the whole build on spareci, for two reasons. BuildKit runs in a container (the builder `setup-buildx-action` creates), or in the Docker VM on macOS, so `127.0.0.1` isn't spareci. And its client ([tonistiigi/go-actions-cache](https://github.com/tonistiigi/go-actions-cache)) reads the proto field names (`signed_upload_url`, `matched_key`, `entry_id`), while spareci sent only the lowerCamelCase ones `@actions/cache` reads. The server now sends both, and signs blob URLs with the host the client used. A `docker` shim, first on jobs' `PATH` when there is a docker to wrap, changes `ACTIONS_RESULTS_URL` for docker commands only while it is spareci's local cache: on macOS to the same listener as `host.docker.internal`, which the Docker VM forwards to the Mac's loopback (checked with OrbStack); elsewhere to GitHub's results service (`ACTIONS_RESULTS_ORL`, which the patched worker sets), as without spareci. Linux therefore still uses GitHub's cache for BuildKit; serving it locally there needs a listener on the Docker bridge.

**Verified live** on 2026-11-10 with a throwaway workflow (`docker-e2e`, run 42909412143) on this Mac with OrbStack: `setup-buildx-action@v3` and `build-push-action@v6` with `cache-to: type=gha,mode=max` in one job, then a second job with a fresh builder and `cache-from`. The second job's `RUN` step was `CACHED` and produced the same file as the first, and spareci logged the BuildKit blobs saved and then hit locally. `SPARECI_TEST_DOCKER=1 go test ./internal/runner -run TestDockerBuildxGHACache -v` does the same locally against a real Docker.

## Other ideas

- **Tool cache.** Already shared: `RUNNER_TOOL_CACHE` is `<cache root>/toolcache` for every job, so `setup-node`, `setup-go` and friends download a version once.
- **Persistent work directory** (keeping `_work` between jobs, as a plain self-hosted runner does) would make checkouts incremental too, but concurrent jobs would share it and leftovers would leak between jobs. The mirror gets most of the gain without that.

## Estimate

`spareci estimate` lists runs with `GET /repos/{repo}/actions/runs?created=FROM..TO`, then each run's jobs with `GET .../runs/{id}/jobs?filter=all`, so re-run attempts count too. GitHub returns at most 1,000 runs for a filtered listing, so a busier window is split in half until each part fits. The run-timing endpoint is no use here: it now reports 0 ms for every job.

Each job that got a runner and finished is billed `ceil(completed_at - started_at)` minutes, at least 1. Jobs with no runner (skipped, or failed before starting) cost nothing. Where a job ran:

- **Self-hosted**: a `self-hosted` label, or a runner not named `GitHub Actions N` outside runner group 0.
- **Larger runner**: a label giving the size (`ubuntu-8-core`, `windows-arm-16core`). The API doesn't report a larger runner's machine size on the job, so other names are shown unpriced.
- **Standard runner**: a GitHub image label (`ubuntu-24.04`, `macos-15`, `windows-11-arm`, `macos-15-xlarge`, ...). The API sometimes returns a null runner group for these.

It checks `GET /rate_limit` before fetching jobs (one request per run) and stops if that would leave fewer than 100 calls. Requests go through the client's ETag cache and up to four run at once; a 5xx on a jobs request is retried twice.

## Stats

When a runner prints `Running job:`, `stats.Recorder` starts looking for the job on GitHub: it lists in-progress runs (and recent ones) and their jobs, and picks the job whose `runner_name` is this runner's name, which is unique. Doing this while the job runs means its run is still in progress and near the top of the list. When the runner exits:

1. `GET /repos/{repo}/actions/jobs/{id}` until the job shows `completed` (GitHub sets `completed_at` a moment after the runner exits). Its `started_at` and `completed_at` are what GitHub bills by, so they replace this machine's clock.
2. `GET .../runs/{run_id}` for the workflow file's path; the job gives the commit (`head_sha`).
3. `GET /repos/{repo}/contents/{path}?ref={sha}`, parsed by `internal/workflows`, cached per (path, sha) for the life of the process. The job GitHub names (`build`, `Unit tests (20)` for a matrix entry, a `name:` with expressions) is matched to its key; two jobs that could match is an error, not a pick.
4. The fallback is read from the two `runs-on` forms `spareci init` writes, `vars.X || 'label'` and `vars.X || fromJSON('[...]')`. Anything else is recorded with the reason.
5. One JSON line is appended to `<cache dir>/stats/jobs.jsonl` with `O_APPEND` in a single write, so several `spareci up` processes share the file. `spareci stats` skips (and counts) a line a crash cut short.

If GitHub can't be reached, the job is still recorded, with this machine's times and no fallback, so it shows as not counted rather than disappearing. `^C` waits up to a minute for jobs still being recorded.

Pricing happens when `stats` runs, not when the job is recorded, through `estimate.ForLabels` and `estimate.Minutes`, the same code `estimate` bills with; `TestPriceMatchesEstimate` checks that the two agree.

Verified live on 2026-11-10: `spareci up` ran CI run 42906945251 (`build`, `cache-save`, `cache-restore`) and recorded all three with GitHub's times, job IDs and the `ubuntu-24.04` fallback read from `ci.yml` at the run's commit, priced as Linux 2-core: 3 min, $0.018.

## Testing

- `go test -short ./...`: unit tests (including the git mirror against local repositories and the docker shim), including the cache protocol over HTTP, the lease and rescue logic against a fake GitHub, the rendered launchd and systemd files, and `init`'s rewrites (table tests over realistic workflows in `internal/workflows/testdata`).
- `go test ./...` with `npm ci --prefix test/compat` done first: also runs the real `@actions/cache` npm client against the server (`test/compat/compat.mjs`), covering both upload paths, restore keys, misses and immutability.
- CI (`.github/workflows/ci.yml`) runs on spareci when it's up, and on GitHub-hosted runners otherwise. Its `cache-save` → `cache-restore` jobs check a real save and restore through `actions/cache`, plus artifact pass-through. On self-hosted runners they also assert that the cache really was local.
