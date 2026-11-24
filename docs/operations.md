# Operations

## Install on a host

See [Install](install.md): the installer, the config files, secrets, and scheduling.

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
