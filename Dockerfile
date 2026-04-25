# syntax=docker/dockerfile:1.7

ARG GO_IMAGE=dhi.io/golang:1.26-dev
ARG RUNTIME_IMAGE=dhi.io/static:20250419

FROM ${GO_IMAGE} AS build
WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/waitfor ./cmd/waitfor

FROM ${RUNTIME_IMAGE}

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

LABEL org.opencontainers.image.title="waitfor" \
      org.opencontainers.image.description="Semantic condition poller for scripts, CI, Kubernetes init containers, and agent workflows" \
      org.opencontainers.image.source="https://github.com/pbsladek/waitfor" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/waitfor /waitfor

ENTRYPOINT ["/waitfor"]
CMD ["--help"]
