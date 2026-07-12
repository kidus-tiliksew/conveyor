# Conveyor's dogfood image: the common sandbox plus the toolchains required to
# run Conveyor's own Phase 3 checks (spec §6.1, roadmap §19).
ARG GO_VERSION=1.24.0
ARG BASE_IMAGE=conveyor-base:dev
FROM golang:${GO_VERSION}-bookworm AS go-toolchain

FROM ${BASE_IMAGE}

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update \
    && apt-get install -y --no-install-recommends make \
    && rm -rf /var/lib/apt/lists/* \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
    && go version \
    && make --version \
    && bash -lc 'go version && make --version'
USER agent
