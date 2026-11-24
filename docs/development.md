# Development

```sh
make check          # gofmt, golangci-lint, go vet, go test -race: what a PR must pass
make cover          # tests with the coverage floor (MIN_COVER, default 75%)
make integration    # real Postgres/Redis/S3; needs the services below
make build VERSION=v0.0.0-local
```

Needs Go (see `go.mod`) and [golangci-lint](https://golangci-lint.run) v2 (`brew install golangci-lint`).
The unit tests need no Docker, databases or network: they use a fake command runner, an in-memory
bucket, `httptest` fakes for Slack, Infisical and S3, and a small SMTP stub.

## Layout

| Package                                                 | Responsibility                                                                                  |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| `cmd/backstash`                                         | Argument parsing and one handler per command                                                    |
| `internal/config`                                       | `config.yml` and `apps.d/*.yml` loading and validation; default secret names                    |
| `internal/secrets`                                      | AWS Secrets Manager and Infisical (REST), plus the last-good-secrets cache                      |
| `internal/execx`                                        | Runs external tools; `InContainer` is the one place that builds `docker exec`; `Fake` for tests |
| `internal/target`                                       | Postgres, Redis and files dump and restore                                                      |
| `internal/storage`, `internal/retention`                | The bucket (with upload retry) and pruning                                                      |
| `internal/encrypt`                                      | age encryption of backups                                                                       |
| `internal/report`, `internal/notify`, `internal/runlog` | Run results, Slack and email, log capture                                                       |
| `internal/run`                                          | Orchestration (`run.go`), wiring (`wire.go`), alerts (`notifier.go`), preflight (`validate.go`) |
| `internal/integration`                                  | Build-tagged tests against real services                                                        |

New external-tool calls go through `execx.Runner` (and `execx.InContainer` for containers) so they
stay testable, and secrets travel in `Cmd.Env`, never argv. Keep dependencies minimal.

## CI (`.github/workflows/ci.yml`)

Runs on every PR and on pushes to `main`. A new push cancels the superseded PR run.

| Job                  | What it proves                                                                                                                                                                                 |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lint                 | `gofmt` clean; golangci-lint (`.golangci.yml`)                                                                                                                                                 |
| Test (ubuntu, macOS) | `go vet`, `go test -race`, total coverage at or above `MIN_COVER`; coverage profile uploaded                                                                                                   |
| Integration          | The real `pg_dump`/`pg_restore`, `redis-cli --rdb`, `docker cp`, `tar` and S3 client against Postgres 16, Redis 7 and an S3 mock, in docker and tcp mode, including restore and a failing dump |
| Vulnerability scan   | `govulncheck` over the module and the Go toolchain                                                                                                                                             |
| Build                | Cross-compiles linux amd64/arm64 and darwin/arm64; smoke-tests that the binary reports the commit                                                                                              |
| CI passed            | One stable check to require in branch protection                                                                                                                                               |

Run the integration tests locally by starting the same containers and exporting the `IT_*` variables
shown in the `integration` job. Without them every test skips.

Raise `MIN_COVER` in the `Makefile` as coverage improves; never lower it to land a change.

## Releasing

```sh
git tag v0.1.0 && git push --tags      # or v0.1.0-rc.1 for a pre-release
```

`.github/workflows/release.yml` then:

1. **Verify**: the tag is semver, the commit is on `main`, tests pass.
2. **Build** linux amd64 and arm64 binaries with the tag baked into `backstash version`.
3. **Publish**: `checksums.txt`, a signed build-provenance attestation, and a GitHub release with
   generated notes (a `-suffix` makes it a pre-release).
4. **Smoke**: downloads the published assets, checks the checksums, runs the binary, and verifies the
   attestation with `gh attestation verify`.

The asset names (`backstash_linux_amd64`, `backstash_linux_arm64`, `checksums.txt`) are a stable contract for
anything that installs releases: `checksums.txt` is `sha256sum` output for each binary, so an installer can
download the binary plus the checksums and refuse a mismatch. Releasing does not deploy anything.

## Repository settings to turn on once

- Branch protection on `main`: require the **CI passed** check and a pull request.
- Settings, Actions, General: workflow permissions can stay read-only; the release job asks for the
  few extra permissions it needs itself.
- Dependabot (`.github/dependabot.yml`) opens weekly grouped PRs for Go modules and actions.
