# CapyDB CLI

The CapyDB CLI is a Go binary for linking local projects to existing CapyDB projects, pulling connection strings into env files, and running repeatable project operations.

## Installation

Homebrew (macOS/Linux):

```bash
brew install capydatabase/tap/capydb
```

Curl installer (macOS/Linux; verifies the release checksum, installs to
`/usr/local/bin` or `$CAPYDB_INSTALL_DIR`):

```bash
curl -fsSL https://raw.githubusercontent.com/capydatabase/capydb-cli/main/scripts/install.sh | sh
```

With `go install`:

```bash
go install github.com/capydatabase/capydb-cli/cmd/capydb@latest
```

From source:

```bash
git clone https://github.com/capydatabase/capydb-cli
cd capydb-cli
make build
```

Release archives are built for macOS, Linux, and Windows. Tagged releases also publish checksums, SBOMs, a container image, and the Homebrew cask.

## Default flow

1. Sign up in CapyDB.
2. Create or join an organization in the dashboard.
3. Create a project in the dashboard.
4. Install the Go CLI binary.
5. Run `capydb login`.
6. Run `capydb link` inside your local app directory.

The CLI will:

- open a browser login flow,
- use the active dashboard organization,
- detect the local app profile,
- write the right env file,
- save a local `.capydb/project.json` link,
- keep secrets in the app env file instead of the local metadata file.

## Main commands

