# Multi-stage build for either binary. Select with --build-arg TARGET=primary|node.
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-bookworm AS build
ARG TARGET=primary
ARG VERSION=dev
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/app ./cmd/${TARGET}

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/app /app
# Numeric UID:GID (distroless 'nonroot') so Config.User is identical on every
# arch; the string form resolved inconsistently across amd64/arm64 builds.
USER 65532:65532
ENTRYPOINT ["/app"]
# On primary this gates real run/sync confirmation (docs/requirements.md §9.2);
# on node it's liveness-only (no shell/curl in this distroless image).
HEALTHCHECK --interval=5m --timeout=10s --start-period=30s CMD ["/app", "healthcheck"]
