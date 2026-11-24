# How it works

Four flows: a backup, a restore, retention, and alerting.

## Backup

```mermaid
sequenceDiagram
    participant C as cron
    participant T as backstash
    participant I as Infisical
    participant X as Postgres / Redis / files
    participant S as bucket
    participant N as Slack and email

    C->>T: backstash backup myapp
    T->>I: read /backstash/shared and the app folder
    loop each configured target, independently
        T->>X: pg_dump / redis-cli --rdb / tar
        X-->>T: staged file (0600, in a 0700 folder)
        T->>S: upload <app>/<env>/<kind>/<name>
        T->>T: remove the local copy (unless --keep)
        T->>S: delete backups past the retention window
    end
    T->>N: one Slack card; on any failure, an email with run.log
```

| Step that fails | What happens |
|---|---|
| The dump | The partial or empty file is deleted. That target is failed; the others still run. |
| Upload | The local file is kept and its path is in the error. Pruning is skipped for that target. |
| Pruning | The backup is already stored, so the target still succeeds. A warning is shown on the card. |
| Reading secrets | Nothing runs. The command exits 1 and the error is in the cron log (see Alerts). |

A panic inside a target is caught and reported as that target's failure, so the alerts still fire.

## Restore

```mermaid
flowchart TD
    start["backstash restore app kind"] --> named{"Backup name given?"}
    named -- no --> list["List stored backups, newest first"]
    list --> pick{"Valid number?"}
    pick -- "blank or invalid" --> silent["Print only, no alert"]
    pick -- yes --> confirm
    named -- yes --> confirm{"Typed the target name? (or --yes)"}
    confirm -- no --> silent
    confirm -- yes --> download["Download to the staging folder"]
    download --> kind{"Kind"}
    kind -- postgres --> pg["pg_restore, ONE transaction, 30 s lock timeout"]
    kind -- files --> tar["tar -x over the folder"]
    kind -- redis --> steps["Keep the file, print stop / copy / start steps"]
    pg & tar --> ok{"Succeeded?"}
    ok -- yes --> done["Delete the download, Slack card"]
    ok -- no --> kept["Download kept for a retry, red card and email"]
```

A failed Postgres restore leaves the database exactly as it was. Redis is never overwritten
automatically: a running Redis cannot load an RDB in place.

## Retention

Runs for each target after its upload, inside that kind's folder only. Default 30 days.

```mermaid
flowchart TD
    list["List objects in <app>/<env>/<kind>/"] --> mine{"Name looks like label-YYYYmmdd-HHMMSS.dump, .rdb or .tar.gz?"}
    mine -- no --> leave["Leave it alone"]
    mine -- yes --> old{"Older than the window?"}
    old -- no --> keep["Keep"]
    old -- yes --> newest{"Is it the newest backup in this folder?"}
    newest -- yes --> keep
    newest -- no --> delete["Delete (batches of 1000)"]
```

`retention_days: 0` turns pruning off. The newest backup is never deleted, so a wrong clock or a
long outage cannot leave you with nothing. Kept or failed-upload files in the staging folder age out
with the same window.

## Alerts

```mermaid
flowchart TD
    finish["Run finished"] --> mistake{"Cancelled, invalid choice, or no terminal?"}
    mistake -- yes --> silent["Print only, no alerts"]
    mistake -- no --> slack{"SLACK_BOT_TOKEN and a channel set?"}
    slack -- yes --> card["Post one card: green or red, one line per target"]
    slack -- no --> skip["Skip Slack"]
    card --> failed
    skip --> failed{"Did anything fail?"}
    failed -- no --> done["Exit 0"]
    failed -- yes --> mail{"SMTP host and alert_email_to set?"}
    mail -- yes --> email["Email the alert address with run.log attached"]
    mail -- no --> end2["Exit 1, the missing setup is logged"]
```

Alerts are best effort: a broken Slack or mail setup never turns a good backup into a failure. The
exit code reflects the backup itself (`0` all targets succeeded, `1` otherwise), which is what lets you gate a
deploy or CI step on a successful backup.

**Startup failure alerting:** if Infisical is unreachable during startup, `backstash` automatically retries with
exponential backoff. If Infisical remains unreachable, it checks for fallback notification credentials in
`identity.env` or process environment variables (`SLACK_BOT_TOKEN`, `SMTP_USER`, `SMTP_PASS`) to dispatch a critical
startup failure alert before exiting with code 1.
