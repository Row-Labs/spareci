#!/bin/sh
# spareci's container entrypoint (internal/runner/image). Runs as root, picks
# the runner install, starts the docker proxy if the runtime's socket is
# mounted, and starts one ephemeral runner as the runner user.
set -eu

jit=${SPARECI_JIT_CONFIG:?SPARECI_JIT_CONFIG is not set}
unset SPARECI_JIT_CONFIG

src=/opt/runner
# The patched worker only when spareci's local cache is in use.
if [ -n "${SPARECI_RESULTS_URL:-}" ] && [ -d /opt/runner-cache ]; then
  src=/opt/runner-cache
fi

# The runner runs from a copy in HOME, so its workspace
# (/home/runner/actions-runner/_work) is at the same path in every job and on
# the same filesystem as HOME, as on a hosted runner. With docker passthrough,
# /home/runner is the job's own volume (Docker fills it from the image).
root=/home/runner/actions-runner
mkdir -p "$root"
cp -a "$src/." "$root/"
chown runner:runner "$root"

# Docker passthrough: the runtime's socket is mounted at
# /run/spareci/docker.sock, and jobs reach it through spareci-dockerproxy,
# which rewrites bind mounts of paths here (SPARECI_DOCKER_MAP, FROM=TO ...)
# to where the daemon sees them. It runs as root, so the socket's owner on
# the daemon's host doesn't matter.
if [ -S /run/spareci/docker.sock ]; then
  # shellcheck disable=SC2086 # the map is a list of FROM=TO words
  /usr/local/bin/spareci-dockerproxy -listen /var/run/docker.sock -upstream /run/spareci/docker.sock ${SPARECI_DOCKER_MAP:-} &
  i=0
  until [ -S /var/run/docker.sock ]; do
    i=$((i + 1))
    if [ "$i" -gt 100 ]; then
      echo "spareci: the docker proxy didn't start" >&2
      exit 1
    fi
    sleep 0.05
  done
  chgrp docker /var/run/docker.sock
  chmod 0660 /var/run/docker.sock
fi
unset SPARECI_DOCKER_MAP

cd "$root"
export HOME=/home/runner USER=runner LOGNAME=runner
exec setpriv --reuid=runner --regid=runner --init-groups ./run.sh --jitconfig "$jit"
