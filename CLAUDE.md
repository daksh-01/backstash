# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`backstash`: a Go CLI that backs up Postgres, Redis and app file folders (each in Docker, on the host,
or remote) to S3-compatible storage, prunes old backups per app, posts a Slack card per run and
emails `run.log` on failure. Secrets are read from Infisical at run time. It shells out to
`pg_dump`/`pg_restore`/`redis-cli`/`docker`/`tar`; there are no database drivers.

User docs: `README.md` is a short overview; everything else is in `docs/` (usage, config reference, flow diagrams, operations and troubleshooting, development). Keep the README short and put detail in `docs/`.

## Commands

```sh
make check                               # gofmt, golangci-lint, go vet, go test -race (what a PR must pass)
make cover                               # coverage floor, MIN_COVER in the Makefile (never lower it to land a change)
make integration                         # real services; see docs/development.md
go test ./internal/run -run TestBackup   # one test
```

CI is `.github/workflows/ci.yml`. Release: push a `v*` tag; `release.yml` builds linux amd64/arm64,
`checksums.txt` and a provenance attestation. Those asset names are a stable contract for installers
(`checksums.txt` is `sha256sum` output), so do not rename them.

## Architecture

`cmd/backstash/` handles CLI entrypoint and OS signals (`main.go`), command dispatch and options (`cli.go`), and console output rendering (`render.go`); the logic is in `internal/`:

- `config`: `config.yml` + `apps.d/*.yml` loading, validation (all errors at once, unknown keys rejected),
  retention precedence, `Prefix(kind)` = `<app>/<env>/<kind>/`.
- `secrets`: Infisical over REST (universal auth); `Set.String()` never prints values.
- `execx`: `Runner` interface, real `Exec` and a recording `Fake`. `InContainer` is the only place that builds
  `docker exec`. Secrets go in `Cmd.Env` (forwarded by name), never argv.
- `target`: `DumpPostgres/Redis/Files` and restores. Stage to a 0700 dir as 0600 files; a failed or empty
  dump deletes its file.
- `storage`: `Store` interface + S3 client (checksums only `WhenRequired` so R2/MinIO accept it).
- `retention`: `Expired` (pure), `Prune`, `PruneLocal`.
- `report`, `notify` (`slack.go`, `mail.go`), `runlog` (slog tee into a buffer for the email attachment).
- `run`: `run.go` orchestration (`Backup`, `Restore`, `List`, `Prune`), `wire.go` (`NewStore`), `notifier.go`,
  `validate.go` (preflight checks).

Tests use `execx.Fake`, an in-memory store, `httptest` fakes for Slack/Infisical/S3, and a tiny SMTP stub:
no Docker or databases needed.

## Behaviors to preserve

Targets are independent (one failing never skips the others). A prune failure never fails a stored
backup. An upload failure keeps the local file and names its path. Retention deletes only names
matching `<label>-YYYYmmdd-HHMMSS.{dump,rdb,tar.gz}`, never the newest backup in a folder, and
`retention_days: 0` disables it. Postgres restore is one transaction with a 30s lock timeout. Redis is
never auto-restored. Cancelled/invalid/no-terminal restores are `Silent` (printed, no alerts). A panic in a
target becomes that target's failure so alerts still fire. Notification failures are logged, never fatal.

## Conventions

`gofmt` and golangci-lint clean, stdlib first. Keep dependencies minimal (the Infisical SDK was rejected for
pulling in ~60 transitive modules). New external-tool calls go through `execx.Runner` so they stay testable.
The S3 upload path must keep request checksums at `WhenRequired` (R2 and MinIO reject the default): a test
guards it.
