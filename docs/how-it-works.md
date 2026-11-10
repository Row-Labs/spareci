# How spareci works

## Layout

| Package | Job |
|---|---|
| `cmd/spareci` | CLI: flags, signals, wiring |
| `internal/gh` | The few GitHub REST calls needed: queued jobs, JIT runners, repo variables |
| `internal/pool` | Poll loop: decides how many runners to start and drains on shutdown |
| `internal/runner` | Installs `actions/runner`, clones it per job, runs it, patches the worker for the cache |
| `internal/cache` | Local implementation of GitHub's cache service |

## Job lifecycle

1. `pool` polls `GET /repos/{repo}/actions/runs?status=queued|in_progress` and their jobs, keeping queued jobs whose labels are all ones this machine carries.
2. For each job not already covered by a starting runner, it calls `POST /repos/{repo}/actions/runners/generate-jitconfig`. That registers a runner that accepts exactly one job.
3. `runner` clones the cached runner install (APFS `clonefile` on macOS, `--reflink=auto` on Linux, so it's instant and uses no disk), writes `.env`, and runs `run.sh --jitconfig ...`.
4. GitHub assigns a matching job, which is not necessarily the one that triggered the spawn, and that's fine, because the counts balance out. The runner prints `Running job: <name>`, then exits when the job ends.
5. The clone is deleted. If the runner never got a job, its registration is deleted too.

Routing: on start, spareci sets the repo variable `SPARECI_RUNS_ON` to its label, so `runs-on: ${{ vars.SPARECI_RUNS_ON || 'ubuntu-24.04' }}` resolves to this machine. On `^C`, it deletes the variable first, so new jobs go to GitHub, then waits for running jobs.

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

## Testing

- `go test -short ./...`: unit tests, including the cache protocol over HTTP.
- `go test ./...` with `npm ci --prefix test/compat` done first: also runs the real `@actions/cache` npm client against the server (`test/compat/compat.mjs`), covering both upload paths, restore keys, misses and immutability.
- CI (`.github/workflows/ci.yml`) runs on spareci when it's up, and on GitHub-hosted runners otherwise. Its `cache-save` → `cache-restore` jobs check a real save and restore through `actions/cache`, plus artifact pass-through. On self-hosted runners they also assert that the cache really was local.
