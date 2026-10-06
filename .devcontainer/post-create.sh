#!/usr/bin/env bash
# Runs once after the dev container is created. Tooling (Go, golangci-lint, age,
# minisign, gh) is already baked into the image (.devcontainer/Dockerfile), so
# this just warms the module cache and prints the toolchain versions.
set -euo pipefail

echo "==> Warming the Go module cache"
go mod download || true

echo "==> Toolchain:"
go version
golangci-lint version 2>/dev/null || true
age --version 2>/dev/null || true
command -v minisign >/dev/null && echo "minisign: installed" || true
gh --version 2>/dev/null | head -1 || true

echo "==> Ready. Try: make test"

