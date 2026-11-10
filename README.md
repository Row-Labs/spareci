# homerun

Run your GitHub Actions jobs on a machine you already own. No Kubernetes, no Terraform, no cloud account.

```sh
homerun up --repo owner/name
```

Then change one line in a workflow:

```yaml
runs-on: ${{ vars.HOMERUN_RUNS_ON || 'macos-15' }}
```

While `homerun up` runs, it sets the `HOMERUN_RUNS_ON` repo variable, so those jobs come to your machine. When you stop it, it clears the variable and they go back to GitHub-hosted runners. CI never sits waiting on a laptop that's asleep.

## How it works

- Polls the repo for queued jobs whose `runs-on` labels this machine carries (`self-hosted`, OS, arch, `homerun`).
- For each one, registers a single-use just-in-time runner (`generate-jitconfig`) and starts GitHub's real `actions/runner` in a copy-on-write clone of a cached install. Logs, checks, artifacts and caches report to GitHub as normal.
- The runner takes one job and exits. Its directory is deleted afterwards.
- The tool cache (`setup-node`, `setup-go`, ...) is shared across jobs, so toolchains download once.
- Auth comes from `GITHUB_TOKEN` or whatever `gh` is logged in as. Registering runners needs admin on the repo.

First `^C` stops taking jobs, routes back to GitHub and lets running jobs finish. A second `^C` aborts them. After a crash, run `homerun down` to clear the routing variable and stale runners.

## Spike limits

- Jobs run directly on the host as your user. Don't point it at a public repo, or at any repo whose workflows you don't trust. VM isolation (Tart on macOS, containers on Linux) is next.
- If the process dies without `^C`, the variable stays set until `homerun down`.
- One repo per process. Polling uses ETags, so idle polls mostly don't count against the rate limit.
