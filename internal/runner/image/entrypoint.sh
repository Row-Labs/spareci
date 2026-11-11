#!/bin/sh
# spareci's container entrypoint (internal/runner/image). Runs as root, picks
# the runner install, gives the runner user the docker socket if one is
# mounted, and starts one ephemeral runner as that user.
set -eu

jit=${SPARECI_JIT_CONFIG:?SPARECI_JIT_CONFIG is not set}
unset SPARECI_JIT_CONFIG

src=/opt/runner
# The patched worker only when spareci's local cache is in use.
if [ -n "${SPARECI_RESULTS_URL:-}" ] && [ -d /opt/runner-cache ]; then
  src=/opt/runner-cache
fi

# With docker passthrough, the runner runs from a directory the docker daemon
# sees at the same path (a volume mounted at its own data path), so the bind
# mounts it makes for container: jobs and for steps resolve on the daemon.
root=$src
if [ -n "${SPARECI_RUNNER_ROOT:-}" ]; then
  root=$SPARECI_RUNNER_ROOT
  cp -a "$src/." "$root/"
  chown runner:runner "$root"
fi
unset SPARECI_RUNNER_ROOT

if [ -S /var/run/docker.sock ]; then
  gid=$(stat -c %g /var/run/docker.sock)
  if [ "$gid" = 0 ]; then
    # Owned by root (some VMs). Changing the socket's mode would change it
    # on the daemon's host, so join the container's root group instead: the
    # container is thrown away, and the job's user has sudo in it anyway.
    usermod -aG root runner
  else
    getent group "$gid" >/dev/null || groupadd -g "$gid" dockerhost
    usermod -aG "$(getent group "$gid" | cut -d: -f1)" runner
  fi
fi

cd "$root"
export HOME=/home/runner USER=runner LOGNAME=runner
exec setpriv --reuid=runner --regid=runner --init-groups ./run.sh --jitconfig "$jit"
