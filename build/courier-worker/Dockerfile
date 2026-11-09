# syntax=docker/dockerfile:1
# Build context is the repository root:
#   docker build -f build/courier-worker/Dockerfile -t courier-worker:dev .
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w" -o /out/courier-worker ./cmd/worker

# static + CA certificates (SMTP and Telegram TLS), runs as non-root
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="courier-worker" \
      org.opencontainers.image.source="https://github.com/huponx/courier" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /out/courier-worker /courier-worker
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/courier-worker"]
