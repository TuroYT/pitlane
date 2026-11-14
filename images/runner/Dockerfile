# Default Pitlane runner image: the official GitHub runner image plus the
# build tools most CIs expect from ubuntu-latest.
FROM ghcr.io/actions/actions-runner:latest
USER root
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential zip unzip xz-utils jq git-lfs rsync openssh-client \
      ca-certificates gnupg lsb-release wget \
 && rm -rf /var/lib/apt/lists/*
USER runner
