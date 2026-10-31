# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.0
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY *.go openapi.json ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" -o /out/imgserver . \
    && mkdir -m 0700 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="imgserver" \
      org.opencontainers.image.description="Shared file uploads, avatars and private downloads with S3-compatible storage" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
WORKDIR /app
COPY --from=build /out/imgserver /app/imgserver
COPY --from=build --chown=65532:65532 /out/data /app/.data
USER 65532:65532
EXPOSE 18080
HEALTHCHECK --interval=30s --timeout=6s --start-period=10s --retries=3 \
    CMD ["/app/imgserver", "healthcheck", "-config", "/app/config.json"]
ENTRYPOINT ["/app/imgserver"]
CMD ["serve", "-config", "/app/config.json"]
