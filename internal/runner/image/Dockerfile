# spareci's Linux job image (internal/runner/image). Built locally on first
# use, per architecture and runner version; never pushed to a registry.
# It is not GitHub's ubuntu-24.04 image: see docs/linux-containers.md for what
# is missing.
FROM ubuntu:24.04

ARG RUNNER_VERSION
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    ImageOS=ubuntu24 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget gnupg lsb-release software-properties-common \
      git git-lfs openssh-client jq unzip zip xz-utils zstd bzip2 tar gzip rsync file \
      build-essential pkg-config python3 python3-pip python3-venv \
      sudo locales tzdata tini iproute2 iputils-ping dnsutils netcat-openbsd less patch time \
 && locale-gen en_US.UTF-8 \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends docker-ce-cli docker-buildx-plugin docker-compose-plugin gh \
 && rm -rf /var/lib/apt/lists/*

# The runner user matches GitHub's hosted images (uid 1001, passwordless sudo).
RUN userdel -r ubuntu 2>/dev/null || true \
 && groupadd -g 1001 runner && useradd -m -u 1001 -g runner -s /bin/bash runner \
 && groupadd -f docker && usermod -aG docker runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner && chmod 0440 /etc/sudoers.d/runner \
 && install -d -o runner -g runner /opt/hostedtoolcache

# actions/runner, plus a copy whose worker lets spareci's local cache supply
# ACTIONS_RESULTS_URL (the same same-length rename as internal/runner/patch.go).
# Hard links: only the patched worker takes extra space.
RUN arch=$([ "$TARGETARCH" = amd64 ] && echo x64 || echo "$TARGETARCH") \
 && mkdir -p /opt/runner \
 && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz" | tar xz -C /opt/runner \
 && /opt/runner/bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && chown -R runner:runner /opt/runner \
 && cp -al /opt/runner /opt/runner-cache \
 && python3 -c 'import os, sys; p=sys.argv[1]; d=open(p,"rb").read(); a="ACTIONS_RESULTS_URL".encode("utf-16-le"); b="ACTIONS_RESULTS_ORL".encode("utf-16-le"); assert a in d, "worker has no ACTIONS_RESULTS_URL"; os.unlink(p); open(p,"wb").write(d.replace(a,b))' /opt/runner-cache/bin/Runner.Worker.dll \
 && chown runner:runner /opt/runner-cache/bin/Runner.Worker.dll

COPY entrypoint.sh /usr/local/bin/spareci-entrypoint
RUN chmod 0755 /usr/local/bin/spareci-entrypoint
WORKDIR /home/runner
# tini -g: SIGINT from `docker stop` reaches the runner's whole process group,
# which the runner treats as "finish up and deregister".
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/spareci-entrypoint"]
