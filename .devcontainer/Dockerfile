# Dev container image for offsite-backup-manager.
#
# Based on the official Go dev container, but we install our tooling here (rather
# than via dev container "features") so we control apt. The upstream image ships
# a yarn apt source whose signing key has expired, which breaks `apt-get update`;
# we remove it first.
FROM mcr.microsoft.com/devcontainers/go:1.25-bookworm

# Drop the broken yarn apt source (expired key NO_PUBKEY 62D54FD4003F6525).
RUN find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
      -exec grep -lE 'dl\.yarnpkg\.com' {} + 2>/dev/null | xargs -r rm -f || true

# CLI tools used to manually verify packages the orchestrator produces.
RUN apt-get update \
 && apt-get install -y --no-install-recommends age minisign \
 && rm -rf /var/lib/apt/lists/*

# golangci-lint v2 (prebuilt binary; independent of the image's Go version, and
# matches the v2 config in .golangci.yml / the v7 action used in CI).
RUN curl -fsSL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
      | sh -s -- -b /usr/local/bin

# GitHub CLI, installed from the release tarball (no extra apt repo/keys).
RUN set -eux; \
    tag="$(curl -fsSL https://api.github.com/repos/cli/cli/releases/latest \
      | grep -m1 '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"; \
    ver="${tag#v}"; \
    curl -fsSL -o /tmp/gh.tgz \
      "https://github.com/cli/cli/releases/download/${tag}/gh_${ver}_linux_amd64.tar.gz"; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m0755 "/tmp/gh_${ver}_linux_amd64/bin/gh" /usr/local/bin/gh; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${ver}_linux_amd64"
