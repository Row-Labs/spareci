# Linux jobs in containers

A Mac takes Linux jobs as well as macOS ones: macOS jobs run natively as before, and each Linux job runs in a fresh container. A Linux machine runs its Linux jobs in containers too. This page covers the container side (`internal/runner/container.go`). Which jobs go where is per-OS routing (see [how-it-works.md](how-it-works.md)).

## How a job runs

1. **Prepare**, once per `spareci up` and architecture: check for a runtime, build the image if this version of it isn't there, and start the runner binary once in it to prove the platform works. Then remove containers and volumes a crash left behind ([Several repos on one machine](how-it-works.md#several-repos-on-one-machine) says whose).
2. **Per job**: write the JIT config and the job's environment to a `0600` env file (secrets stay off docker's command line), then `docker run` a container named after the runner. Its output is the runner's, parsed exactly as on the host. When the runner exits, the container (`docker rm -f -v`) and its work volume are removed.
3. **Stop**: `docker stop` sends `SIGINT` (`--stop-signal`), which the runner treats as "finish up and deregister", then `SIGKILL` after 30 seconds (`--stop-timeout`). `tini -g` is PID 1, so the signal reaches the runner's whole process group. Nothing else signals the container: `docker run` has `--sig-proxy=false` and its own process group, so a `^C` meant for spareci (which drains) doesn't reach the job.

Inside, an entrypoint running as root chooses the runner install, copies it to `/home/runner/actions-runner`, starts the docker proxy if the runtime's socket is mounted (below), and starts `run.sh --jitconfig` as the `runner` user (uid 1001, passwordless sudo, as on GitHub's images). The JIT config is removed from the environment first, so steps don't see it.

### Paths are the same in every job

Every job of a repo sees the same paths, as on a hosted runner, where they are `/home/runner/work/<repo>/<repo>` with HOME on the same disk:

| | Path |
|---|---|
| `HOME` | `/home/runner` |
| Workspace (`GITHUB_WORKSPACE`) | `/home/runner/actions-runner/_work/<repo>/<repo>` |
| `pnpm store path` | `/home/runner/.local/share/pnpm/store/v<N>` |
| `npm config get cache` | `/home/runner/.npm` |
| `go env GOMODCACHE` | `/home/runner/go/pkg/mod` |
| Tool cache | `/opt/hostedtoolcache` |

This matters for caches: `actions/cache` makes a cache entry's version from its paths, so a path that changes from job to job never restores. pnpm puts its store at the root of the filesystem that holds the project when HOME is on another one, and `setup-node` caches that path. When each job's workspace was a volume mounted at its own daemon path (`/var/lib/docker/volumes/spareci-job-<runner>/_data`), the store path was different in every job and the pnpm cache never hit. Now the job's volume is mounted at `/home/runner`, so HOME and the workspace share a filesystem and every path is fixed. Concurrent jobs are still isolated: each has its own container and its own volume at that path. Measured with `TestContainerPathsE2E` (two jobs at once).

What persists between jobs:

| | Where | Why |
|---|---|---|
| Tool cache | volume `spareci-toolcache-<owner>_<repo>-<arch>` at `/opt/hostedtoolcache` | `setup-go`, `setup-node` and friends download a version once. Same path as GitHub's images: `setup-python`'s builds only work there. |
| Git mirror | the host's mirror and its template, bind-mounted read-only at their own paths | Checkouts borrow objects exactly as on the host. The template's hook copies them in after checkout. |
| Local cache | the host's cache server | Reached as `host.docker.internal` (below). |

