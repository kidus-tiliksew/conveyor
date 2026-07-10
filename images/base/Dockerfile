# Base Conveyor image (spec §6.1): the common layer every repo
# devcontainer extends. Provides git, the harness CLIs, the job shim,
# browsers for verification (Phase 4), and the cloud toolbelt (§11.1).
#
# Phase 1 keeps this minimal: git + node (for the Codex CLI) + gh + the
# shim. The toolbelt (gcloud, aws, kubectl, terraform) and browsers land
# with the phases that need them.

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git openssh-client jq \
    && rm -rf /var/lib/apt/lists/*

# Node for the Codex CLI. Pinned major; bump deliberately.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Harness CLIs. Versions pinned alongside the adapter's PinnedVersion.
RUN npm install -g @openai/codex

# GitHub CLI for PR operations inside sandboxes.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# The job shim: a dependency-free static binary built by `make shim`
# (GOOS=linux) and copied in at image build time.
COPY conveyor-shim /usr/local/bin/conveyor-shim

# Non-root agent user; job worktrees mount under /conveyor/jobs at a
# deterministic path per task (spec §8.3 note 3).
RUN useradd --create-home --shell /bin/bash agent \
    && mkdir -p /conveyor/jobs /conveyor/cache \
    && chown -R agent:agent /conveyor
USER agent
WORKDIR /conveyor/jobs

ENTRYPOINT ["conveyor-shim"]
