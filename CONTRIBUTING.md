# Contributing

Thanks for helping. Bug reports, docs fixes and pull requests are all welcome.

## Before you start

For anything bigger than a small fix, open an issue first so we can agree on the approach.

## Setup

Needs Go (version in `go.mod`) and [golangci-lint](https://golangci-lint.run) v2.

```sh
make check          # gofmt, golangci-lint, go vet, go test -race: what a PR must pass
make cover          # coverage floor; never lower MIN_COVER to land a change
make integration    # real Postgres/Redis/S3, optional; see docs/development.md
```

## Pull requests

- Keep each PR to one change, with tests. Unit tests use `execx.Fake` and in-memory fakes: no Docker needed.
- New external-tool calls go through `execx.Runner`. Secrets go in `Cmd.Env`, never argv.
- Keep dependencies minimal; explain any new one in the PR.
- Update `docs/` when behavior or config changes. Keep the README short.
- Do not rename release assets (`checksums.txt`, binary names): installers depend on them.

Package layout and CI details are in [docs/development.md](docs/development.md).

By contributing you agree your work is released under the [MIT license](LICENSE).
