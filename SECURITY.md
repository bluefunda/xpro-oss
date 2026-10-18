# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest  | Yes       |
| Older   | No        |

We support only the latest released version. Please upgrade before reporting.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

To report a vulnerability, please open a
[private security advisory](../../security/advisories/new) on GitHub.
Include:

- A description of the vulnerability and its potential impact.
- Steps to reproduce the issue.
- Any proof-of-concept code, `config.yaml` (with credentials redacted), or
  sample input that triggers it.

We will respond within **72 hours** and aim to ship a patch within **14
days** for confirmed vulnerabilities.

## Scope

In scope:

- Credential handling in `config` (env expansion, BTP binding resolution).
- SQL statement construction in `db` (the INSERT column/table list is
  config-driven — injection via a malicious `config.yaml` value, or via
  parsed XML content reaching a query, is in scope).
- SSH/SFTP authentication and host-key handling in `source`.
- XPath/XML parsing in `parser` (malformed or adversarial input files).

Out of scope:

- Issues in upstream dependencies (report to the upstream project;
  `govulncheck` runs on every PR to catch known CVEs).
- GitHub Actions runner security (GitHub's responsibility).

## Security Considerations for Users

- **Credentials**: `config.yaml` accepts `${VAR}` references — never commit
  real database or SFTP passwords/keys directly into the file.
- **`sslmode`**: the PostgreSQL target defaults to `sslmode: require`. Only
  relax this for local development.
- **SSH host keys**: the direct (non-BTP) SFTP path currently trusts the
  remote host key on first connect (via btp-go's `sshclient`, which uses
  `ssh.InsecureIgnoreHostKey` by default). Tighten this in regulated
  environments — see `source.SFTP` and `sshclient.Config.HostKeyCallback`.
- **`workers`/`batchSize`**: set conservatively against untrusted or
  unbounded source directories to avoid resource exhaustion.
