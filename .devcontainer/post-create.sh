#!/usr/bin/env bash
# Provision developer tooling for the offsite-backup-manager dev container.
# Installs the Go linter and the age/minisign CLIs used to manually verify
# packages produced by the orchestrator.
set -euo pipefail

echo "==> Installing golangci-lint"
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

echo "==> Installing age (encryption) and minisign (signing) CLIs"
sudo apt-get update -y
sudo apt-get install -y --no-install-recommends age minisign

echo "==> Warming the Go module cache"
go mod download || true

echo "==> Done. Try: make test"
