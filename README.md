# BackStash

Backs up **Postgres**, **Redis** and **app file folders** to S3-compatible storage (Cloudflare R2,
AWS S3, MinIO). It prunes old backups per app, posts a Slack card for every run, and emails the run log
when something fails. Secrets are read from [AWS Secrets Manager](https://aws.amazon.com/secrets-manager/)
at run time, so no database or bucket password sits in a config file.

- One static Go binary, no runtime to install; shells out to `pg_dump`, `pg_restore`, `redis-cli`, `docker`, `tar`
- Each target runs in Docker, on the host, or on a remote server
- One bucket per app, one folder per environment and kind
- Targets are independent: one failing never skips the others
- Optional [age](https://age-encryption.org) encryption, `verify` of the newest backup, safe pruning with a delete cap
- Dead-man's-switch heartbeat and Prometheus textfile metrics

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh | sh
```

1. Store your secrets (S3 keys, database passwords, Slack token) as a key/value secret in AWS Secrets Manager.
   See [Secrets](docs/secrets.md).
2. Point `config.yml` at it and describe one app:

   ```yaml
   # ~/.config/backstash/config.yml
   secrets_provider: aws
   shared_secrets_path: /backstash/shared
   ```

   ```yaml
   # ~/.config/backstash/apps.d/myapp-prod.yml
   app: myapp
   bucket: myapp-backups
   secrets_path: /backstash/myapp-prod
   postgres:
     {
       mode: docker,
       container: myapp-prod-postgres-1,
       user: myapp,
       database: myapp,
     }
   ```

3. Check it, then run it:

   ```sh
   backstash validate myapp    # config, secrets, tools and the bucket; writes nothing
   backstash backup myapp      # back up everything configured for the app
   ```

Schedule `backup` with cron or a systemd timer; see [Install](docs/install.md).

## Documentation

| Page                                          | Read it for                                                         |
| --------------------------------------------- | ------------------------------------------------------------------- |
| [Install](docs/install.md)                    | Put it on a server, configure an app, schedule it                   |
| [Secrets](docs/secrets.md)                    | AWS Secrets Manager setup, secret names, IAM policy, credentials    |
| [Configuration](docs/configuration.md)        | Every config key, secret name, and the bucket layout                |
| [Usage](docs/usage.md)                        | Commands, flags and exit codes                                      |
| [How it works](docs/how-it-works.md)          | Flow diagrams: backup, restore, retention, alerts                   |
| [Operations](docs/operations.md)              | Restore safely, encryption, pruning, metrics, upgrade, troubleshoot |
| [Alerts](docs/alerts.md)                      | The Slack card, the failure email, and the Slack app manifest       |
| [Deploy with Ansible](docs/ansible-deploy.md) | Install and configure it on many servers                            |
| [Development](docs/development.md)            | Tests, package layout, releasing                                    |

## Contributing and security

Issues and pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). To report a vulnerability,
see [SECURITY.md](SECURITY.md); please do not open a public issue for it.

## License

MIT, see [LICENSE](LICENSE).
