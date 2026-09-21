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
        -X github.com/ziozzang/dorang/internal/app.Version=${VERSION} \
        -X github.com/ziozzang/dorang/internal/app.Commit=${COMMIT} \
        -X main.buildDate=${BUILD_DATE}" \
      -o /out/dorang    ./cmd/dorang && \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dorangctl ./cmd/dorangctl

# The state directory, prepared HERE because the runtime stage has no shell to
# prepare it in and no way to fix it at start-up. 65532 is the numeric id of
# distroless's `nonroot`, spelled as a number because this stage's /etc/passwd
# does not have that name in it.
#
# OWNERSHIP is the whole mechanism; the mode is not. BuildKit gives the
# directory COPY creates its own 0755 and Docker's local volume driver gives the
# volume root 0755 as well, so neither honours a chmod here — and neither needs
# to, because everything dorang writes inside is created 0700 by the process
# itself (internal/app, internal/meter, internal/batch).
RUN mkdir -p /out/state && chown 65532:65532 /out/state

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
# The directory is copied in ALREADY OWNED by the user this image runs as, and
# that is the load-bearing part rather than a tidiness. Docker seeds a fresh
# named volume from whatever is at the mount point in the image, ownership
# included — so a directory that arrives here owned by root produces a volume
# owned by root, and the first thing the process does is
#
#   dorang: app: storage directory: mkdir /var/lib/dorang/.dorang: permission denied
#
# under `restart: unless-stopped`, forever, with no shell in the image to fix it
# from. The failure appears ONLY when Docker creates the volume: an operator who
# chowned it once out of band never sees it again and cannot reproduce it. The
# fix is one COPY --chown and it belongs in the image, because the image is the
# thing that told the operator to mount here.
#
# The seeding rule is a NAMED-VOLUME rule. A bind mount (`-v /srv/dorang:…`)
# keeps the host directory's ownership and Docker copies nothing into it, so a
# bind mount still has to be `chown 65532:65532` on the host first — CONFIG §23
# says so beside DORANG_STATE_DIR, where an operator is choosing between the two.
#
# DORANG_STATE_DIR is what makes the defaults land inside the volume. Every
# shipped state path is written "~/.dorang/…", and config.ExpandPath resolves
# that leading "~" to this directory. Without it, "~" is /home/nonroot and all of
# it — the database and the pepper that makes its keys verifiable — goes into the
# container's writable layer and is lost on restart.
COPY --from=build --chown=65532:65532 /out/state /var/lib/dorang
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