Everything else, `HOME` included, starts fresh each job (from the image's `/home/runner`), like a hosted runner.

## Runtime

Any Docker-API runtime the `docker` CLI reaches, through `DOCKER_HOST` or the current docker context: OrbStack (tested), Docker Desktop, Colima, Docker Engine and rootless Docker on Linux. spareci runs the CLI with its own environment, so the user's context applies.

**No runtime.** `Prepare` fails with `runner.ErrNoRuntime` when there is no `docker` CLI, the daemon doesn't answer within 15 seconds, the daemon runs Windows containers, or a container for the architecture won't start (no Rosetta or QEMU for x64). The machine then takes no Linux jobs. They stay with GitHub-hosted runners rather than being taken and failed.

### Apple's `container` (macOS 26): not yet

[apple/container](https://github.com/apple/container) runs each container in its own lightweight VM through Virtualization.framework. That is a stronger boundary than a shared Docker VM, and the reason to come back to it for public repos. It isn't supported yet, because:

- **It isn't the Docker API.** It has its own CLI and no Docker socket, so it needs a second driver behind the `Docker` interface, with different flags for run, volumes, build and stop.
- **No Docker inside jobs.** Without a daemon to pass through, `container:` jobs, `services:` and `docker build` steps can't work (Docker-in-Docker would need a privileged nested daemon in each VM).
- **Reaching the local cache.** There is no `host.docker.internal` forwarding to the Mac's loopback. The cache would have to listen on the vmnet gateway address, which only exists while a container runs.
- **Not measured.** It isn't installed on the test Mac. Start-up per job (a VM boot) has to be measured against a container start before it can be the default.

The `Docker` interface is small (run to completion, stream a run), so a `container` driver for jobs that don't need Docker is a contained piece of work once those are answered.

## Image

`internal/runner/image/Dockerfile`, embedded in the binary and built locally on first use: `docker build --platform linux/<arch>` with the Dockerfile and entrypoint as a tar context on stdin. Nothing is pulled except `ubuntu:24.04` and the packages, and nothing is pushed to any registry.

- **Tag:** `spareci-runner:<runner version>-<arch>-<hash of the Dockerfile, entrypoint and proxy source>`. A new runner version or a changed Dockerfile is a new tag, which triggers a build. Older spareci images for that architecture are removed after a successful build.
- **Size:** 1.7 GB (arm64), 2.0 GB (x64).
- **Cold build on an M-series Mac with OrbStack:** about 1m10s arm64, about 2m45s x64 (Rosetta). A rebuild after a small change reuses Docker's layer cache: under a minute.
- **Contents:** Ubuntu 24.04; actions/runner for the architecture with its dependencies (`installdependencies.sh`); git, git-lfs, curl, wget, jq, unzip, zip, xz, zstd (`actions/cache` uses it), bzip2, rsync, openssh-client, build-essential, pkg-config, python3 with pip and venv, sudo, locales (`LANG=C.UTF-8`), tzdata, the docker CLI with buildx and compose, the `gh` CLI, basic network tools, and `spareci-dockerproxy` (built in a `golang` stage from `image/dockerproxy/main.go`, for the target from the build machine's platform). `ImageOS=ubuntu24` is set, as some `setup-*` actions read it.
- **The patched worker:** `/opt/runner-cache` is a hard-linked copy of `/opt/runner` whose `Runner.Worker.dll` has the same `ACTIONS_RESULTS_URL` rename as on the host ([how-it-works.md](how-it-works.md#local-cache)). The entrypoint uses it only when the job has the local cache. If a runner release drops the string, the image has no patched copy and jobs use GitHub's cache, as on the host.

### Missing compared with GitHub's `ubuntu-24.04`

Node.js (the latest release of an LTS line, currently 24) **is** on PATH, as on GitHub's image: some actions run before `setup-node` and need a `node` binary, `pnpm/action-setup` among them. Jobs that need a specific version still use `setup-node`.

GitHub's image is tens of GB with hundreds of tools. Jobs that use `setup-*` actions work the same here. Jobs that assume a preinstalled tool fail with "command not found" until they add the setup step. Not included:

- **Language runtimes on PATH:** Go, Java, .NET, Ruby, PHP, Rust (rustup), Swift, Kotlin, Julia, Haskell, and the pre-populated tool cache. Use `setup-node`, `setup-go`, `setup-java`, `dtolnay/rust-toolchain` and so on. The tool cache volume keeps them after the first job.
- **Package managers:** yarn, pnpm, pipx, Homebrew, conda.
- **Compilers and build tools:** clang/LLVM, gfortran, cmake, ninja, bazel.
- **Browsers:** Chrome, Firefox, Edge and their drivers. Playwright and Puppeteer install their own.
- **Databases:** PostgreSQL and MySQL servers and clients, SQLite CLI. Use `services:` or install them.
- **Cloud and infrastructure CLIs:** aws, az, gcloud, kubectl, helm, terraform, packer, ansible.
- **Other container tools:** podman, buildah, skopeo.
- **System:** systemd isn't running, so `systemctl` steps fail, as in any container. There's no swap file, and `/mnt` is not a second disk.
- **Resources:** a standard hosted Linux runner has 4 vCPUs and 16 GB. A container gets whatever its limits allow (below).

## Architecture

Apple Silicon runs arm64 containers natively and x64 containers under Rosetta (`--platform linux/amd64`; OrbStack and Docker Desktop turn Rosetta on by default). `runner.ContainerArches()` lists what a machine offers, native first: `[arm64 x64]` on Apple Silicon, `[arm64]` on an arm64 Linux machine, `[x64]` on an x64 one (QEMU arm64 is too slow for CI). Each architecture is its own `Container` backend with its own labels (`self-hosted, Linux, ARM64` or `X64`).

**Measured** on an M-series Mac with OrbStack (2026-11-11): this repo's own build steps in each image, with Go downloaded first (about 2.8s either way):

| Step | arm64 | x64 (Rosetta) |
|---|---|---|
| `go vet ./...` (cold build cache) | 7.3s | 45.5s |
| `go test -short ./...` (after vet) | 2.7s | 16.9s |
| `go test -short -count=1 ./...` (warm) | 1.1s | 6.7s |
| `go build ./cmd/spareci` | 0.4s | 1.1s |

Rosetta costs about 6x on compiler-heavy work like this. GitHub's whole `build` job takes about 30s (25–41s over 14 runs).

**Live**, `spareci up --repo Row-Labs/spareci --max 3` on the same Mac (2026-11-11), dispatching `ci.yml` on main. Each pass ran twice: the first run with an empty tool cache volume and no `setup-go` cache for that architecture, the second warm. Times are GitHub's, from job start to end, in seconds. Hosted is the latest GitHub-hosted run of the same jobs (43100842127) and the range over 14 earlier ones.

| Job | x64 (Rosetta, `--linux-x64-on-arm=false`), cold / warm | arm64 (default), cold / warm | Hosted `ubuntu-24.04` |
|---|---|---|---|
| `build` | 108 / 37 | 33 / 14 | 21 (25–41) |
| `cache-save` | 13 / 12 | 7 / 8 | 10 (5–10) |
| `cache-restore` | 9 / 12 | 7 / 9 | 5 (5–15) |

Runs 43101146581 and 43101489980 (x64), 43101737090 and 43101874814 (arm64), all green. In each run, `cache-save` saw `ACTIONS_RESULTS_URL=http://host.docker.internal:<port>/` and saved to the local cache, `cache-restore` hit it, and `setup-go`'s module and build cache was saved locally and hit in the warm run. After each job, its container and work volume were gone (`docker ps -a` and `docker volume ls` filtered on spareci's labels were empty).

Where the time goes in the warm `build` (seconds; x64 / arm64 / hosted): setting up the job 5 / 3 / 1, `setup-go` 1 / 1 / 10 (hosted downloads its cache from GitHub), `go vet` 4 / 0 / 1, `go test` 11 / 3 / 4. A job here pays a few seconds more than hosted to start (container, runner copy, runner connection), which is why the short cache jobs are slightly slower, and gains on anything cached locally or compiled natively.

**Recommendation.** Run jobs whose fallback is `ubuntu-24.04` (x64) on arm64 containers by default. Most CI (interpreted languages, Go, Rust, Node builds, tests) doesn't depend on the CPU architecture, and arm64 is several times faster here. Use x64 only for jobs that need it: prebuilt x64-only binaries, x64 artifacts built natively rather than cross-compiled, or tests of architecture-specific code. This is the default: `--linux-x64-on-arm` is on, so an arm64 machine runs Linux x64 jobs as arm64. On a Mac that means only the arm64 container backend is made, and the x64 image isn't built. Warm arm64 beat GitHub-hosted on this repo's build (14s against 21s), while x64 under Rosetta took 37s, and 108s cold. `spareci init` recommends `ubuntu-*` jobs on such a machine with the note "runs as arm64 here".

The cost is correctness for the jobs that do care. A job that downloads an x64-only binary fails with "exec format error". A job that builds a native x64 artifact and uploads or releases it produces an arm64 one instead. For those, either leave the job unconverted (it keeps running on GitHub's `ubuntu-24.04`), or opt the machine out with `--linux-x64-on-arm=false`. A Mac then builds the x64 image and runs those jobs under Rosetta: correct, but several times slower.

## Jobs that need Docker

`container:` jobs, `services:`, Docker actions and `docker build` steps all need a Docker daemon. The options:

| | Works | Cost |
|---|---|---|
| **Pass the runtime's socket through (default, `DockerSocket`)** | `docker build`, `docker run`, `container:` jobs and `services:` reached by name from them, Docker actions | A job controls the runtime: it can start a privileged container or mount the daemon host's files. On a Mac, that is the Docker VM, and OrbStack and Docker Desktop share the Mac's home directory with it. So it is **not isolation**: a job can do about what a native job on the host can, and no more. |
| Refuse (`DockerNone`) | Everything else | Steps using docker fail with "Cannot connect to the Docker daemon". spareci can't refuse such jobs up front, because a JIT runner takes whichever matching job GitHub hands it. `spareci init` already won't route `container:` jobs to a Mac. |
| Docker-in-Docker (`--privileged`) | Everything | A privileged container is root on the Docker VM anyway: no safer than the socket, and slower. |

The socket is the default because spareci already runs jobs with the user's own Docker on the host (see the README's Security section), so it adds no new exposure for the private, trusted repos spareci supports today. Public repos need real isolation first ([isolation.md](isolation.md)). Use `DockerNone` there, or a VM backend.

**Paths.** The runner bind-mounts paths into the containers it starts: the workspace into a `container:` job (`/__w`), its node (`/__e`) and the tool cache (`/__t`) into it, and steps do their own `docker run -v $PWD:...` or `docker compose` with `./:/app`. With the socket passed through, the daemon resolves those paths on its own host, not in the job's container. Each job gets a volume, `spareci-job-<runner>`, mounted at `/home/runner`; on the daemon's host it is at `<DockerRootDir>/volumes/<name>/_data` (from `docker info`), a different path for every job.

So jobs don't get the socket itself. It is mounted at `/run/spareci/docker.sock`, and `spareci-dockerproxy` (`internal/runner/image/dockerproxy`, built into the image) listens at `/var/run/docker.sock` in front of it. It passes the Docker API through unchanged, except that in `POST /containers/create` (`HostConfig.Binds`, bind `HostConfig.Mounts`) and bind volumes (`POST /volumes/create`, `device`), sources under `/home/runner` are rewritten to the job's volume on the daemon, and `/opt/hostedtoolcache` to the tool cache volume. Because it works at the API, it covers the runner, the docker CLI, `docker compose` and SDKs (testcontainers, docker-py) alike. Attach and exec connections are passed through as raw streams, half-close included, so `docker run -i` gets its stdin EOF. `docker inspect` still shows the daemon-side paths. A path outside those two (`-v /tmp/x:/x`) is resolved on the daemon's host, as before.

The entrypoint copies the runner into `/home/runner/actions-runner` (1.5–2.2s per job), in both docker modes, so the workspace is at the same path in both.

**The socket.** On macOS it is `/var/run/docker.sock` inside the runtime's VM, which is where OrbStack, Docker Desktop and Colima put it. On Linux it is the socket the CLI uses (`DOCKER_HOST` or the current context, so rootless Docker works). A daemon reached over TCP gets no passthrough: jobs run without docker, with a warning.

**Known gap: `services:` on a job without `container:`.** The runner publishes service ports on the daemon's host. A hosted runner reaches them as `localhost:<port>`, but in the job's container `localhost` is the container itself. Jobs that use `container:` reach services by name and work. Fixing this needs the job's container on the daemon host's network, which would also expose the host's local services to the job.

## Local cache

The cache server listens on the host's `127.0.0.1`, one port per job. From a container on macOS, `host.docker.internal` reaches the Mac's loopback (OrbStack and Docker Desktop forward it; checked with OrbStack in #14). So the container backend rewrites `ACTIONS_RESULTS_URL` and `SPARECI_RESULTS_URL` from `127.0.0.1` to `host.docker.internal`. The cache server signs blob URLs with the host the client used, so uploads and downloads follow. BuildKit's `type=gha` cache uses the same URL, so containers need no docker shim. Colima is untested: its `host.docker.internal` may not reach the Mac's loopback.

On Linux, `127.0.0.1` on the host is out of a container's reach. Until the cache server can also listen on the Docker bridge, Linux-host containers drop the cache variables and use GitHub's cache service, as without spareci (`containerEnv`).

## Resource limits

`Container.CPUs` and `Container.Memory` become `docker run --cpus` and `--memory` per job. Unset, a job can use what the runtime has: OrbStack and Docker Desktop cap their VM, and Docker Engine on Linux doesn't cap at all. `--cpus 4 --memory 16g` matches a standard hosted runner.

## Testing

- `go test -short ./internal/runner`: the container backend against a fake runtime (build once per tag, pruning, a missing or unusable runtime, run arguments, env file and URL rewriting, cancel, cleanup).
- `go test ./internal/runner/image/dockerproxy`: the proxy against a fake daemon (bind rewriting, pass-through, streaming, upgrade with half-close).
- `SPARECI_TEST_CONTAINER=1 go test ./internal/runner -run TestContainerE2E -v -timeout 30m`: builds the image for each architecture this machine offers against the real runtime, checks the tools and the patched worker in it, and runs a runner with a JIT config GitHub rejects, checking that the container and its volume are removed.
- `SPARECI_TEST_CONTAINER=1 go test ./internal/runner -run TestContainerPathsE2E -v -timeout 30m`: runs two jobs at once through the real entrypoint and proxy, with a stand-in for the runner. Both must see the same `pwd`, `$HOME`, `pnpm store path`, `npm config get cache` and `go env GOMODCACHE`; neither may see the other's workspace; and `docker run -v $PWD`, the runner's `/__w` and `/__t` mounts, `docker compose`, `docker build` and `docker run -i` must reach the job's own files.

Building a new image removes older spareci images for that architecture that no container is using, so running the e2e tests on a machine that also runs `spareci up` can remove the image that service is using if none of its jobs is running at that moment. Its next Linux job then fails to start (`docker run --pull never`) until the service is restarted and rebuilds it. Use a different `SPARECI_RUNNER_VERSION` for the tests on such a machine, or run them while it is down.
