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

## Production safeguards

- **Heartbeat.** Set `heartbeat_url` to a [healthchecks.io](https://healthchecks.io)-style check. Every backup
  pings it (`/fail` on failure). Slack and email only fire when a run happens; the heartbeat also catches cron
  not firing, the host being down, or the binary being gone. Set the check's period to your schedule plus slack.
- **Verify.** Run `backstash verify <app>` weekly (cron). It downloads the newest backup, decrypts it if needed,
  and checks it: `pg_restore --list`, the RDB header, a full read of the tar.gz. It also fails a backup under half
  the recent median size. It is a structural check, not a trial restore: do a real restore drill now and then.
- **Encryption.** Set `age_recipient` to a public key (`age-keygen` makes the pair). Only the public key is on the
  host. Store the private key offline and, to restore, supply it as `BACKUP_AGE_IDENTITY`. **Lose the private key
  and the backups are unrecoverable.** Encrypted files end in `.age`; retention treats them like any backup.
- **Safe pruning.** A prune that would delete more than `max_prune` (default 10) backups refuses, so a typo in
  `retention_days` or a wrong clock cannot wipe your history. Preview with `prune --dry-run`; override with
  `--force`. For a hard backstop, turn on bucket versioning or object lock in the bucket itself.
- **Disk check.** Before each dump it needs free space in the staging dir: twice the last backup, at least 256 MB.
- **Size warning.** A backup under half the recent median is uploaded but flagged in Slack.
- **Metrics.** With `metrics_dir`, each backup writes `backstash_<app>_<env>.prom`: `backstash_run_timestamp_seconds`,
  `backstash_run_success`, `backstash_run_duration_seconds`, `backstash_target_success`, `backstash_target_size_bytes`.
  Alert on `time() - backstash_run_timestamp_seconds > 90000`.
- **Secrets cache.** With `secrets_cache_days: N`, a provider outage no longer stops backups: the last good
  secrets (up to N days old) are used and a warning is logged. The cache is sealed, but a key derived from the
  identity opens it, so treat it like `identity.env`. A revoked identity keeps working until the cache expires.
- **Upload retry.** A failed upload is retried twice (10s, 20s backoff), re-sending the file each time.

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
