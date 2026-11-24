# Usage

```sh
backstash --config-dir ~/.config/backstash validate myapp   # checks everything, writes nothing
backstash backup myapp                                     # all configured targets
backstash backup myapp --only postgres --keep
backstash list myapp
backstash restore myapp postgres                           # pick from a list, type the DB name
backstash restore myapp postgres myapp-20261122-020000.dump --yes
backstash prune myapp
```

| Command | What it does |
|---|---|
| `backup <app> [--env prod] [--only a,b] [--keep]` | Dump, upload, prune each target. Exit 1 if any failed. |
| `restore <app> <postgres\|redis\|files> [NAME] [--yes]` | Download and restore. Redis prints manual steps instead of overwriting. |
| `list <app>` | Stored backups per target, newest first. |
| `prune <app>` | Apply retention without taking a backup. |
| `validate [app]` | Config, secrets, installed tools, running containers, bucket access. |
| `version` | Print the version. |

`--env` defaults to `prod`. `--config-dir` defaults to `$BACKSTASH_CONFIG_DIR`, else `~/.config/backstash`.
Flags can go before or after the positional arguments. Exit codes: `0` success, `1` a target or
setup step failed, `2` bad arguments.

See [Operations](operations.md) for restoring safely and [Configuration](configuration.md) for the files
the commands read.
