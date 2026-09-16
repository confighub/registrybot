# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT

###############
# Pinned runtime base image (multi-arch index digest) for reproducible builds and
# a version-exact GPL corresponding-source reference (see OS_LICENSE_NOTICE.txt).
# alpine:3.24.1 as of 2026-06-16. Bump deliberately: re-run
# `docker buildx imagetools inspect alpine:3.24 --format '{{.Manifest.Digest}}'`,
# update this digest, and refresh the package list in OS_LICENSE_NOTICE.txt.
ARG ALPINE_BASE=alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

###############
# Build stage #
###############
# The builder runs natively on the BUILD platform (the amd64 GitHub runner) and
# cross-compiles the Go binary for the TARGET arch. CGO is disabled, so this is a
# fast pure-Go cross-compile with no QEMU emulation. Buildx injects the
# BUILDPLATFORM / TARGETOS / TARGETARCH args automatically.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /go/src/app

# Cache dependency downloads (arch-independent)
COPY go.mod go.sum ./
RUN go mod download

# Cross-compile for the target arch. VERSION is stamped into the binary and
# reported on /healthz and in the User-Agent the bot sends.
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /go/bin/registrybot .

#################
# Runtime stage #
#################
# Target-arch image. It contains NO RUN steps, so QEMU is only ever used to
# assemble/pull layers — it never executes an emulated binary. CA certs are
# copied from the builder (a PEM bundle is arch-independent); the numeric USER
# needs no /etc/passwd entry.
FROM ${ALPINE_BASE}
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 1000:3000
WORKDIR /app
COPY --from=builder /go/bin/registrybot .
# Third-party notices shipped with the image (MIT/BSD/Apache require them to
# accompany binary distributions):
#   THIRD_PARTY_LICENSES.txt — Go modules linked into the registrybot binary;
#                              regenerate with scripts/gen-third-party-licenses.sh
#   OS_LICENSE_NOTICE.txt    — Alpine base OS packages + GPL source offer
COPY --from=builder /go/src/app/LICENSE /go/src/app/THIRD_PARTY_LICENSES.txt /go/src/app/OS_LICENSE_NOTICE.txt ./
EXPOSE 8080
ENTRYPOINT ["/app/registrybot"]
