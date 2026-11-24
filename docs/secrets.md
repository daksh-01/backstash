# Secrets

BackStash reads every secret from AWS Secrets Manager on each run. Nothing sensitive goes in `config.yml`
or `apps.d/*.yml`. (Infisical is also supported: see [Configuration](configuration.md#secrets-infisical).)

## 1. Create the secrets

Each path is the name of one Secrets Manager secret of type **Other type of secret / key-value**, whose
JSON holds the names below. BackStash reads `shared_secrets_path` first, then the app's `secrets_path`;
the app's value wins on a clash.

```sh
aws secretsmanager create-secret --name /backstash/shared --secret-string '{
  "S3_ACCESS_KEY_ID": "...",
  "S3_SECRET_ACCESS_KEY": "...",
  "SLACK_BOT_TOKEN": "xoxb-...",
  "SMTP_USER": "...",
  "SMTP_PASSWORD": "..."
}'

aws secretsmanager create-secret --name /backstash/myapp-prod --secret-string '{
  "POSTGRES_PASSWORD": "...",
  "REDIS_PASSWORD": "..."
}'
```

| Secret                                     | Used for                                          |
| ------------------------------------------ | ------------------------------------------------- |
| `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | The bucket. Needs read, write and delete          |
| `POSTGRES_PASSWORD`, `REDIS_PASSWORD`      | Database passwords (names overridable per target) |
| `SLACK_BOT_TOKEN`                          | Slack alerts. Skipped without it                  |
| `SMTP_USER`, `SMTP_PASSWORD`               | Failure email login                               |
| `BACKUP_AGE_IDENTITY`                      | Only to restore or verify encrypted backups       |

## 2. Turn it on

```yaml
# config.yml
secrets_provider: aws
shared_secrets_path: /backstash/shared
```

and set `secrets_path: /backstash/myapp-prod` in the app file.

## 3. Give the host credentials

`AWS_REGION` is required. Then pick one:

- **A role (recommended).** On EC2, ECS or EKS attach a role; with neither key variable set, BackStash uses the
  AWS default chain (instance or task role, SSO, `~/.aws`). No key is stored on the host.
- **Static keys.** Put `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (optionally `AWS_SESSION_TOKEN`) in the
  environment or in `<config-dir>/identity.env` (mode `0600`). Setting only one of the two is an error.

`AWS_ENDPOINT_URL` is optional (for LocalStack or a VPC endpoint).

## 4. Least-privilege policy

The backup identity only reads, and only under `/backstash/`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": "arn:aws:secretsmanager:<region>:<account-id>:secret:/backstash/*"
    }
  ]
}
```

Keep `BACKUP_AGE_IDENTITY` in a secret this identity cannot read, so a stolen host cannot decrypt old backups.

## Outages

With `secrets_cache_days: N` the last good secrets are reused for up to N days if Secrets Manager is
unreachable. The cache is unavailable with role credentials. See [Operations](operations.md).

Check everything with `backstash validate <app>`.
