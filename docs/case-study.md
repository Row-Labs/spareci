# Case study: Commission's macOS CI bill

spareci exists because of one repo. [Commission](#about-the-numbers) ships a macOS app, and its CI ran on GitHub's `macos-15` runners. This is what that cost, what spareci changed, and what it didn't. Every number below comes from GitHub's API or from job logs, checked on 2026-11-10, and the run IDs are given so they can be checked again.

## The bill

`spareci estimate` prices every job a repo ran the way GitHub bills it. Over the 30 days to 2026-11-10, Commission's repo (`Row-Labs/relay`, its old name) looked like this:

```
$ spareci estimate --repo Row-Labs/relay --days 30
Row-Labs/relay · Oct 11 – Nov 10 (30 days) · 225 runs, 806 jobs

  RUNNER                   JOBS   MINUTES           COST  THIS MACHINE
  macOS standard            526     2,091           $130  yes
  Linux 2-core              279       969          $5.81  no
  Self-hosted                 1         7          $0.00  already
  Total, GitHub-hosted              3,060           $135
```

These are list prices ($0.062 a minute for macOS, $0.006 for Linux) before the plan's included usage comes off. The repo's first run was on 2026-10-18, so that's about 23 days of activity, not 30.

macOS was 68% of the minutes and 96% of the money. Broken down by job:

| Workflow | Job | Runner | Jobs | Billed minutes | Median length |
|---|---|---|---|---|---|
| CI | `checks` | macOS | 127 | 789 | 6.1 min |
| Release | `desktop-macos` | macOS | 63 | 466 | 6.9 min |
| Release | `checks` | macOS | 36 | 238 | 6.0 min |
| CI | `tauri` | macOS | 130 | 223 | 1.4 min |
| Release | `bundles-macos` | macOS | 64 | 209 | 2.3 min |
| CI | `mobile` | macOS | 106 | 166 | 1.1 min |
| Release | `bundles-linux`, `publish`, CI `changes` and others | Linux | 279 | 969 | |

A typical pull request run (`checks`, `tauri` and `mobile` on macOS) billed 10 macOS minutes, about $0.63 at list price (median of 90 successful CI runs before 2026-11-10). The repo was getting busier: 28 macOS `checks` jobs on 2026-11-09 alone, against 2 to 5 a day in late October.

## The first run on spareci

The spike ran Commission's `checks` job on an Apple Silicon Mac with spareci (then called `homerun`), changing nothing in the workflow but the `runs-on` line. The job installs dependencies with pnpm, then typechecks, lints, checks formatting, runs the unit and contract tests and builds the web app. It passed: run 42856146332, 2026-11-10. This was the spike build, before spareci had its local cache, so the job saved its cache to GitHub's cache service like any self-hosted runner.

To compare, here are the 37 successful `checks` jobs on GitHub's `macos-15` from 2026-11-08 to 2026-11-10:

| Step | GitHub `macos-15` (median of 37) | spareci spike (run 42856146332) |
|---|---|---|
| Whole job | 6m40s (range 5m45s to 7m57s) | 6m37s |
| Install | 22s | 19s |
| Unit and contract tests | 4m35s | 3m35s |
| `setup-node` post step (saving the pnpm cache) | 24s | **1m21s** |

The Mac did the real work faster: the tests took a minute less than on GitHub's runner. It then gave almost all of that back saving the pnpm store to GitHub's cache. The job came out at the same length as a hosted one, so spareci was cheaper (that job cost $0 instead of about $0.43) but no faster.

## The finding: GitHub's cache was the bottleneck

On a GitHub-hosted runner, `actions/cache` uploads to storage in the same cloud, so saving a few hundred megabytes takes seconds. A self-hosted runner uploads over whatever connection it has, and downloads come back the same way. The spike's sandbox repo showed the same thing: `setup-go` spent 59s saving its cache and 81s restoring it, in a job whose real work took 3s.

So spareci now runs its own cache server on the machine. `actions/cache` and every `setup-*` action's `cache:` option talk to it instead of GitHub, and the archives stay on local disk. It follows GitHub's rules: keys are immutable, a pull request can read its base branch's caches but not write them, and old entries are evicted past a size limit. [How it works](how-it-works.md#local-cache) has the protocol details.

These cache numbers come from spareci's own CI (`Row-Labs/spareci`), not from Commission. It shows the difference on one 1.19 GB Go module and build cache, saved from the same Mac:

| | GitHub's cache (run 42863196283) | Local cache (run 42865164223) |
|---|---|---|
| Uploading 1.19 GB | 31s, averaging about 36 MB/s with stalls | 2s, at about 510 to 570 MB/s |
| Whole `setup-go` post step | 60s | 28s |

What's left in the 28 seconds is `tar` and zstd compressing 1.1 GB, which a local cache doesn't remove. Smaller caches show the transfer cost going to nothing: in later runs, 29 MB and 34 MB caches were written at about 1 GB/s (runs 42886931380 and 42904037716).

Commission's `checks` job hasn't been run on spareci with the local cache yet, so this case study has no measured "after" number for it. The 1m21s it spent saving the pnpm cache is the part the local cache targets.

## What Commission did first: moved what didn't need a Mac

The estimate made one thing obvious that had nothing to do with spareci: most of those macOS minutes didn't need macOS. `checks` is TypeScript tooling, tests and a web build. On 2026-11-10, Commission moved CI's `checks` and `mobile` jobs and the release `checks` gate to `ubuntu-24.04`. It split the tests into five parallel jobs and dropped the `tauri` job (the desktop Rust checks, which are mostly macOS-only) from CI, running it locally instead.

That's the cheapest fix for any macOS bill, and it should come before spareci. Linux minutes cost a tenth as much. The first CI run after the change (42872824005) billed 33 Linux minutes, $0.20, against a median of $0.63 for the macOS runs before it. It's one run, so treat it as a first reading rather than an average.

What still needs a Mac is the release build. The `desktop-macos` and `bundles-macos` jobs compile native code for macOS and build the signed app. Over the period they billed 632 minutes in 51 successful releases (675 counting failed and cancelled runs), about $42 at list price. That is the part spareci can take.

## What it costs to run, and what you give up

spareci is free and the Mac was already there, so the direct cost is the machine's power and wear. We haven't measured the power. The real costs are trade-offs:

**One machine runs a few jobs at a time.** GitHub starts a fresh runner for every job. In the period, Commission's macOS jobs overlapped up to 15 at once, though for 58% of the time that any macOS job was running only one was, and for 93% three or fewer were. `spareci up --max 2` runs two jobs at once, which covers Commission's two macOS release jobs running side by side. A job that has waited longer than `--max-wait` (10 minutes by default) is sent back to GitHub-hosted runners, so a busy machine makes CI slower but doesn't block it.

**The machine has to be there.** spareci routes jobs to itself with a repository variable, and the workflow falls back to `macos-15` whenever that variable is unset. When `spareci up` stops cleanly, it clears the variable and sends jobs still queued for the machine back to GitHub. When the machine dies without warning, an optional watchdog workflow notices that the heartbeat has stopped and does the same. Expect that within 15 to 20 minutes. It was tested on spareci's own repo: with a fresh heartbeat it left routing alone (run 42893719071). After a `kill -9` it cleared routing and re-ran the stranded run on GitHub (42895780440 rescuing 42894946226). While spareci is healthy, the watchdog's runs are skipped and cost nothing.

**It can share a laptop.** On a machine you also use, `--only-on-ac`, `--idle 10m` and `--hours 18:00-08:00` limit when it takes jobs. When a condition stops holding, new jobs go to GitHub and running jobs finish.

**The security model is your user account.** Jobs run on the host as your user, with their own home directory and a trimmed environment. That keeps them out of your shell config and tokens, but not out of your files, SSH keys or Docker. That's acceptable for a private repo whose workflows and contributors you trust, which describes Commission, and not for anything else. Don't point it at a public repo. A release job that signs an app needs signing credentials on that machine, so moving Commission's release builds is a decision about where those credentials live as much as about cost. Per-job VM and container isolation is designed ([isolation.md](isolation.md)) but not built.

## Where it stands

- Commission's macOS spend over the period was about $130 at list price. Moving work that didn't need a Mac to Linux removed most of it, without spareci.
- The macOS work that remains, the release build, is about $42 a month at list price at the current release rate. spareci can run it for nothing on a Mac that's already there, if the owner is happy to keep signing credentials on that machine.
- On the one Commission job measured so far, a Mac running spareci did the work faster than GitHub's `macos-15` but lost the gain to GitHub's cache. spareci's local cache cuts a 1.19 GB upload from 31s to 2s on its own CI. It hasn't been measured on Commission's jobs yet.

## About the numbers

Commission is a supervisor for Claude Code sessions, built by the author of spareci. Its repository is private, so readers can't open its run IDs, but they're the ones the numbers were read from.

Where each number comes from:

- **Commission's repo (`Row-Labs/relay`), GitHub-hosted runs:** the bill, the per-job breakdown, the `macos-15` baseline, the concurrency figures, the release build minutes and the first Linux CI run (42872824005).
- **The spareci spike on Commission's repo:** the one `checks` run on spareci (42856146332), which used GitHub's cache, not the local one.
- **The spike's sandbox repo** (since deleted): the 59s and 81s `setup-go` cache timings, as recorded in the spike notes.
- **spareci's own CI (`Row-Labs/spareci`):** every local cache measurement (42863196283, 42865164223, 42886931380, 42904037716) and the watchdog runs (42893719071, 42895780440, 42894946226).
 Billed minutes round each job up to the whole minute, as GitHub does. Step timings come from the jobs API's step timestamps, and cache transfer rates from the `actions/cache` log lines. Prices are GitHub's list prices as of 2026-11-10, from [GitHub's pricing page](https://docs.github.com/en/billing/reference/actions-runner-pricing).
