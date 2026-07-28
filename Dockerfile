# Build. Pinned by digest at release time; a floating tag here would make the
# image irreproducible, which matters more than convenience for something that
# holds provider credentials.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO stays off: the SQLite driver is pure Go, so the result is a static binary
# that runs on scratch. That is the whole reason the notebook tier has zero
# required dependencies — a dynamically linked build would need a base image
# with a libc and would undo it.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags "-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildDate=${BUILD_DATE}" \
      -o /out/dorang    ./cmd/dorang && \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dorangctl ./cmd/dorangctl

# Runtime. Nothing but the binaries and the roots needed to dial an upstream
# over TLS — no shell, no package manager, nothing to pivot from if the process
# is ever compromised.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/dorang    /usr/local/bin/dorang
COPY --from=build /out/dorangctl /usr/local/bin/dorangctl

# Writable state for the notebook tier: the embedded database, the trace spool,
# the generated key pepper and batch blobs. Mount a volume here in any deployment
# that matters.
#
# DORANG_STATE_DIR is what makes the defaults land inside the volume. Every
# shipped state path is written "~/.dorang/…", and config.ExpandPath resolves
# that leading "~" to this directory. Without it, "~" is /home/nonroot and all of
# it — the database and the pepper that makes its keys verifiable — goes into the
# container's writable layer and is lost on restart.
VOLUME ["/var/lib/dorang"]
ENV DORANG_STATE_DIR=/var/lib/dorang

EXPOSE 4100

USER nonroot:nonroot

# Liveness only. Readiness is a separate endpoint because a draining node is
# alive and must not be restarted (§13) — conflating them turns a graceful
# drain into a kill.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/dorangctl", "health", "--addr", "http://127.0.0.1:4100"]

ENTRYPOINT ["/usr/local/bin/dorang"]
CMD ["--config", "/etc/dorang/config.yaml"]
