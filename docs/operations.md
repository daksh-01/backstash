# Operations

## Install on a host

```sh
curl -fsSL https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh | sh
```

The script picks the right Linux binary (amd64 or arm64), downloads it with `checksums.txt`, refuses to
install if the checksum does not match, and puts `backstash` in `/usr/local/bin` if it is writable,
otherwise `~/.local/bin`. If `gh` is installed and logged in it also verifies the build provenance.
To read it before running it, download it first: `curl -fsSLO <url> && less install.sh && sh install.sh`.

| Variable | Meaning |
|---|---|
| `BACKSTASH_VERSION` | Release tag to install, such as `v0.1.0` (default: latest) |
| `BACKSTASH_INSTALL_DIR` | Where to put the binary |
| `BACKSTASH_REPO` | `owner/name` to download from, for a fork |

Pin a version for servers: `curl -fsSL <url> | BACKSTASH_VERSION=v0.1.0 sh`. Run it again to upgrade.
There is no macOS or Windows binary: build from source with `make build`.

Then create the config (see [Configuration](configuration.md)) and run `backstash validate`.
By hand instead of the script: download the binary and `checksums.txt` from the release page, run
`sha256sum -c --ignore-missing checksums.txt`, and `install -m 0755` the binary.

Docker-mode targets need nothing else on the host except Docker access for the running user.
tcp-mode Postgres needs `pg_dump`/`pg_restore` of the **same major version as the server** (a newer
client writes dumps an older server cannot restore); tcp-mode Redis
needs `redis-cli`.

## Schedule

```
0 2 * * * ~/bin/backstash backup myapp >> ~/.config/backstash/myapp-prod.log 2>&1
```

Cron uses the server's time zone and a minimal `PATH`: use full paths. The exit code is `0` on
success and `1` if any target failed.

## Check a backup worked

- A green Slack card, or the lines `postgres: <file> (<size>)` in the cron log.
- `backstash list myapp`.
- Now and then, restore the latest backup into a scratch database. A backup you have never restored
  is a hope, not a backup.

## Restore

```sh
backstash restore myapp postgres                                   # choose from a list
backstash restore myapp postgres myapp-20261122-020000.dump --yes
backstash restore myapp files
backstash restore myapp redis                                      # downloads, prints the steps
```

- Postgres and files **overwrite** their target. You must type its name (the database, or the
  container/path for files) unless you pass `--yes`.
- Stop the application first. A Postgres restore needs table locks; with the app connected it waits
  30 seconds and fails. It runs in one transaction, so a failure leaves the database as it was, and
  the download stays in the staging folder for a retry.
- Files in a restored archive overwrite existing ones; others are kept. Restore Postgres and files
  from the same time: they are captured seconds apart, not atomically.
- Redis is never overwritten for you. The command downloads the RDB and prints the stop, copy and
  start steps (and how to deal with AOF).

## Upgrade

Run the install command again (it replaces the binary in place), then `backstash validate` to confirm
your config still loads. Config files are not versioned with the binary.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `infisical identity incomplete, missing: ...` | `identity.env` is missing or not in the config dir (or pass `--config-dir`). |
| `infisical login: HTTP 401` | Wrong client id or secret. |
| `read secrets from /x: HTTP 404` | The folder path is wrong, or the identity cannot see it. |
| `secrets not found in Infisical: S3_ACCESS_KEY_ID` | The secret was not renamed (see above) or is in the wrong folder. |
| `could not run pg_dump (is it installed?)` | tcp mode without the client installed, or not on cron's `PATH`. |
| `pg_dump failed ... server version mismatch` | The host client is older than the server. Use docker mode or install a newer client. |
| `container X exists but is not running` / `No such object` | Wrong container name in the YAML. Check `docker ps`. |
| `upload ...` errors | Wrong bucket, endpoint or keys, or no write access. The dump stays in the staging folder. |
| `not_in_channel` | The Slack bot is not in the channel: `/invite @Backup Alerts`. |
| `channel_not_found` | Wrong channel id, or a private channel the bot has not joined. |
| `invalid_auth` | The Slack token is wrong or revoked. |
| No failure email | SMTP host or `alert_email_to` missing, or the provider rejects the sender. The reason is in the log. |
