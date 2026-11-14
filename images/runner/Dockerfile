# Default Pitlane runner image: GitHub's official runner image plus the tools
# workflows usually expect from `ubuntu-latest`, so most of them run unchanged.
#
# Toolchains needed in several versions (Go, Python, Java, Node…) are still best
# installed with the setup-* actions: Pitlane keeps their downloads in a local
# tool cache shared between jobs.
FROM ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300

ARG TARGETARCH
ARG NODE_MAJOR=24
USER root
ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8

# Build tools, archives, network/debug utilities, database clients, Python and Java.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential cmake ninja-build pkg-config autoconf automake libtool patch \
      zip unzip xz-utils zstd bzip2 p7zip-full \
      jq git-lfs rsync openssh-client ca-certificates gnupg lsb-release wget curl \
      file tree time parallel tzdata locales sudo less nano \
      sqlite3 libsqlite3-dev default-mysql-client redis-tools \
      libssl-dev libffi-dev zlib1g-dev libyaml-dev libreadline-dev \
      python3-pip python3-venv python3-dev pipx \
      openjdk-17-jdk-headless \
 && git lfs install --system \
 && rm -rf /var/lib/apt/lists/*

# PostgreSQL client from the official PGDG repository: the latest major can
# talk to every older server (Ubuntu's client is too old for recent services).
RUN set -eux; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; apt-get install -y --no-install-recommends postgresql-client; rm -rf /var/lib/apt/lists/*; \
    pg_dump --version

# Node.js LTS with corepack (pnpm / yarn), like ubuntu-latest.
RUN set -eux; \
    arch="$(case "${TARGETARCH:-amd64}" in arm64) echo arm64 ;; *) echo x64 ;; esac)"; \
    ver="$(curl -fsSL https://nodejs.org/dist/index.json | jq -r --arg m "v${NODE_MAJOR}." '[.[] | select(.version | startswith($m))][0].version')"; \
    curl -fsSL "https://nodejs.org/dist/${ver}/node-${ver}-linux-${arch}.tar.xz" | tar -xJ -C /usr/local --strip-components=1; \
    corepack enable; \
    node --version; npm --version

# GitHub CLI.
RUN set -eux; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; apt-get install -y --no-install-recommends gh; rm -rf /var/lib/apt/lists/*

# docker compose plugin and yq.
RUN set -eux; \
    carch="$(case "${TARGETARCH:-amd64}" in arm64) echo aarch64 ;; *) echo x86_64 ;; esac)"; \
    mkdir -p /usr/local/lib/docker/cli-plugins; \
    curl -fsSL "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${carch}" -o /usr/local/lib/docker/cli-plugins/docker-compose; \
    chmod +x /usr/local/lib/docker/cli-plugins/docker-compose; \
    curl -fsSL "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${TARGETARCH:-amd64}" -o /usr/local/bin/yq; \
    chmod +x /usr/local/bin/yq; \
    docker compose version; yq --version

ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk-${TARGETARCH:-amd64}
USER runner
