# BackStash

Backs up **Postgres**, **Redis** and **app file folders** to S3-compatible storage (Cloudflare R2,
AWS S3, MinIO). It prunes old backups per app, posts a Slack card for every run, and emails the run log
when something fails. Secrets come from [Infisical](https://infisical.com) at run time.

- One static Go binary, no runtime to install
- Each target runs in Docker, on the host, or on a remote server
- One bucket per app, one folder per environment and kind
- Targets are independent: one failing never skips the others

```sh
curl -fsSL https://raw.githubusercontent.com/daksh-01/backstash/main/install.sh | sh

backstash validate myapp    # check config, secrets, tools and the bucket
backstash backup myapp      # back up everything configured for the app
```

## Documentation

| Page                                   | Read it for                                                   |
| -------------------------------------- | ------------------------------------------------------------- |
| [Usage](docs/usage.md)                 | Commands, flags and exit codes                                |
| [Configuration](docs/configuration.md) | Every config key, secret name, and the bucket layout          |
| [How it works](docs/how-it-works.md)   | Flow diagrams: backup, restore, retention, alerts             |
| [Operations](docs/operations.md)       | Install, schedule, restore safely, upgrade, troubleshoot      |
| [Alerts](docs/alerts.md)               | The Slack card, the failure email, and the Slack app manifest |
| [Development](docs/development.md)     | Tests, package layout, releasing                              |

## License

MIT, see [LICENSE](LICENSE).
