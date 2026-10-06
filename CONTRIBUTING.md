# Contributing

Thanks for your interest. This is an **AI-driven project**: most code is generated
by AI (GitHub Copilot) under human direction and review. The workflow below keeps
that process safe and the history clean.

## Golden rules

- **`main` is protected.** No direct pushes. Every change lands via a reviewed pull
  request with green CI.
- **Small, focused PRs.** One logical change per PR; easier to review (especially
  AI-generated code).
- **Never commit secrets.** No tokens, keys, `age` identities, or `minisign` secret
  keys. CI runs a secret scan; `.gitignore` blocks the common cases.

## Branching model

Trunk-based with short-lived branches off `main`:

- `feat/<short-desc>` — new functionality
- `fix/<short-desc>` — bug fixes
- `docs/<short-desc>` — documentation only
- `chore/<short-desc>` — tooling, CI, deps, housekeeping
- `refactor/<short-desc>` — behaviour-preserving changes

Branch from the latest `main`, push, open a PR, keep it up to date with `main`.

## Commits

- Follow [Conventional Commits](https://www.conventionalcommits.org/):
  `type(scope): summary` — e.g. `feat(primary): add age encryption of run package`.
  Types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`, `build`, `perf`.
- **Sign your commits** (`git commit -S`). Signed commits are required on `main`.
- Write imperative, present-tense summaries ("add", not "added").

## Pull requests

1. Ensure CI is green (build, vet, test, lint, secret scan).
2. Update docs/tests alongside code changes.
3. Fill out the PR template; call out any security-relevant changes.
4. Resolve all review conversations.
5. **Squash-merge** into `main` (linear history), then delete the branch.

## Local checks before pushing

Once the Go module exists:

```bash
go build ./...
go vet ./...
go test ./...
# golangci-lint run   # if installed
```

## Releasing

Push a `vX.Y.Z` tag on `main` (e.g. `git tag -s v0.1.0 && git push origin v0.1.0`)
to trigger [`.github/workflows/release.yml`](.github/workflows/release.yml),
which builds and publishes both multi-arch images to GHCR tagged with that
exact version (no `latest`).

## Code of conduct

Be respectful and constructive. Assume good faith.
