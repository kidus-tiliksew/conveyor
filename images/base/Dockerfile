# Base Conveyor image (spec §6.1): the common layer every repo
# devcontainer extends. Provides git, the harness CLIs, the job shim,
# browsers for verification (Phase 4), and the cloud toolbelt (§11.1).
#
# Phase 2 keeps this minimal: git + node, both harness CLIs, gh, and the
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

# Harness CLIs. Keep versions aligned with each adapter's PinnedVersion;
# runtime checks fail closed on drift.
ARG CODEX_VERSION=0.142.0
ARG CLAUDE_CODE_VERSION=2.1.206
RUN npm install -g \
        "@openai/codex@${CODEX_VERSION}" \
        "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && test "$(codex --version)" = "codex-cli ${CODEX_VERSION}" \
    && test "$(claude --version | awk '{print $1}')" = "${CLAUDE_CODE_VERSION}"

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
