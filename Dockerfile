# Pinned multi-architecture bases keep tag builds reproducible.
ARG GO_IMAGE=golang:1.24.0-bookworm@sha256:b970e6d47c09fdd34179acef5c4fecaf6410f0b597a759733b3cbea04b4e604a
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

FROM ${GO_IMAGE} AS build
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
      -ldflags "-X github.com/kidus-tiliksew/conveyor/internal/releaseinfo.Version=${VERSION}" \
      -o /out/conveyor ./cmd/conveyor \
    && CGO_ENABLED=0 go build \
      -ldflags "-X github.com/kidus-tiliksew/conveyor/internal/releaseinfo.Version=${VERSION}" \
      -o /out/conveyord ./cmd/conveyord

FROM ${RUNTIME_IMAGE}
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 conveyor \
    && useradd --uid 10001 --gid conveyor --create-home --home-dir /home/conveyor conveyor \
    && mkdir -p /etc/conveyor \
    && chown -R conveyor:conveyor /home/conveyor

COPY --from=build /out/conveyor /usr/local/bin/conveyor
COPY --from=build /out/conveyord /usr/local/bin/conveyord

ARG VERSION=dev
LABEL org.opencontainers.image.version="${VERSION}"
ENV HOME=/home/conveyor
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/conveyord"]
CMD ["-config", "/etc/conveyor/conveyor.yaml"]
