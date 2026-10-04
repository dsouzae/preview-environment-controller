# Builds the manager offline from the committed vendor tree; nothing is
# downloaded during the build. The Makefile docker-build/docker-buildx targets
# and the publish commands in docs/smoke-test.md supply the build args.
# TARGETARCH must match the cluster nodes, not the workstation.
FROM golang:1.27.1-alpine3.23 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
# Linked into internal/buildinfo; surfaces as service.version in traces and
# should equal the VERSION file and the commit being built.
ARG VERSION=dev
ARG GIT_HASH=unknown
WORKDIR /src
COPY . .
# CGO_ENABLED=0 gives a static binary with no C runtime dependency, so the
# runtime stage needs no compatibility libraries.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=vendor \
    -ldflags="-X edlab.dev/preview-environment-controller/internal/buildinfo.Version=${VERSION} -X edlab.dev/preview-environment-controller/internal/buildinfo.GitHash=${GIT_HASH}" \
    -o /manager ./cmd

# Runtime stage. ca-certificates lets HTTPS exporters (OTLP collectors behind
# public CAs) verify certificates; tini runs as PID 1 so signals reach the
# manager and leader election can release cleanly. UID 1000 matches runAsUser
# in config/manager/manager.yaml; the root filesystem is mounted read-only there.
FROM alpine:3.23
RUN apk add --no-cache ca-certificates tini && adduser -D -u 1000 app
COPY --from=builder /manager /manager
USER 1000:1000
ENTRYPOINT ["/sbin/tini", "--", "/manager"]
