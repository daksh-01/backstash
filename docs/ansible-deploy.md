# Deploying backstash with Ansible

Backstash only reads secrets itself, from Infisical (or AWS Secrets Manager with `secrets_provider: aws`).
It cannot take them from a file. So Ansible does not hand over the secrets, only the **identity** that lets
backstash fetch them.

## Flow

1. Ansible reads the backstash machine identity from Infisical on the control node (for example with the `infisical.vault` collection, using your deploy identity).
2. Ansible writes it to `~/.config/backstash/identity.env` on the host, mode 0600.
3. On each scheduled run, backstash uses that identity to fetch `/backstash/shared` and
   `/backstash/<app>-prod` itself.

## One-time setup in Infisical

- Create a **second machine identity** just for backstash, read-only on `/backstash/**`. Do not reuse the
  deploy identity, which can read everything.
- Store its credentials where the deploy identity can read them, e.g. `/backstash-identity`, with keys
  `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID`, `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET`, `INFISICAL_PROJECT_ID`.
- Add the backup secrets:
  - `/backstash/shared`: `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY`, `SLACK_BOT_TOKEN`, `SMTP_USER`,
    `SMTP_PASSWORD`
  - `/backstash/<app>-prod`: `POSTGRES_PASSWORD`, `REDIS_PASSWORD`

## First-time install on a server

Do this once per host, before the first scheduled run. These steps go in
a playbook of your own (call it `backstash.yml`), ahead of the config and identity steps.

### Prerequisites (assert in the playbook)

- A published release tag exists (e.g. `v0.1.0`). Pin it in `backstash_version`; never use "latest" on servers.
- `ansible_user` can run `docker ps` (in the docker group). Fail early if not.
- Host tools match your target modes:
  - Docker mode: nothing extra.
  - Postgres `tcp` mode: `pg_dump` and `pg_restore` of the same major version as the server.
  - Redis `tcp` mode: `redis-cli`.
  - Files `path` mode: `tar`.
- Installing packages needs root, which the playbooks don't use. Assert the tools exist and print the
  `sudo apt install ...` hint instead.

### Install steps

1. **Pick the arch.** Map `ansible_architecture` to `amd64` or `arm64` (needs `gather_facts: true`, or run
   `uname -m`). Fail on anything else.
2. **Create dirs.** `<install dir>/bin` (0755), `~/.config/backstash/apps.d` (0700),
   `~/.local/state/backstash/staging` (0700).
3. **Download** `backstash_linux_<arch>` and `checksums.txt` from release `v{{ backstash_version }}` of
   `daksh-01/backstash` into a temp dir. If the repo is private, use the GitHub App token (a GitHub App or fine-grained token), passed on stdin, not argv. Keep the asset names exactly as published (stable contract).
4. **Verify** with `sha256sum -c --ignore-missing checksums.txt`; fail the play on mismatch. Optionally run
   `gh attestation verify` if `gh` is on the host.
5. **Install** with `install -m 0755 backstash_linux_<arch> <install dir>/bin/backstash`. Make it
   idempotent: run `backstash version` first and skip the download if it already prints
   `backstash v<wanted>`. Upgrades are then a version bump and a re-run.
6. **Confirm** with `backstash version` (`changed_when: false`), assert it matches, clean up the temp dir.

Alternative: call the repo's `install.sh` with `BACKSTASH_VERSION` and `BACKSTASH_INSTALL_DIR`. Same checks in
one task, but it pipes a script from `main`. Doing the steps yourself is about five tasks and stays pinned.

## Playbook order (first run)

1. Prerequisite checks.
2. Binary install (above).
3. Read the identity: a task that reads `/backstash-identity` into a variable such as `backstash_identity`.
4. Write `identity.env` (0600, `no_log: true`, `diff: false`,, from a template), plus `config.yml` and
   `apps.d/<app>-prod.yml` from playbook vars. These files hold no secrets. Keep per-app values in the app's own
   playbook `vars`.
5. `backstash validate <app>`. Read-only; fails the deploy early if a secret, tool, container or the bucket
   is wrong.
6. Optional first manual `backstash backup <app>`, gated behind `-e first_run=true`.
7. Cron entry, using the existing remove-then-add pattern and timezone conversion:
   `backstash backup <app> >> .../backup.log 2>&1`. Set `BACKSTASH_CONFIG_DIR` if not using the default
   location.

Each app playbook can then import it, passing the app name, backup time and timezone.

Later runs: steps 2 to 7 are safe to repeat. The version check skips the download, files are rewritten from
templates, and the cron entry is re-added.

## Gotchas

- The host must reach `app.infisical.com` (or `INFISICAL_URL`) at backup time, not just at deploy time.
- Rotating the identity means re-running the playbook to update `identity.env`.
- If Infisical is down at backup time, backstash falls back to `SLACK_BOT_TOKEN` and `SMTP_*` found in
  `identity.env` to send the alert. Add those keys to `/backstash-identity` and the template to keep that
  working.
- AWS option: set `secrets_provider: aws` in `config.yml` and write `AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` into `identity.env` instead. The rest of the flow is the same.

## Verifying (do not deploy to test an edit)

```sh
ansible-playbook backstash.yml -e target=<group> --syntax-check
ansible <group> -m ping
ansible-playbook backstash.yml -e target=<group> --step   # real dry run; --check skips command tasks
```
