# Spike results (2026-11-10)

Machine: Eventric (Apple Silicon Mac), actions/runner 2.338.2.

The spike ran under the working name `homerun`; renamed to spareci afterwards.

## What was proven

- **Sandbox** (`blaze-rowland/homerun` CI, since deleted): push → job queued → runner up → job running in 9s. Passed; runner dir and registration cleaned up.
- **Relay** (`Row-Labs/relay` CI `checks` job, dispatched on branch `homerun-spike`): the full pnpm install / typecheck / lint / tests / web build passed on this Mac without changing the workflow other than the `runs-on` line.
- **Routing:** the routing variable (then `HOMERUN_RUNS_ON`) was set while up and cleared on `^C`, so jobs fall back to hosted runners.

## Relay `checks` timing

| Where | Duration |
|---|---|
| GitHub `macos-15` (last 3 runs) | 6m50s – 7m18s |
| GitHub `ubuntu-24.04` (latest run) | 4m46s |
| spareci on Eventric | 6m33s |
| ...of which `setup-node` cache upload to GitHub | 1m21s |

## Findings

1. **GitHub's cache service is the bottleneck on self-hosted runners.** In the sandbox, `setup-go` cache save took 59s and restore 81s for a job whose real work took 3s. Relay spent 81s uploading the pnpm store. A local cache server that the runner talks to instead (via `ACTIONS_RESULTS_URL`) would remove that time and make spareci faster than hosted runners, not just cheaper. **Next feature.**
2. JIT runners cold-start in about 8s (clone + connect). Fine.
3. Jobs run on the host as the user. That's acceptable for your own private repos and unacceptable for anything else. Next: Tart VMs on macOS, containers on Linux.
4. If the process is killed without `^C`, the variable stays set and jobs queue for an offline machine until `spareci down`. Fix: a launchd/systemd service plus a heartbeat (e.g. the variable carries an expiry that a tiny hosted job checks), or the scale-set long-poll API.

## Next steps (in order)

1. Local actions/cache server (finding 1).
2. `spareci install`: launchd/systemd service, survives reboot, crash-safe routing.
3. Multiple repos / org-level runners from one daemon.
4. Isolation: Tart on macOS, Docker on Linux.
5. Fleet: one coordinator, many machines (Commission already knows the machines).
