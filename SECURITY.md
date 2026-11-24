# Security policy

## Reporting a vulnerability

Please **do not open a public issue**. Report it privately through GitHub's
["Report a vulnerability"](https://github.com/daksh-01/backstash/security/advisories/new) form on this repository.
Include the version (`backstash version`), what you found, and steps to reproduce.

You can expect an acknowledgement within a few days. Fixes ship in a new release, and the advisory credits
you unless you prefer otherwise.

## Supported versions

Only the latest release gets fixes.

## Design notes

- Secrets are read at run time and passed to child processes by environment variable, never on the command line.
- Staged backups are written to a `0700` directory as `0600` files.
- Release binaries come with `checksums.txt` and a build-provenance attestation.
- Use a read-only, least-privilege identity for secrets; see [docs/secrets.md](docs/secrets.md).
