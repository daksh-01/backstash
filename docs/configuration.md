# Configuration

Non-secret settings live in YAML. Secrets live in Infisical and are fetched on every run. Unknown
YAML keys are an error, and every problem in a file is reported at once.

## Example

```
~/.config/backstash/
  config.yml               host-wide defaults: S3 endpoint, Slack channel, SMTP host, alert email
  identity.env             Infisical machine identity (the only secret on disk, 0600)
  apps.d/myapp-prod.yml  one file per app and environment
```

```yaml
# apps.d/myapp-prod.yml
app: myapp
bucket: myapp-backups
secrets_path: /backstash/myapp-prod
retention_days: 30
postgres: {mode: docker, container: myapp-prod-postgres-1, user: myapp, database: myapp}
redis:    {mode: docker, container: myapp-prod-redis-1}
files:    {mode: docker, container: myapp-prod-myapp-1, path: /var/lib/myapp/data, label: myapp-uploads}
```

## `config.yml` (host-wide defaults)

| Key | Default | Meaning |
|---|---|---|
| `s3.endpoint` | AWS S3 | R2: `https://<account-id>.r2.cloudflarestorage.com` |
| `s3.region` | `auto` | Use a real region such as `us-east-1` for AWS S3 |
| `slack.channel` | | Channel id (`C0123456789`, survives renames) or name |
| `smtp.host` / `smtp.port` | / `587` | Port 465 uses implicit TLS |
| `smtp.tls` | `true` | STARTTLS on any port other than 465 |
| `smtp.from` | `SMTP_USER` | Must be an address your provider lets you send as |
| `alert_email_to` | | Who gets failure emails. Required if `smtp.host` is set |
| `retention_days` | `30` | Host default. `0` turns pruning off |
| `shared_secrets_path` | `/backstash/shared` | Infisical folder every app reads first |
| `staging_dir` | `~/.local/state/backstash/staging` | Private (0700) local folder backups are written to before upload |

## `apps.d/<app>-<env>.yml`

| Key | Required | Meaning |
|---|---|---|
| `app` | yes | Lowercase letters, digits, dashes. Part of the storage path |
| `environment` | no (`prod`) | Same rules. Selected with `--env` |
| `bucket` | yes | One bucket per app |
| `secrets_path` | yes | Infisical folder for this app. Wins over the shared folder |
| `retention_days` | no | Overrides the host value. `0` disables pruning for this app |
| `slack_channel` | no | Overrides the host channel |

### `postgres`

| Key | Meaning |
|---|---|
| `mode` | `docker` runs `pg_dump` inside the container (no host client or published port needed). `tcp` runs the host's `pg_dump` against `host:port`, for host-local or remote servers; its major version must **match the server's** (a newer client writes dumps an older server cannot restore). Docker mode always matches, because it uses the container's own tools |
| `container` | Container name (docker mode) |
| `host`, `port` | tcp mode; port defaults to `5432` |
| `user`, `database` | Required |
| `password_secret` | Secret holding the password, default `POSTGRES_PASSWORD`. Required in tcp mode, optional in docker mode (the socket inside the official image trusts local connections) |

### `redis`

| Key | Meaning |
|---|---|
| `mode`, `container`, `host`, `port` | As above; port defaults to `6379`. tcp mode needs `redis-cli` on the host |
| `password_secret` | Default `REDIS_PASSWORD`. Optional |

A snapshot is taken with `redis-cli --rdb`, which does not touch the server's own data directory.

### `files`

| Key | Meaning |
|---|---|
| `mode` | `docker` (tar inside a running container, which needs `tar` in the image) or `path` (a folder on this host) |
| `container` | Docker mode |
| `path` | Absolute folder to archive |
| `label` | Archive name prefix, default `files`. Letters, digits, `_ . -` |

## Secrets (Infisical)

Read from `shared_secrets_path` and then the app's `secrets_path`; the app's value wins on a clash.

| Secret | Used for |
|---|---|
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | The bucket. Needs read, write and delete |
| `POSTGRES_PASSWORD`, `REDIS_PASSWORD` | Database passwords (names overridable per target) |
| `SLACK_BOT_TOKEN` | Bot User OAuth Token (`xoxb-...`). Slack is skipped without it |
| `SMTP_USER`, `SMTP_PASSWORD` | Failure email login |

The Infisical identity comes from `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID`, `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET`
and `INFISICAL_PROJECT_ID` (optionally `INFISICAL_URL`, `INFISICAL_ENV`, default `prod`). If they are
not in the environment they are read from `<config-dir>/identity.env`. Give this identity **read-only
access to `/backstash/**` and nothing else**.

## Bucket layout

```
<bucket>/<app>/<env>/postgres/<database>-YYYYmmdd-HHMMSS.dump
<bucket>/<app>/<env>/redis/redis-YYYYmmdd-HHMMSS.rdb
<bucket>/<app>/<env>/files/<label>-YYYYmmdd-HHMMSS.tar.gz
```

Retention only ever looks inside one `<app>/<env>/<kind>/` folder.
