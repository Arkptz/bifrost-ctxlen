# Contributing

Thanks for contributing. This project ships a Nix dev shell and automated
quality gates so local checks match CI.

## Development setup

1. Install [Nix](https://nixos.org/download) with flakes enabled.
2. Enter the dev shell: `nix develop` (or `direnv allow` to load `.envrc`).

The shell provides the full toolchain and runs `lefthook install`, which wires
the pre-commit and commit-msg git hooks.

## Running checks locally

```bash
go build ./...
go test -race ./...
golangci-lint run ./...
gofumpt -l .
govulncheck ./...
```

## Running CI locally

```bash
./scripts/ci-local.sh             # all act-compatible jobs
./scripts/ci-local.sh --job fmt   # one job
```
