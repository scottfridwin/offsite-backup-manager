# Security Policy

This project handles **backups and cryptographic material** (asymmetric package
encryption with `age`, manifest signing with `minisign`, per-node auth tokens, and
WORM guarantees). Security issues are taken seriously.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Please report privately via GitHub's
[security advisories](https://github.com/scottfridwin/offsite-backup-manager/security/advisories/new)
("Report a vulnerability"). Include:

- a description of the issue and its impact,
- steps to reproduce or a proof of concept,
- affected version/commit, and
- any suggested remediation.

You can expect an acknowledgement within a few days. Please allow reasonable time
for a fix before any public disclosure.

## Scope / areas of particular concern

- Encryption and key custody (the private identity must never be derivable from, or
  present on, the Primary or any node).
- The WORM guarantee (the Primary must have no path to delete or modify node data).
- Node authentication and enrollment (token issuance, rotation, revocation).
- Supply chain (build/release pipeline, image provenance).

## Notes

Because much of this code is AI-generated, security-sensitive areas receive extra
human review. If you spot something that looks AI-plausible but wrong, please flag
it — that feedback is especially valuable here.
