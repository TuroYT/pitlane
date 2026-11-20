# Default Pitlane runner image: GitHub's official runner image plus the tools
# workflows usually expect from `ubuntu-latest`, so most of them run unchanged.
#
# Toolchains needed in several versions (Go, Python, Java, Node…) are still best
# installed with the setup-* actions: Pitlane keeps their downloads in a local
# tool cache shared between jobs.
FROM ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300

ARG TARGETARCH
# Every download below is pinned to a version and checked against its
# SHA-256 before use: the build fails on any mismatch. Renovate bumps the
# versions (see renovate.json); the checksums come from the checksum file each
# project publishes with that release. The apt signing keys have no version:
# their hash is pinned here and must be updated by hand when they rotate.
# renovate: datasource=node-version depName=node versioning=node
ARG NODE_VERSION=26.13.1
# renovate: datasource=github-releases depName=docker/compose
ARG COMPOSE_VERSION=v5.7.1
# renovate: datasource=github-releases depName=mikefarah/yq
ARG YQ_VERSION=v4.54.3
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
ARG GH_CLI_KEY_SHA256=6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b
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
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    echo "${PGDG_KEY_SHA256}  /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc" | sha256sum -c -; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list; \
    apt-get update; apt-get install -y --no-install-recommends postgresql-client; rm -rf /var/lib/apt/lists/*; \
    pg_dump --version

# Node.js LTS with corepack (pnpm / yarn), like ubuntu-latest.
RUN set -eux; \
    arch="$(case "${TARGETARCH:-amd64}" in arm64) echo arm64 ;; *) echo x64 ;; esac)"; \
    file="node-v${NODE_VERSION}-linux-${arch}.tar.xz"; \
    tmp="$(mktemp -d)"; cd "$tmp"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors -O "https://nodejs.org/dist/v${NODE_VERSION}/${file}"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors -O "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt"; \
    sum="$(awk -v f="$file" '$2 == f { print $1 }' SHASUMS256.txt)"; \
    echo "$sum" | grep -Eqx '[0-9a-f]{64}'; \
    echo "${sum}  ${file}" | sha256sum -c -; \
    tar -xJf "$file" -C /usr/local --strip-components=1 --no-same-owner; \
    cd /; rm -rf "$tmp"; \
    corepack enable; \
    test "$(node --version)" = "v${NODE_VERSION}"; npm --version

# GitHub CLI.
RUN set -eux; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg; \
    echo "${GH_CLI_KEY_SHA256}  /usr/share/keyrings/githubcli-archive-keyring.gpg" | sha256sum -c -; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; apt-get install -y --no-install-recommends gh; rm -rf /var/lib/apt/lists/*

# docker compose plugin and yq.
RUN set -eux; \
    carch="$(case "${TARGETARCH:-amd64}" in arm64) echo aarch64 ;; *) echo x86_64 ;; esac)"; \
    yarch="$(case "${TARGETARCH:-amd64}" in arm64) echo arm64 ;; *) echo amd64 ;; esac)"; \
    tmp="$(mktemp -d)"; cd "$tmp"; \
    compose="docker-compose-linux-${carch}"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors -O "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/${compose}"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "https://github.com/docker/compose/releases/download/${COMPOSE_VERSION}/checksums.txt" -o compose.sums; \
    sum="$(awk -v f="*${compose}" '$2 == f { print $1 }' compose.sums)"; \
    echo "$sum" | grep -Eqx '[0-9a-f]{64}'; \
    echo "${sum}  ${compose}" | sha256sum -c -; \
    install -D -m 0755 "$compose" /usr/local/lib/docker/cli-plugins/docker-compose; \
    yq="yq_linux_${yarch}"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors -O "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/${yq}"; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/checksums" -o yq.sums; \
    curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/checksums_hashes_order" -o yq.order; \
    col="$(grep -nx 'SHA-256' yq.order | cut -d: -f1)"; \
    sum="$(awk -v f="$yq" -v c="$((col + 1))" '$1 == f { print $c }' yq.sums)"; \
    echo "$sum" | grep -Eqx '[0-9a-f]{64}'; \
    echo "${sum}  ${yq}" | sha256sum -c -; \
    install -m 0755 "$yq" /usr/local/bin/yq; \
    cd /; rm -rf "$tmp"; \
    docker compose version; yq --version

ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk-${TARGETARCH:-amd64}
USER runner
