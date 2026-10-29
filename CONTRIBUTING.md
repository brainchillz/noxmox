# Contributing

## Repository hooks

The hooks in `.githooks/` keep private material out of the public history:

- `pre-commit` refuses to stage anything under `planning/` or any
  credentials file (`proxmox-creds`, `*.creds`, `.env*`).
- `pre-push` refuses to push any ref that contains those files, and only
  lets `main` and `v*` tags go to GitHub.

Git does not clone hooks, so enable them once per checkout:

    git config core.hooksPath .githooks

## Building and testing

    go vet ./...
    go test ./...
    go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o bin/nox ./cmd/nox
    perl -I test/pve-stubs -c pve/NoxPlugin.pm

The Go build needs `libsanlock-dev` (the cgo binding links libsanlock
dynamically). The Perl syntax check uses the stubs in `test/pve-stubs/`
so it runs on a machine without Proxmox; on a PVE node use
`perl -I/usr/share/perl5 -c` instead.

CI runs the same four commands on every push and pull request, on GitHub
Actions (`.github/workflows/ci.yml`) and on GitLab (`.gitlab-ci.yml`).

## Releases

Releases are annotated tags `vX.Y.Z` on `main`. The version is embedded in
the binary at build time through `-ldflags -X main.version=...` and shown by
`nox version` and `nox status`.

## License

By contributing you agree that your contributions are licensed under the
Apache License 2.0 (see `LICENSE`).
