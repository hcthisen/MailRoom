# syntax=docker/dockerfile:1
#
# Mailroom: one static binary in a distroless image (PRD O-1, O-6).
#
#   docker build -t mailroom --build-arg VERSION=1.0.0 .
#
# The build context is this directory (app/). Go modules come from vendor/
# (go mod vendor, kept in the kit), so the build needs no network after the
# base images are pulled; without vendor/ they are downloaded from the Go
# proxy. The server's compose file is ../deploy/docker-compose.yml.

# The Go toolchain: at least the version in go.mod's go line.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
ARG VERSION=dev
# Static, reproducible, no VCS stamping (the kit is not a git checkout) and
# no toolchain download: the image's Go builds the module.
ENV CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local GOFLAGS=-buildvcs=false
WORKDIR /src
COPY . .
RUN set -eu; \
    if [ -f vendor/modules.txt ]; then \
        echo "building with the vendored modules"; mod=-mod=vendor; \
    else \
        echo "vendor/ not found: downloading modules"; go mod download; mod=-mod=readonly; \
    fi; \
    go build "$mod" -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mailroom ./cmd/mailroom; \
    /out/mailroom version; \
    mkdir -p /out/data /out/tmp; \
    chmod 0700 /out/data; \
    chmod 1777 /out/tmp

# distroless/static: CA certificates (AWS, Let's Encrypt, SNS certificate
# fetches), tzdata and a non-root user (nonroot, uid/gid 65532); no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mailroom /mailroom
COPY --from=build /out/tmp /tmp
COPY --from=build --chown=65532:65532 /out/data /data
# "/" is on PATH so "docker compose exec mailroom mailroom ..." finds the
# binary under the name the runbook uses.
ENV PATH=/usr/local/bin:/usr/bin:/bin:/ \
    MAILROOM_DATA_DIR=/data \
    TMPDIR=/tmp
USER 65532:65532
# mailroom.db, media/, backups/, imports/, secret.key and autocert/.
VOLUME ["/data"]
# 443 (HTTPS) and 80 (ACME HTTP-01, redirect, /healthz) with autocert;
# 8080 with HTTP_ONLY=1 behind a reverse proxy. Binding 80/443 needs no
# root: Docker lets containers bind low ports (ip_unprivileged_port_start).
EXPOSE 80 443 8080
ENTRYPOINT ["/mailroom"]
CMD ["serve"]
