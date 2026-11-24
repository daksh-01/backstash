# Install

Install backstash on a Linux server (amd64 or arm64), configure one app, and schedule it.
Takes about ten minutes. For macOS or Windows, build from source (`make build`); there are no prebuilt binaries.

## What you need first

- A published release (a `v*` tag), because the installer downloads from it.
- An [Infisical](https://infisical.com) project with a machine identity that can read your backup secrets.
- An S3-compatible bucket (Cloudflare R2, AWS S3, MinIO) and its access keys.
- Docker access for the user running backups, if your databases or files live in containers.
- Optional: a Slack bot token and SMTP login for alerts (see [Alerts](alerts.md)).

## 1. Install the binary

```sh
curl -fsSL https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh | BACKSTASH_VERSION=v0.1.0 sh
backstash version
```

The script checks the CPU, downloads the binary and `checksums.txt`, refuses to install if the checksum
does not match, and installs to `/usr/local/bin` if writable, otherwise `~/.local/bin`.

| Variable                | Meaning                                                         |
| ----------------------- | --------------------------------------------------------------- |
| `BACKSTASH_VERSION`     | Release tag to install (default: latest). Pin it /clon servers. |
| `BACKSTASH_INSTALL_DIR` | Install somewhere else                                          |
| `BACKSTASH_REPO`        | `owner/name` to download from, for a fork                       |

To read the script first: `curl -fsSLO https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh`,
then `less install.sh && sh install.sh`. To install by hand, download `backstash_linux_<arch>` and
`checksums.txt` from the release page, run `sha256sum -c --ignore-missing checksums.txt`, then
`install -m 0755 backstash_linux_<arch> /usr/local/bin/backstash`.

If `gh` is installed and logged in, the script also verifies the build provenance. You can do it yourself:
`gh attestation verify backstash_linux_amd64 --repo daksh-01/backstash`.

## 2. Tools the host needs

| Target mode               | Needs on the host                                                      |
| ------------------------- | ---------------------------------------------------------------------- |
| Anything in `docker` mode | Docker, and permission to run it                                       |
| Postgres in `tcp` mode    | `pg_dump` and `pg_restore` of the **same major version as the server** |
| Redis in `tcp` mode       | `redis-cli`                                                            |
| Files in `path` mode      | `tar`                                                                  |

Docker mode uses the container's own tools, so there is no version to match.

## 3. Create the config

```sh


```

`~/.config/backstash/config.yml` holds settings shared by every app on the host:

```yaml
s3: { endpoint: "https://<account-id>.r2.cloudflarestorage.com", region: auto }
slack: { channel: C0123456789 }
smtp: { host: smtp.example.com, port: 587 }
alert_email_to: you@example.com
retention_days: 30
```

One file per app and environment, `~/.config/backstash/apps.d/myapp-prod.yml`:

```yaml
app: myapp
bucket: myapp-backups
secrets_path: /backstash/myapp-prod
postgres:
  { mode: docker, container: myapp-postgres-1, user: myapp, database: myapp }
redis: { mode: docker, container: myapp-redis-1 }
files:
  {
    mode: docker,
    container: myapp-app-1,
    path: /var/lib/myapp/data,
    label: uploads,
  }
```

Keep only the targets you use. Use `docker ps` for the exact container names. Every key is explained in
[Configuration](configuration.md); typos are rejected with a clear error.

To keep the config somewhere else, pass `--config-dir DIR` or set `BACKSTASH_CONFIG_DIR`.

## 4. Secrets

Secrets are never in these files. They live in Infisical and are fetched on every run.

Create `~/.config/backstash/identity.env` and lock it down, since it is the only secret on the disk:

```sh
cat > ~/.config/backstash/identity.env <<'ENV'
INFISICAL_UNIVERSAL_AUTH_CLIENT_ID=...
INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET=...
INFISICAL_PROJECT_ID=...
ENV
chmod 600 ~/.config/backstash/identity.env
```

Give that identity **read-only** access to the folders below and nothing else. Then add these secrets:

| Folder                  | Secret                                     | Needed for                                       |
| ----------------------- | ------------------------------------------ | ------------------------------------------------ |
| `/backstash/shared`     | `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | the bucket (read, write, delete)                 |
| `/backstash/shared`     | `SLACK_BOT_TOKEN`                          | Slack cards (skipped if unset)                   |
| `/backstash/shared`     | `SMTP_USER`, `SMTP_PASSWORD`               | failure emails                                   |
| `/backstash/myapp-prod` | `POSTGRES_PASSWORD`                        | Postgres in `tcp` mode (optional in docker mode) |
| `/backstash/myapp-prod` | `REDIS_PASSWORD`                           | Redis, if it has a password                      |

A secret in the app's folder wins over the same name in the shared folder.

## 5. Check it, then run it once

```sh
backstash validate myapp     # config, secrets, tools, running containers, bucket. Writes nothing.
backstash backup myapp
backstash list myapp
```

Fix whatever `validate` marks `FAIL` before going on. A good `backup` prints one line per target and posts a
Slack card.

## 6. Schedule it

**cron** (simplest):

```sh
(crontab -l 2>/dev/null; echo '0 2 * * * /usr/local/bin/backstash backup myapp >> ~/.config/backstash/myapp-prod.log 2>&1') | crontab -
```

Use the full path to the binary, because cron's `PATH` is minimal. Cron uses the server's time zone. The
exit code is `0` on success and `1` if any target failed.

**systemd timer** (catches up if the server was off at 02:00). Save as `/etc/systemd/system/backstash@.service`:

```ini
[Unit]
Description=backstash backup of %i

[Service]
Type=oneshot
Environment=BACKSTASH_CONFIG_DIR=/etc/backstash
ExecStart=/usr/local/bin/backstash backup %i
```

and `/etc/systemd/system/backstash@.timer`:

```ini
[Unit]
Description=Daily backstash backup of %i

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true
RandomizedDelaySec=10min

[Install]
WantedBy=timers.target
```

Move the config folder to `/etc/backstash` (`chmod 700`), then `sudo systemctl enable --now backstash@myapp.timer`
and check with `systemctl list-timers` and `journalctl -u backstash@myapp`.

## Restore

```sh
backstash restore myapp postgres        # pick from a list; you must type the database name
```

Stop the app first. [Operations](operations.md) covers restoring Postgres, files and Redis safely.

## Upgrade

Run the install command again with a newer `BACKSTASH_VERSION`, then `backstash validate myapp`. Config is not
versioned with the binary.

## Uninstall

Remove the cron line (or `systemctl disable --now backstash@myapp.timer`), then
`rm /usr/local/bin/backstash` and `rm -r ~/.config/backstash`. Backups already in your bucket are untouched.

## If something fails

| Message                                            | Likely cause                                                                            |
| -------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `download failed`                                  | No release for that version yet, or the repo is private                                 |
| `checksum mismatch`                                | Corrupted or tampered download: nothing was installed. Retry; if it persists, report it |
| `infisical identity incomplete`                    | `identity.env` missing, or not in the config folder                                     |
| `secrets not found in Infisical: S3_ACCESS_KEY_ID` | Wrong folder name, or the identity cannot read it                                       |
| `container X exists but is not running`            | Wrong container name in the app file                                                    |

More in [Operations](operations.md#troubleshooting).
