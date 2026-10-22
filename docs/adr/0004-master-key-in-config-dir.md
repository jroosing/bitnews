# The master key lives in the config directory by default

The master key encrypts the secrets bitnews must send in plaintext later: Usenet Server passwords and archive passwords. It is generated on first start and stored as `bitnews.key` (mode `0600`) next to `bitnews.db`, so a default install has nothing to set up. `BITNEWS_KEY_FILE` still places it elsewhere, for example in a Docker secret or a systemd credential.

The original design forced the key outside the config directory, so a backup of the config directory never held both the database and its key. That cost a separate secret to create, mount and back up for every install, and the owner of the deployment judged it too much setup for a homelab daemon.

## Considered Options

- **No encryption.** Simplest, but then even a database copy attached to a support request exposes the provider logins. Rejected.
- **Derive the key from the admin password.** The daemon has to read Server passwords at startup with nobody logged in, so this cannot work unattended.
- **Key outside the config directory (the previous design).** Strongest against a leaked database copy, but a manual secret per install. Kept as an option.

## Consequences

- A copy of `bitnews.db` alone does not expose the stored passwords, but anyone who can read the whole config directory can. Backups of the config directory contain both.
- Losing `bitnews.key` loses only the stored Server and archive passwords, which are re-entered; the Servers are listed as needing a password.
- The check that refused a key file inside the config directory is gone.