- `capydb login`
- `capydb logout`
- `capydb whoami`
- `capydb status [--remote] [--project <ref>]`
- `capydb doctor` (checks config, API reachability, auth, the local project link, and psql; non-zero exit when any check fails)
- `capydb config show` (resolved config; the API key is shown only as a `****abcd` fingerprint)
- `capydb version [--check]` (build info; `--check` compares against the latest GitHub release with a 5s timeout and degrades to a warning offline)
- `capydb orgs list` / `capydb orgs switch <org-id|slug>` (the CLI stores credentials per organization; switch the active one)
- `capydb projects list`
- `capydb regions list`
- `capydb link`
- `capydb unlink`
- `capydb env pull`
- `capydb preview list`
- `capydb preview create`
- `capydb preview reset`
- `capydb preview delete`
- `capydb preview extend <preview-id> --ttl-hours N`
- `capydb ephemeral create [--name N] [--no-env]` (a throwaway database with **no account and no login**: writes `DATABASE_URL` to the env file, or prints the connection strings with `--no-env`. Destroyed after 72 hours unless claimed. The claim token is kept in the git-ignored `.capydb/ephemeral.json`, mode 0600 - it cannot be recovered)
- `capydb ephemeral status [--claim-url]` (state and time left; `--claim-url` prints the browser claim link, which embeds the secret token)
- `capydb ephemeral claim` (keep it: logs in if needed, moves the database into your organization as a normal project, and links the directory. Data and connection strings do not change)
- `capydb ephemeral destroy` (end it early with its claim token and free its slot; the database is deleted within seconds)
- `capydb backups list`
- `capydb backups create`
- `capydb import` / `capydb import preflight --source-url <url>` (checks size, Postgres version, and extension compatibility before any destructive step)
- `capydb restore`
- `capydb jobs get`
- `capydb studio`
- `capydb connection-string [--pooled] [--preview <id>]` (prints only the URL - script-friendly)
- `capydb psql [--pooled] [--preview <id>] [-- <psql args>]` (opens psql against the project or a preview)
- `capydb sql "select ..." [--max-rows N] [--json]` (runs a query through the bounded SQL runner)
- `capydb metrics [--json]` (storage/connection usage, alerts, active and slow queries)
- `capydb extensions list|enable <name>|disable <name> [--project <ref>]` (Postgres extensions; enable/disable queue jobs and support `--wait`)
- `capydb alerts list [--project <ref>]` / `capydb alerts ack <alert-id> [--project <ref>]` (resource alerts)
- `capydb audit list [--project <ref>] [--limit N]` (project audit events)
- `capydb api-keys list|create|revoke` (`create` requires `--name` and `--scopes`; `--project` makes the key project-scoped; the plaintext key is shown exactly once)
- `capydb webhooks list|create|delete|rotate-secret|deliveries` (organization webhook endpoints; signing secrets are shown exactly once)
- `capydb kv status|create|credentials|rotate-token|flush|delete [--project <ref>]` (the project's K/V store - key-value and rate limiting. `create` and `rotate-token` are the only chance to capture the token - only its hash is stored - and accept `--write-env` to merge `CAPYKV_REST_URL`, `CAPYKV_REST_TOKEN` and `CAPYKV_REDIS_URL` (the RESP URL, which carries the token as its password) into the project's env file, in which case the token and the RESP URL are written there instead of printed; `credentials` cannot show the token, because only its hash is stored)
- `capydb create` (create a project and link the current directory; `--postgres-version 16|17|18`, `--environment production|non_production`)
- `capydb projects list|always-on|set-environment` (production projects stay awake by default; `always-on` and `set-environment` change the sleep policy)
- `capydb credentials rotate [--grace-hours N]` (new database password; with a grace period, a new username too, and the old one keeps working until the window ends)
- `capydb restore-points list|create|delete` (labelled points to restore to)
- `capydb export [list|download]` (a downloadable dump of the project database)
- `capydb logs [--follow]` (the project's database logs)
- `capydb advisor indexes|index-hygiene` (index suggestions, and unused or redundant indexes)
- `capydb upgrade preflight|minor` (Postgres version upgrades)
- `capydb schema dump|diff` and `capydb generate types|zod|drizzle` (schema inspection and typed code from the live schema)
- `capydb init drizzle` (scaffold a Drizzle setup for the linked project)
- `capydb migrate scan|deps|rls|verify-rls|squash|verify|codemod` (plan and check a move from another Postgres provider)
- `capydb integrations env` (print the env payload an integration would inject)
- `capydb completion bash|zsh|fish|powershell` (shell completions; release archives and the Homebrew cask ship pre-generated scripts)

Global flags on every command:

- `--output text|json` (`-o`): switch between human-readable text and machine-readable JSON. In JSON mode stdout carries only the JSON document, and lists always marshal as `[]`, never `null`.
- `--api-url`, `--api-key`, `--app-url`: override the saved configuration for one invocation.

Commands that queue async jobs (`create`, `preview create|delete|reset`, `backups create`, `import`, `restore`, `extensions enable|disable`, `kv create|flush|delete`, `jobs get`) accept `--wait` plus `--wait-timeout` (default 30m). Dump uploads are bounded by a 30m deadline; `CAPYDB_HTTP_TIMEOUT` (a Go duration such as `45s` or `2m`) overrides both the API and upload timeouts.

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | success |
| 1 | generic error |
| 2 | usage or validation error |
| 3 | authentication or authorization error (HTTP 401/403, missing credentials) |
| 4 | resource not found (HTTP 404/410) |
| 5 | conflict or failed precondition (HTTP 409/412/428) |
| 6 | timeout (HTTP 408/504, `--wait-timeout` expiry) |

Compatibility aliases:

- `capydb auth login`
- `capydb auth logout`
- `capydb auth whoami`
- `capydb connect` -> `capydb link`

## Linking a local project

```bash
cd /path/to/project
capydb login
capydb link --project my-project
```

`capydb link` accepts:

- project id
- project slug
- project name

If you omit `--project`, the CLI will use the linked project in `.capydb/project.json` when available, or prompt you to choose a project when multiple matches exist in an interactive terminal.

## Local files

Saved globally:

- CLI auth goes in the user config directory (`capydb/config.json`), keyed per organization with an `active_org` pointer. Older single-credential config files are migrated to the per-organization shape automatically on first load. Use `capydb orgs list` and `capydb orgs switch` to manage entries, and `capydb config show` to inspect the resolved values.

Saved locally:

- `.capydb/project.json` stores the linked project id, chosen env file, detected profile, and other non-secret metadata.
- The CLI adds both `.capydb/` and the credential-bearing env file (e.g. `.env.local`) to `.gitignore` so the connection URL with the database password is never committed.

Written into the app:

- `DATABASE_URL`
- `DATABASE_DIRECT_URL`
- `DATABASE_POOL_URL`
- framework-specific aliases such as `DIRECT_URL` for Prisma

## Detected profiles

The CLI currently detects common app/framework and DB-layer combinations, including:

- Next.js
- SvelteKit
- Remix
- Django
- Rails
- Go services
- generic Node apps
- generic Python apps
- Prisma
- Drizzle
- `pg`
- `pgx`
- SQLAlchemy
- Active Record

For monorepos, the CLI can detect nested app directories and prompt for the app to link when there is more than one candidate.

## Notes

- `capydb create` still exists for power users, but it is no longer the default onboarding path.
- `capydb studio` opens the dashboard page for the linked project. If your CLI API URL points directly at the backend instead of the frontend proxy, set `CAPYDB_APP_URL` or pass `--app-url`.

## Development

```bash
make build
./capydb --version
```

Useful targets:

- `make fmt`
- `make lint`
- `make test`
- `make check`
- `make build-all`
- `make release-check`
- `make release-snapshot`

Release automation lives in [`.github/workflows/ci.yml`](.github/workflows/ci.yml) and [`.github/workflows/release.yml`](.github/workflows/release.yml). Pushing a tag that matches `v*.*.*` runs the release workflow.
