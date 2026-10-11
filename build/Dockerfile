# Multi-stage build for either binary. Select with --build-arg TARGET=primary|node.
# The build stage runs on the build host and cross-compiles for the target
# platform, so arm/v7 and arm64 images build without QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG TARGET=primary
# The primary uses its version as the default node image tag for provisioning,
# so release builds must pass the exact image version (e.g. 1.2.3).
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/app ./cmd/${TARGET}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/app /app
# Numeric UID:GID (distroless 'nonroot') so Config.User is identical on every
# arch; the string form resolved inconsistently across amd64/arm64 builds.
USER 65532:65532
ENTRYPOINT ["/app"]
# On primary this gates real run/sync confirmation (docs/requirements.md §9.2);
# on node it's liveness-only (no shell/curl in this distroless image).
HEALTHCHECK --interval=5m --timeout=10s --start-period=30s CMD ["/app", "healthcheck"]
