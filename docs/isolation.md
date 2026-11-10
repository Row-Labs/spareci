# Per-job isolation (design)

Status: proposal. Nothing here is built yet except the job home (milestone 1).

## Why

spareci runs every job as the host user. Since the job home landed, jobs no longer use the user's personal tool state, but a job can still read anything the user can (`~/.ssh`, the unlocked login keychain, other repos' checkouts, the cache store) and can leave things behind for the next job. That's acceptable for your own private repos and unacceptable for public ones, where a fork's pull request runs code chosen by a stranger. Public-repo support waits for the isolation described here.

## Goals

1. A job can't read or change the host user's files, credentials or other jobs' state.
2. Each job starts from a known clean state, as on GitHub-hosted runners.
3. Still fast: start a job in seconds, keep the local cache, keep downloaded toolchains warm.
4. Still a single, open-source binary that a user installs with `go install` or Homebrew. No Kubernetes, no daemon the user has to configure beyond what the OS ships.
5. A fallback: if isolation can't start (no Docker, no virtualization), say so and refuse public repos, rather than silently running on the host.

## What stays on the host

In every option below, spareci itself stays on the host: polling GitHub, minting JIT configs, the routing variable, the local cache server and its store. The guest only runs `actions/runner` with one JIT config. That keeps GitHub credentials (spareci's `gh` token) out of the guest; the guest only ever sees the JIT config and the job's own `GITHUB_TOKEN`.

The cache server needs one change everywhere: today each job's listener binds to `127.0.0.1`, which a guest can't reach. Per-job listeners, HMAC-signed URLs and branch scoping already exist, so the server only needs to bind where the guest can reach it (or be tunnelled, see below).

## Linux

### Option A: a container per job

Run `actions/runner` in a fresh container for each job with Docker or Podman.

- **Image.** Start from GitHub's own `ghcr.io/actions/actions-runner` (the image actions-runner-controller uses; Ubuntu, runner preinstalled, small). It is not the hosted-runner image: the hosted image has hundreds of preinstalled tools and is tens of GB. Jobs that rely on preinstalled tools fail unless they use `setup-*` actions, which is the same trade-off every self-hosted runner makes. spareci can let users pass `--image` for their own.
- **Startup.** With the image present, `docker run` takes well under a second; the runner then takes the same ~8s to connect as today. The first pull is a one-time cost.
- **Warm state.** Mount the toolcache and, for private repos, the per-repo job home as volumes, so `setup-go` and friends stay fast. For public repos, mount nothing writable and shared: caches go through `actions/cache`, which already scopes by ref.
- **Reaching the cache.** Bind the job's listener to the bridge gateway (`docker0`, or Podman's `host.containers.internal`) and pass `--add-host=host.docker.internal:host-gateway`; set `ACTIONS_RESULTS_URL` to that address. The listener is per job and its URLs are signed, so other containers on the bridge can't use it without the job's token. `--network=host` would be simpler and must not be used, because it exposes every host-local service to the job.
- **Rootless.** Rootless Podman (or rootless Docker) means a container escape lands as an unprivileged user, not root. Recommend it and detect it. Rootful Docker is common on VPSes; support it, but say that root in the container is close to root on the host if the runtime has a bug.
- **Docker inside the job.** Many workflows use `docker build`, `services:`, `container:` jobs or Docker actions. Options: mount the host socket (defeats isolation, never for public repos), Docker-in-Docker with `--privileged` (defeats isolation), or a runtime that makes nested containers safe (Sysbox). First version: no Docker inside jobs for isolated mode, clearly reported.
- **Isolation strength.** Containers share the host kernel. Kernel and runtime escapes appear every year. Good enough to contain accidents and trusted collaborators; borderline for anonymous fork PRs. gVisor (`--runtime=runsc`) narrows the kernel surface at a CPU and I/O cost and is worth offering as a flag.

### Option B: a microVM per job

Boot a small VM per job with Firecracker or Cloud Hypervisor.

- **Isolation.** A separate kernel behind KVM: the boundary public CI providers rely on. Docker inside the job works normally, because the job has its own kernel.
- **Requirements.** `/dev/kvm`. Bare metal and most desktops have it; many cheap VPSes don't expose nested virtualization, and those users would be limited to containers.
- **Startup.** Firecracker boots a minimal kernel in well under a second; the cost is the root filesystem. Use a prebuilt ext4 image (the runner image flattened) and a copy-on-write overlay per job (reflink copy, or a device-mapper snapshot). Expect a few seconds to a working runner, to be measured.
- **Reaching the cache.** A tap device per VM with a host address on a /30, or vsock with a small forwarder in the guest. Same per-job listener as above.
- **Cost of building it.** spareci has to ship or build the kernel and rootfs, manage tap devices or vsock, and needs root or `CAP_NET_ADMIN` for networking. That's real work and more moving parts for users.

### Linux recommendation

Ship containers first (rootless Podman or Docker, official runner image, cache on the bridge), because nearly every Linux machine can run them and they cover the private-repo case well. Make microVMs the requirement for public repos on Linux, with Firecracker as the backend, once containers have settled the guest side (image layout, volumes, cache wiring), which carries over.

## macOS

Apple Silicon Macs can only run macOS guests through Apple's Virtualization.framework. The question is whether spareci drives it directly or through Tart.

### Constraints that apply to any option

