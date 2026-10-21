# Alerts

Every run (except a cancelled or mistyped restore) posts **one** Slack card. A run where anything
failed also sends an email with the run log attached. Both are best effort: they can never make a
good backup fail.

## Slack card

```
| Backup Successful
|
| Application   Environment   Host
| myapp       prod          server-1
|
| Size          Duration      Old backups removed   Completed
| 85.2 MB       1m 15s        2                     November 23, 2026, 07:01:20 UTC
|
| ✅ postgres: myapp-20261123-020005.dump (84.2 MB)
| ✅ redis: redis-20261123-020030.rdb (1.0 MB)
```

A failed run says `Backup Failed`, drops Size, and shows `❌ <kind>: <error>` for each failed target
while still listing the targets that worked. Error text is escaped, so a message containing
`<!channel>` cannot ping anyone.

## Failure email

To `alert_email_to`, plain text, with `run.log` attached (everything logged during that run).

```
Subject: [backstash] backup FAILED: myapp/prod (redis)

Backup failed for myapp (prod) on server-1 after 1m 15s.

[postgres] ok: myapp-20261123-020005.dump (84.2 MB)
[redis] FAILED: redis-cli failed (exit 1): NOAUTH Authentication required.

The full run log is attached as run.log.
```

## Create the Slack app

You need a bot with the `chat:write` scope. Create the app from this manifest:

```yaml
display_information:
  name: Backup Alerts
  description: Posts database backup and restore results.
features:
  bot_user:
    display_name: Backup Alerts
    always_online: false
oauth_config:
  scopes:
    bot:
      - chat:write
settings:
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
```

1. Go to <https://api.slack.com/apps>, choose **Create New App**, then **From a manifest**, pick the
   workspace and paste the YAML.
2. Open **Install App**, install it, and put the **Bot User OAuth Token** (`xoxb-...`) in Infisical as
   `SLACK_BOT_TOKEN` (in `/backstash/shared`).
3. In the alerts channel run `/invite @Backup Alerts`. Without this, posting fails with `not_in_channel`.
4. Put the channel id (or name) in `config.yml` under `slack.channel`.

Treat the token as a secret. If it leaks, reinstall the app and update the Infisical secret.
