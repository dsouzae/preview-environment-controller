FROM golang:1.27.1-alpine3.23 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_HASH=unknown
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=vendor \
    -ldflags="-X edlab.dev/preview-environment-controller/internal/buildinfo.Version=${VERSION} -X edlab.dev/preview-environment-controller/internal/buildinfo.GitHash=${GIT_HASH}" \
    -o /manager ./cmd

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tini && adduser -D -u 1000 app
COPY --from=builder /manager /manager
USER 1000:1000
ENTRYPOINT ["/sbin/tini", "--", "/manager"]