- **Two VMs per Mac.** The macOS licence allows up to two additional macOS instances in VMs per Mac, and the framework enforces it. So `--max` is capped at 2 in isolated mode. Linux guests on a Mac aren't limited.
- **Base image.** A macOS guest is installed from an IPSW restore image (about 15 GB). After the install, Setup Assistant has to be clicked through to create a user and turn on auto-login; there's no API for it. Tart's Packer plugin does it by typing keystrokes over VNC, which breaks when Apple changes Setup Assistant. Then: Xcode command-line tools, Homebrew, the runner, a guest agent.
- **Image updates.** macOS point releases and Xcode versions mean rebuilding the base image every few weeks. Users won't do this by hand.
- **Per-job disks.** APFS `clonefile` copies a 50 GB disk image instantly and uses no space until the guest writes; delete the clone after the job. Same trick spareci uses for runner installs today.
- **Boot time.** A cold macOS boot to a logged-in desktop takes tens of seconds on M-series hardware (to be measured on Eventric). macOS 14 added save and restore of a VM's running state: restoring a saved, logged-in state paired with a fresh clone of the same disk should take a few seconds. Worth prototyping early, because it decides whether isolation feels as fast as today.
- **Networking to the host cache.** NAT networking gives the guest an address on `192.168.64.0/24` with the host at the gateway, but that interface only exists while a VM runs and binding to it is fiddly. Better: a `virtio-vsock` device, with a small agent in the guest that forwards `127.0.0.1:<port>` to the host over vsock. The same agent can receive the JIT config and start the runner, so spareci needs no SSH into the guest and no guest password.
- **Entitlement.** A binary using Virtualization.framework must be signed with `com.apple.security.virtualization`. Ad-hoc signing works, so `go install` users can run one `codesign` command (or `spareci` can print it), and release builds get signed properly.

### Option A: Virtualization.framework directly, via Code-Hex/vz

[Code-Hex/vz](https://github.com/Code-Hex/vz) is MIT-licensed Go bindings for Virtualization.framework, covering macOS guests, restore images, vsock, NAT and save/restore.

- Licence-clean for an OSS project, and spareci stays one binary.
- Needs cgo on macOS, which `go install` handles with the Xcode command-line tools.
- spareci owns everything Tart already does: installing from IPSW, the Setup Assistant automation, image storage. That's most of the work.

### Option B: Tart

[Tart](https://github.com/cirruslabs/tart) (Cirrus Labs) wraps the same framework in a CLI, stores images in OCI registries, and has ready-made macOS images with Xcode.

- Much less to build: `tart clone`, `tart run`, `tart ip`, plus Cirrus's maintained images.
- **Licence.** Tart is under Cirrus Labs' Fair Source licence: source-available, free below a usage threshold, paid above it. It isn't OSI open source. spareci can't vendor or embed it without inheriting those terms, and pointing users at it means some of them owe Cirrus a licence. Calling a `tart` binary the user installed themselves keeps spareci's own licence clean, but makes a non-free tool the main path of an open-source project.
- Redistributing macOS images is legally grey under Apple's licence; relying on a third party's image registry ties spareci to their choices.
- Tart's default guest access is SSH with a known password, which spareci would replace with the vsock agent anyway.

### macOS recommendation

Build on Virtualization.framework through Code-Hex/vz, and keep the backend behind an interface so a `tart` backend (calling a user-installed Tart, for people who already have it) can be added cheaply. Ship image creation as `spareci image create`, from the IPSW Apple publishes, with the Setup Assistant step automated. Until that automation is reliable, also accept an existing VM bundle (Tart's on-disk format is a disk image, NVRAM and a JSON config, which vz can boot), so early users can start from an image they built another way.

## Recommendation

1. Isolation becomes a backend choice: `--isolation=host|container|vm`. `host` is today's behaviour with the job home. Public repos require `container` with gVisor or `vm` on Linux, and `vm` on macOS.
2. One guest-side contract for every backend: a small agent that receives the JIT config and the cache address, starts `actions/runner` and reports the result. The host side doesn't change shape: poll, mint, start a guest, wait, delete.
3. Linux: containers first, Firecracker second. macOS: Virtualization.framework via Code-Hex/vz; Tart optional.
4. Public repos get no shared writable state at all: no job home, no shared toolcache volume. Warm caches only through `actions/cache`, which already scopes by ref, so a fork PR can read its base branch's caches but never write them.

## Milestones

| # | What | Done when |
|---|---|---|
| 1 | Job home (shipped) | Jobs get a spareci-owned `HOME` and a filtered environment by default. |
| 2 | Backend interface | `runner.Host.Run` becomes one backend; the cache listener can bind to a given address. No behaviour change. |
| 3 | Linux containers | `--isolation=container` runs this repo's CI in rootless Podman and in Docker, with the local cache over the bridge. |
| 4 | macOS VM prototype | One job runs in a vz guest booted from a hand-made image, with cache over vsock. Measure cold boot against restore-from-saved-state. |
| 5 | macOS images | `spareci image create` builds a base image from an IPSW unattended; `spareci image update` rebuilds it. |
| 6 | Linux microVMs | `--isolation=vm` on Linux with Firecracker; Docker inside jobs works. |
| 7 | Public repos | spareci accepts a public repo only with a strong backend, never shares writable state between its jobs, and documents the fork PR policy. |
