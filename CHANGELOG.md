# Changelog

All notable changes to the `capydb` CLI are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[SemVer](https://semver.org/).

Releases are cut with GoReleaser from a git tag; entries under **Unreleased** ship with the next tag.

## [Unreleased]

### Added

- **`capydb postgres-versions`** lists the Postgres majors new databases can be created on, with
  each one's release channel (`previous`, `stable` - the default - `current`, or `beta`), whether
  it is the default, and whether it is production ready (`-o json` for the raw list).
- `--postgres-version` on `create`, `ephemeral create` and `cloudflare create-database` documents
  `19`, accepted while CapyDB offers it as a beta. `create`, `ephemeral create`/`status` and
  `status --remote` show the database's major with its channel (`Postgres 18 (current)`) and print
  the beta warning when the control plane sends one; `create -o json` adds `postgres_version`,
  `postgres_channel` and `postgres_warning`.

- **`capydb upgrade major|confirm|rollback|status`: self-serve Postgres major upgrades.**
  `upgrade major --target-major N` runs the preflight first and starts the upgrade only when it
  passes (the control plane requires a passing preflight from the last hour); the previous
  database is kept for 72 hours, until `upgrade confirm` deletes it or `upgrade rollback` returns
  to it (discarding writes since the cutover). Each step needs its own single-use approval from an
  organization admin, passed with `--approval-token` or `CAPYDB_APPROVAL_TOKEN` and checked before
  anything runs, plus the usual typed-name confirmation or `--confirm`. `upgrade status` shows the
  upgrade in flight and `rollback_available_until`. A 403 says that self-serve upgrades must be
  enabled for the organization; they are off until CapyDB turns them on.

- **`capydb restore --restore-time` says when the time was clamped.** A point-in-time target later
  than the latest restorable point is now moved back to that point instead of refused; the command
  prints the requested and effective times (on stderr with `-o json`, where stdout stays the job),
  and the `--wait` summary names the effective time.

- **`capydb projects retry <project>`** re-runs a failed provisioning: the same database with the
  same credentials, so env files keep working. Repeating it returns the job already in flight; a
  project that failed after its database was provisioned is refused with an explanation. Supports
  `--wait` and `-o json`.

- **`capydb roles app show|enable|rotate`: the split role model's runtime login.** `enable`
  creates `app_user` on the project's database (it checks first whether the project already has it
  and whether the platform offers it), `rotate` replaces its password, and `show` reports whether
  it exists and when it was created or rotated. All take `--project`, `-o json`, and `--wait` for
  the job.
- **`capydb env pull` (and `link`/`create`) write `DATABASE_APP_URL` and `DATABASE_APP_POOL_URL`**
  when the project has its runtime login. `DATABASE_APP_URL` takes the same pooled-or-direct choice
  `DATABASE_URL` makes for the detected stack; `DATABASE_APP_POOL_URL` is always pooled. A project
  without the login gets no new lines.
- `capydb migrate rls --role-model split` (default `--target capydb`) now ends with a note that the
  bundle needs the project's runtime login and names `capydb roles app show|enable`.

- **`capydb sql --preview <id>`** runs the statement against a preview database, with the same
  guards (`--allow-unqualified-writes`, `--read-only`, `--max-rows`) - the way to rehearse a
  destructive statement before running it on the project.

### Changed

- capydbclient v1.13.0 -> v1.14.0.
- Text output labels every job type the control plane reports (major-upgrade steps, app-role
  enable and rotate, K/V stop and start, export, extension update, credential expiry, and the
  storage-tuning jobs) instead of falling back to a reworded internal name.
- **`capydb regions` shows each region's display name and location.** Region ids are now neutral
  (`eu-north-1`); the table lists `REGION  NAME  LOCATION`, the bare `capydb regions` lists like
  `capydb regions list`, and `-o json` returns `{"regions": [{"id", "display_name", "location"}]}`
  (it was a list of ids). The `create` region prompt shows the same labels, and a `--region` the
  list does not contain (such as the deprecated `hel1`) is passed to the control plane, which
  resolves deprecated names and rejects unknown ones.
- capyrls v1.15.0 -> v1.16.0.
- **`capydb migrate rls --role-model split` works on CapyDB.** It was refused for the default
  `--target capydb`. It now builds a bundle for the project's runtime role `app_user`, which the
  platform creates once the project enables it (`POST /v1/projects/{id}/roles/app`): the bundle
  creates no roles, checks that `app_user` exists (and says how to enable it if not), grants to it,
  and turns `anon`/`authenticated` into predicates on it. The owner is the service path - it
  bypasses row security on tables that are not FORCEd - so there is no `BYPASSRLS` role.
  `--role-model single` stays the default.

## [2.0.0] - 2026-09-29

### Changed

- capyrls v1.14.0 -> v1.15.0.
- **`capydb init drizzle` no longer filters `pg_stat_statements_info`.** Every cell now keeps
  that view out of tenant reach, so drizzle-kit never sees it; the generated `drizzle.config.ts`
  excludes only `pg_stat_statements`. Existing configs that still list both keep working.
- **Breaking: `capydb connect` now opens psql; it no longer aliases `capydb link`.** `connect` is
  the spelling other Postgres platforms use for "open a shell on my database", and the spec's
  minimum CLI surface lists it with that meaning; as an alias of `link` it only wrote env vars.
  It is now an alias of `capydb psql` (direct connection, `--pooled` for the pooler, `--project`,
  `--preview`, arguments after `--` passed to psql). Scripts that ran `capydb connect` to link a
  directory must switch to `capydb link`. `--env-file` and `--overwrite-env` now fail with a usage
  error, but `capydb connect --project <p>` is valid for both commands and now opens psql instead of
  writing env vars - search scripts for `capydb connect`.
- **`capydb env pull` no longer replaces another provider's value without a word.** It used to
  overwrite every key it writes silently. It still refreshes values that point at the same CapyDB
  host (a credential rotation) silently; a value pointing anywhere else - typically a `DIRECT_URL`
  left over from a previous provider - is prompted for on a terminal and announced with a warning
  in CI before it is replaced, the same as `link` and `create`.
- **`capydb cloudflare create-database` (hidden; the Cloudflare partner flow is not enabled yet)
  follows the CLI's conventions.** It prints a text summary by default and one JSON document with
  `-o json` (it always printed JSON), reports missing flags as usage errors (exit 2), takes no
  positional arguments, and uses the global `--api-url` instead of redeclaring it. Its help now says
  what it does today: the control plane rejects every request until Cloudflare onboarding completes,
  and the flag names may still change then.
- **The Docker image is published as `ghcr.io/capydatabase/capydb-cli:<version>` and `:latest`
  only.** The `<version>-amd64` / `<version>-arm64` tags pointed at the same multi-platform image
  as `<version>` (both architectures), so they named something they were not; Docker picks the
  platform on pull. Images for v1.4.0-v1.8.0 remain as published (v1.3.0 has none: its release run
  failed before the build).

### Added

- `LICENSE` with the MIT license text.
- **`capydb migrate rls --uid-type text` for identity providers whose user ids are not uuids.**
  The converted user-id accessor (`app.user_id()`, or `auth.uid()` with `--mode supabase-compat`)
  cast the subject to `uuid`, so a Clerk-style `user_2abc...` id made every policy calling it fail
  with `invalid input syntax for type uuid`. With `--uid-type text` it returns the id as `text`,
  and the report names each uuid column the user id is compared to or defaulted into - change those
  to `text` before applying, or the apply stops with `operator does not exist: text = uuid`. The
  default stays `uuid`. Built on capyrls v1.14.0.
- **`capydb projects delete <project>` deletes a project** - its database, preview databases, and
  backups. The project must be named (id, slug, or name); the linked project is never used
  implicitly. Confirm by retyping the project name or pass `--confirm` (`--yes` is accepted too). A
  production project also needs a delete approval an organization admin creates on the project's
  settings page in the dashboard, passed with `--approval-token` or `CAPYDB_APPROVAL_TOKEN`; without
  one the command stops before any destructive call and prints the settings page URL.
  Non-production projects need only the confirmation. `--wait` follows the deletion job.
- **`capydb import` and `capydb import preflight` stop a source CapyDB cannot reach, before any API
  call, and say what works instead.** A source on `localhost`, a unix socket, a private network
  (RFC 1918 / IPv6 ULA), CGNAT/Tailscale (`100.64.0.0/10`), link-local, `*.internal`, or `*.local` -
  directly or through what the name resolves to on this machine - used to fail with the control
  plane's generic "host is not allowed". The CLI now explains that imports run from CapyDB's network
  and prints the two commands that work: `pg_dump -Fc ... -f source.dump` (password masked) and
  `capydb import --file source.dump`. `--follow` explains that streaming needs a reachable source;
  the preflight points at `capydb migrate scan --source-url` for a local check. Because the source
  is reachable from here, it also compares the local `pg_dump` major with the source's.
- **Import preflight warns when the local `pg_dump` major differs from the source's.** An older
  `pg_dump` refuses to dump a newer server; a newer one dumps fine but can write settings an older
  restore target rejects. The note names the direction and the pinned client to use
  (`docker run --rm postgres:<major> pg_dump`). It is advisory - the server-side import uses its
  own client - and appears only when `pg_dump` is on PATH.
- **Supabase sources get the project's real pooler host.** With `SUPABASE_ACCESS_TOKEN` set, `import`
  and `import preflight` read the project's pooler from the Supabase Management API and warn when
  the connection string uses the wrong `aws-0` / `aws-1` prefix, the IPv6-only direct host, or the
  transaction pooler port 6543, naming the session pooler (`<host>:5432`) to use instead. Without a
  token the preflight explains where to copy the Session pooler string and prints the `curl` that
  reads it.
- **`capydb create --source-url <url>` creates the project on the source database's Postgres
  major.** A dump restores into the same or a newer major, never an older one, so without
  `--postgres-version` the project gets the source's major (read with one read-only query), raised to
  16 for older sources; a source newer than every offered major (18) stops the create with an
  explanation. An explicit `--postgres-version` still wins, with a warning when it is older than the
  source. An unreachable `--source-url` fails before anything is created. Without the flag, when the
  env file `create` is about to write already points `DATABASE_URL` / `DIRECT_URL` at a database
  outside CapyDB, `create` says so and names the flag; it does not connect to that database.
- **`link`, `create`, `env pull`, and `ephemeral create` write `DIRECT_URL` for every stack, and
  `integrations env` (dotenv, json, vercel, netlify) includes it.** It is the name Prisma's
  `directUrl` and most ORM and provider guides use for the direct connection; it was written only
  for Prisma projects. It carries the same value as `DATABASE_DIRECT_URL`, which stays.
  `DATABASE_URL` keeps its per-stack default (pooled for JS/TS, direct for Go, Python, and Ruby).
  `create -o json` lists it in `env_vars`.
- **`capydb migrate rls --mode supabase-compat` now has a service path.** Under the default single
  role model the bundle emits a service escape keyed on the claims: a transaction whose verified
  `request.jwt.claims` carry `"role": "service_role"` skips the row filters, where before nothing
  bypassed the FORCEd policies. In both modes the escape now also passes restrictive policies,
  as `service_role`'s `BYPASSRLS` did. capyrls v1.15.0.
- **The `migrate rls` report lists `SECURITY DEFINER` functions that write to FORCEd tables**, with
  the fix: under FORCE they are filtered by the caller's policies and cross-user writes fail with
  `42501`. The CLI summary prints the count under Warnings.
- **`capydb migrate rls --target capydb|postgres`** (default `capydb`). `--role-model split` is
  refused up front on CapyDB, before any source is read, naming why: it creates a non-owning
  runtime role and a `BYPASSRLS` role, which a CapyDB database role cannot create. Pass
  `--target postgres` to build a split bundle for another Postgres - previously the CLI wrote a
  split bundle that stopped at its first `CREATE ROLE` on CapyDB.
- **`capydb generate go` and `capydb generate python`.** Go structs (with `db`/`json` tags, a string
  type plus constants per enum, and schema/table/column name constants) and Python models (frozen
  dataclasses by default, pydantic with `--style pydantic`) rendered from the same schema document
  as the TypeScript generators. Nullable columns are pointers in Go and `T | None` in Python;
  `numeric` stays a string in Go so no precision is lost; types the generator does not know are
  `any`/`Any`. `--package` sets the Go package (default `db`).
- **`capydb generate <language> --watch`** keeps running and rewrites the file whenever the
  schema changes. It checks every `--watch-interval` (default 5s), backs off to once a minute
  while nothing changes, and never reads a paused database - reading the schema would wake it.
  Note that while the database is awake, each check counts as activity. With `--output json` each
  regeneration prints one JSON line.
- **`capydb status --usage`**: what the project uses against its plan in plain language - plan and
  billing status, storage and connections against their limits, active previews, retained backups
  and their size, plus a note when storage or connections pass 80%. A paused database is not
  read (reading storage would wake it); the storage limit is still shown.
- **`capydb db lint`**: read-only schema and index checks - tables without a primary key (pointing
  at a NOT NULL unique constraint that could be promoted), foreign keys with no index leading with
  their columns, byte-for-byte duplicate indexes, unused and redundant indexes (from
  `advisor index-hygiene`), and tables whose dead rows outgrow autovacuum. Every finding carries
  the statement that fixes it. `--exit-code` fails on warnings for CI; `--preview` runs the
  schema checks against a preview. The catalog checks go through the SQL endpoint, so the key
  needs `projects:write` (the statement itself runs in a read-only transaction).
- **`capydb seed`** loads seed data into the linked project, `--project`, or a `--preview`. A
  `.sql` file runs through psql in one transaction (COPY blocks and meta-commands work; a failing
  file leaves nothing behind). `--run "<command>"` runs any seeder (drizzle-seed, Prisma, a script)
  with every database variable name - `DATABASE_URL`, `DATABASE_DIRECT_URL`, `DIRECT_URL`,
  `CAPYDB_DATABASE_URL`, the linked names - set to the target's direct URL. With neither, it
  runs the package.json `db:seed`/`seed` script, Prisma's `prisma.seed`, or a conventional
  `seed.sql`. A production project needs `--confirm-production` (or the typed project name) and
  gets a restore point first, so the seed can be undone; `--dry-run` shows what would run.
- **`capydb create --template <name>`** applies a built-in starter after the project is created
  and linked: `drizzle-starter` (users and posts, with sample rows) or `auth-starter` (users,
  OAuth accounts, sessions and verification tokens, one demo user); `empty` is the default.
  Schema and seed run in one transaction; if that fails the project still exists, nothing was
  applied, and the SQL is saved next to you for `capydb seed`.
- **`capydb migrate codemod neon` rewrites `db.batch([...])`** instead of only reporting it. In a
  repo that uses drizzle's neon-http driver, a batch whose statements are written inline on the
  client becomes `db.transaction(async (tx) => { ... })` that runs the same statements on `tx`, in
  order, and returns the same tuple (typed with `as [typeof r0, ...]` in TypeScript) - neon-http
  batches are one transaction, so the semantics match. Batches built from variables, spreads,
  statements containing `await`, a client reached through a property (`this.db`), or files with
  syntax the codemod cannot read with certainty (regex literals) are reported with the reason.
- **`capydb init prisma` and `capydb init kysely`** next to `init drizzle`, and `capydb init --orm
  <drizzle|prisma|kysely>` as the same thing. Prisma targets ORM 7: `prisma.config.ts` points
  migrations and `db pull` at `DATABASE_DIRECT_URL` (falling back to the integrations'
  `DATABASE_URL_UNPOOLED`), `prisma/schema.prisma` uses the `prisma-client`
  generator, and `src/db.ts` builds the client on `@prisma/adapter-pg` over the pooled
  `DATABASE_URL`; the install hint pins `@7` because the `prisma` CLI's latest tag is an 8.x release
  candidate without `db pull`. Kysely gets `src/db/database.types.ts` from `capydb generate types`
  and a client that maps the generated Row/Insert/Update types onto Kysely's column types
  (defaults optional on insert, generated columns not writable), tables outside `public` as
  `"schema.table"`. `init --output json` now includes `orm`.
- **`capydb import --from <provider>`** (`supabase`, `neon`, `planetscale`, `railway`, `render`,
  `rds`; `--from-neon` is shorthand) fixes up `--source-url` before the import and preflight see
  it, printing every change: Neon's `-pooler` endpoint becomes the direct one, Supabase's
  transaction pooler port 6543 becomes the session pooler 5432, PlanetScale's PgBouncer port 6432
  becomes 5432, and `sslmode=require` is added when the URL leaves TLS to libpq's `prefer`.
  Railway's `*.railway.internal` and Render's internal hostnames stop with where to find the
  public URL. Each preset lists its provider's traps (logical replication for `--follow`, IPv6-only
  Supabase direct host, RDS security groups).
- **`capydb import --data-only --source-url <url>`** copies rows into the tables your migrations
  already created, parents before children by the project's foreign keys - `pg_dump`'s
  alphabetical order fails there, and `pg_restore --disable-triggers` needs a superuser. It runs
  from this machine (so `localhost` and private-network sources work), reads the source in one
  snapshot, writes the project in one transaction, and loads only into empty tables. A foreign-key
  cycle is loaded with its deferrable constraints deferred; a cycle without one stops before
  writing. User triggers are off and FORCE ROW LEVEL SECURITY is lifted per table during the load
  (restored in the same transaction), generated columns are left to the project, serial and
  identity sequences continue from the source's position, materialized views are refreshed and
  tables analyzed afterwards. Column differences are reported. `--dry-run` prints the load order.
- **`capydb import --from-supabase-dump <file>`** restores a Supabase `pg_dump -Fc` file in the
  order a project role can apply it, in one transaction: the capyrls auth shim (`auth.uid()` and
  friends), the app schemas' tables and functions, the data, indexes and constraints, then FORCE
  ROW LEVEL SECURITY and the converted policies. Supabase-managed schemas, the dump's own policies,
  grants to the PostgREST roles, publications and event triggers stay behind, as do foreign keys
  into `auth.users` - each is listed. `extensions.` qualifications are rewritten to `public.` when
  the project keeps its extensions there. It stops before writing when the dump needs an
  extension the project lacks or the project already has tables. The policy bundle and report go
  to `--rls-out` (default `capyrls/`); `--uid-type text` for non-uuid user ids; `--dry-run` shows
  the plan. Needs `pg_restore` (at least the dump's major) and `psql`.
- **`capydb env sync vercel|netlify`** pushes the project's connection env vars to a Vercel project
  or Netlify site through CapyDB's token-connect integration and waits for the first push. The
  target comes from `.vercel/project.json` / `.netlify/state.json` (written by `vercel link` /
  `netlify link`) or `--vercel-project`/`--team` / `--site`; the token from `--token`,
  `VERCEL_TOKEN` or `NETLIFY_AUTH_TOKEN`. CapyDB stores the token encrypted and pushes again on
  every credential rotation; `--preview-branches` adds a preview database per branch deployment.
- **`capydb doctor --fix`** applies the fixes that are mechanical, then runs the checks: database
  env vars missing from the linked env file are added (existing values are never overwritten),
  drizzle-kit configs get `schemaFilter: ["public"]` and the direct URL for credentials, Prisma
  schemas get `directUrl`. Fixes that remove something - the link to a project that no longer
  exists, database vars in other env files that shadow the linked one - are asked one by one on a
  terminal, applied without asking with `--yes`, and skipped otherwise. Everything else is listed
  as manual. `doctor` also gained an `env_vars` check for the linked env file.


## [1.8.0] - 2026-09-26

### Changed

- **`capydb restore --target-kind project` takes an approval a person created instead of minting
  one.** The control plane no longer lets an API key approve its own overwrite restore, so the CLI
  stops minting the approval itself. An organization admin creates one on the project's Backups page
  in the dashboard and hands over the token; pass it with `--approval-token` or
  `CAPYDB_APPROVAL_TOKEN` (it works for 10 minutes). Without one the command stops before the
  destructive call and prints the Backups page URL.

## [1.7.1] - 2026-09-24

### Fixed

- **`capydb init drizzle` runs the Drizzle scaffold again.** `create` carried the alias `init`, and
  cobra's first match shadowed the `init` command group, so the documented `capydb init drizzle`
  started `capydb create` (and could create a project). The alias is removed; `capydb create` is the
  only spelling.
- **`capydb migrate rls` no longer reports a service escape it never emits.** With
  `--mode supabase-compat --role-model single`, `service_role`-only policies were reported as
  covered by an escape the bundle does not contain; the report now says no service path exists.
  capyrls v1.13.1.

## [1.7.0] - 2026-09-22

### Added

- **`capydb ephemeral destroy`**: ends this directory's ephemeral database now instead of at the
  72-hour mark, and removes the local record so the next `create` is not refused. Anonymous like
  `status`: the recorded claim token is the credential. A database that was already claimed is a
  project and is not touched. Needs a control plane that serves
  `DELETE /v1/ephemeral-databases/{projectID}`. Against an older control plane the command explains
  that early destroy is not supported yet and leaves the local record in place.

### Changed

- **`capydb migrate squash` follows the engine rename to `capysquash`** (github.com/capydatabase/capysquash).
  The install hint points at that repository, the validation DSN is handed over as
  `CAPYSQUASH_VALIDATION_DSN`, and both the `capysquash.external-validation.v1` contract of the
  next engine release and the `pgsquash.external-validation.v1` contract of engine v0.11.0 are
  accepted. A `capysquash` binary is preferred on PATH; the old `pgsquash` name still works.

## [1.6.0] - 2026-09-22

### Added

- **`capydb ephemeral create|status|claim`: a throwaway database with no account.** `create` sends no
  credential and needs no login: it provisions a real Postgres database, waits for it, and writes
  `DATABASE_URL` (and the framework's companions) to the env file exactly like `capydb create` - or,
  with `--no-env`, prints the connection strings instead. The database is destroyed with its data
  72 hours later unless it is claimed. `claim` logs in if needed, moves it into your organization as
  a normal project (it stops expiring and gains nightly backups), and links the directory; the data
  and connection strings do not change, so the app keeps running. The claim token is the database's
  only credential and cannot be recovered, so it is written to the git-ignored
  `.capydb/ephemeral.json` (mode 0600) before anything else can fail, never printed, and deleted once
  spent; `status --claim-url` prints the browser claim link on request. Requires capydbclient 1.13.0.

### Fixed

- **Homebrew no longer warns when it loads the installed cask.** The generated cask used the
  `postflight` stanza to strip the macOS quarantine attribute from the installed binary, which
  Homebrew deprecated in favour of the declarative `postflight_steps`; `brew` commands that load
  installed casks printed `Warning: Calling postflight is deprecated!` and asked the user to report
  it to the tap. The cask now emits `postflight_steps` instead, with the same effect. The warning
  clears for users once this release updates the tap.

## [1.5.0] - 2026-09-16

### Changed

- **K/V environment variables are now `CAPYKV_REST_URL` and `CAPYKV_REST_TOKEN`** (were
  `CAPYDB_KV_REST_URL` / `CAPYDB_KV_REST_TOKEN` in 1.2.0-1.4.0). `kv create --write-env`,
  `kv rotate-token --write-env`, `kv credentials` and `env pull` all use the new names; nothing
  reads the old ones. No store could exist yet - the K/V service has not been deployed - so there is
  nothing to migrate.

### Added

- **`CAPYKV_REDIS_URL`**, the published name for a K/V store's RESP URL. `kv create` and
  `kv rotate-token` print it with the token and, with `--write-env`, write it into the env file
  next to `CAPYKV_REST_URL` / `CAPYKV_REST_TOKEN` instead of printing it, since it carries
  the token as its password. `kv credentials` still prints the password-free form unlabelled,
  because that one is not connectable.

## [1.4.0] - 2026-09-11

### Added

- **`migrate rls` bundles now carry the FORCE foreign-key warning** (capyrls v1.13.0). Verified in
  the built binary rather than the build: a generated `capyrls_02_force_rls.sql` contains the
  `23503` symptom, the parent-only recipe and the note that the owner has no privileged view.

- **`migrate scan --source-url` reports unvalidated foreign keys.** A `NOT VALID` foreign key still
  owes Postgres a validation scan — and on a FORCEd destination that scan is the one thing row
  security applies to. Runtime enforcement and `CHECK` validation are unaffected, but
  `VALIDATE CONSTRAINT` (and `ADD CONSTRAINT ... FOREIGN KEY`) fail with `23503` naming rows that
  exist and are only invisible, which also means `drizzle-kit push` adding a foreign key later hits
  it. The preflight names each one with its parent, because the parent is the table that has to lose
  `FORCE` for the length of the statement. Validate them before the cutover and it never comes up.

## [1.3.0] - 2026-09-10

### Added

- **`capydb migrate verify-rls`** — proves a migrated policy corpus behaves identically instead of
  asking you to believe it. Reads every RLS-enabled table as every caller identity you supply,
  against both the old database and the new one, and diffs the answers. Row counts and denials are
  both evidence, so errors compare by SQLSTATE: "denied on both sides" is equivalence. Every read
  runs in its own rolled-back transaction — claims are transaction-local, so the transaction is the
  identity boundary, and the audit cannot alter the databases it audits.

  Catches the failure this exists for: RLS enabled but `FORCE` forgotten, where the app connects as
  the owner and every identity silently sees everything. Verified end to end on postgres:17 —
  `anon: 0 -> 3` on the un-FORCEd target, `IDENTICAL` once fixed, both databases unmodified.
- **Three RLS risk reports in `capydb migrate scan --source-url`**: FORCE/owner exposure (how many
  RLS-enabled tables would go inert on a single-credential destination), SECURITY DEFINER functions
  reaching an RLS table (split by whether they write and whether they swallow errors), and the
  policy→helper→own-table cycle graph, which is a static error under FORCE RLS. Each of these had to
  be written by hand during the myroomiev3 migration, and each changed the plan.
- `capydb projects always-on <on|off>` — keep a database awake instead of pausing it when idle.
  Production projects default to on.

### Fixed

- **The test suite no longer reads the developer's real environment.** Isolation was opt-in via
  `isolateUserConfig`, and 11 of 13 tests in `root_test.go` did not opt in — so anyone who exports
  `CAPYDB_API_KEY` (the normal way to use this CLI) saw six failures locally that CI never sees: the
  real key reached `httptest` servers as a 401, the "fails without auth" tests found auth, and the
  login tests reached the network and timed out. The failing set even varied between runs, which read
  as flakiness rather than a leak. A `TestMain` now clears every `CAPYDB_*` variable for the whole
  package, so the safe state is the default; a test that wants one set still calls `t.Setenv` and
  wins. Suite is green across repeated runs.

### Changed

- **`capydb migrate verify-rls --writes` also probes write reachability.** Reads alone cannot see a
  widened `UPDATE` policy: the battery reports IDENTICAL while the new database lets an identity
  update rows the old one protected. The probe is `SELECT ... FOR UPDATE`, which additionally
  applies the UPDATE policy's `USING` clause (verified on postgres:17: a table showing 2 rows to
  `SELECT` showed 1 under `FOR UPDATE`). It writes nothing and every probe is rolled back, but it
  does take row locks — hence opt-in, with a warning. Demonstrated end to end: reads report
  IDENTICAL, `--writes` reports `posts / user_a [write]: 1 -> 2`.
- **`capydb migrate verify` holds ONE connection for the whole watch** instead of spawning a `psql`
  process per sample, and no longer needs `psql` on PATH at all. Proven by counting sessions on the
  server: one, across a five-sample run. The pool is capped at 1 so the check cannot become the
  leftover connection it is looking for.

- `capydb migrate verify` is now bounded by wall clock rather than a sample count, and prints each
  sample as it lands. It previously slept `--interval` *and* waited for each query, so a 10-minute
  watch through a slow pooler took 25 minutes and printed nothing until the end. Ctrl-C now prints
  the verdict from the samples taken.
- `capydb create` states the two things about a fresh cell that were otherwise discovered later and
  expensively: whether it pauses when idle, and that `capydb advisor indexes` needs an extension
  whose enable **restarts the database** — a cheap decision on an empty cell, a maintenance window
  once it carries traffic.

## [1.2.0] - 2026-09-09

### Added

- **`capydb kv`** - the project's K/V store (CapyDB Knight/Valkyrie: key-value and
  rate limiting). `status` shows the store (and says so plainly when there is
  none, rather than failing); `create` provisions it; `credentials` prints the
  endpoints; `rotate-token` mints a new token; `flush` empties the keyspace;
  `delete` removes the store. `create`, `flush` and `delete` queue jobs and take
  `--wait`; `rotate-token` is synchronous, because the response is the only place
  the new plaintext exists.

  `create` and `rotate-token` are the only chance to capture the token - only
  its SHA-256 hash is stored, so it can be replaced but never recovered - and
  accept `--write-env` to merge `CAPYDB_KV_REST_URL` and `CAPYDB_KV_REST_TOKEN`
  into the project's env file through the same upsert every other credential
  write uses. With `--write-env` the token is written there and not also echoed
  into the terminal, so text output leaves no second copy in scrollback;
  `--output json` still carries it, because that document is what the caller
  parses.
  `credentials` deliberately cannot show the token: it explains why and points at
  `rotate-token` instead of returning a password-free RESP URL that would look
  like a working credential. `flush`, `delete` and `rotate-token` are irreversible
  and a K/V store has no backup, so each refuses to run unconfirmed.

- `capydb env pull` now refreshes `CAPYDB_KV_REST_URL` when the project has a K/V
  store, and stays silent when it does not. Only the URL: the token is not
  recoverable, so it is written once by `capydb kv create --write-env` and left
  untouched afterwards.

### Changed

- **Both container images were rebuilt on current Docker conventions (Engine 29 /
  BuildKit 0.33).** `Dockerfile` (the from-source path behind `make docker-build`)
  is now multi-stage with a `# syntax=docker/dockerfile:1` frontend, a read-only
  bind mount of the source instead of `COPY . .`, and BuildKit cache mounts for
  the module and build caches. The Go build cache now persists between builds,
  so editing a source file triggers an incremental recompile rather than a full
  rebuild, and editing `go.mod` downloads only the modules that actually
  changed. The toolchain runs on `$BUILDPLATFORM` and cross-compiles for
  `$TARGETPLATFORM` rather than building under emulation, and the runtime stage
  now carries OCI image labels and uses a numeric uid/gid (10001). The version,
  date, commit and `builtBy=docker` ldflags are unchanged.
- `Dockerfile.goreleaser` (the published image) collapses its `RUN chmod` layer
  into `COPY --chmod=0755 --chown=...`, and its `app` user becomes an explicit
  uid/gid 10001 so `runAsNonRoot` policies can verify it. The
  `ARG TARGETPLATFORM` / `COPY ${TARGETPLATFORM}/capydb` contract GoReleaser
  depends on is unchanged, and labels are still injected by GoReleaser rather
  than duplicated in the file.
- Added a `.dockerignore`, so `.git/`, local `.env` files, keys and build outputs
  no longer enter the build context.

### Added

- `capydb migrate scan --out <file>`: writes the whole assessment as a versioned JSON artifact -
  the graded verdict, the recommended migration path, every finding with its evidence, and the raw
  scan. Drop it on capydb.dev/switch/check for the same report as a page (parsed in the browser;
  the file is never uploaded), or gate a migration on it in CI:
  `capydb migrate scan --source-url "$URL" --output json | jq -e '.assessment.blockers|length==0'`.
  The grading lives in the CLI rather than in a web page, so the terminal, the JSON and the page
  cannot disagree about whether a migration is safe.

- `capydb migrate scan --project <ref>`: adds the control plane's import preflight to the
  assessment. That half is not a rule table - it connects to the source and simulates the actual
  restore against the actual target, so it reports which extensions get pre-created, which the
  restore cannot create at all, which event triggers are lost, and which foreign keys a
  public-schema dump would orphan. A **failing** preflight check becomes a blocker and re-grades the
  verdict, so a failed restore simulation is never printed underneath a clean bill of health; the
  scan still exits zero, because `capydb import preflight` is the gate that exits non-zero.

- **Live source identification.** `--source-url` now asks the server which provider runs it, rather
  than guessing from the hostname - which cannot work for Cloud SQL or AlloyDB (neither publishes a
  hostname), calls Heroku Postgres an ordinary EC2 instance, and says nothing at all about a source
  reached through a bastion. Detects Heroku, Aurora, RDS, AlloyDB, Cloud SQL, Azure, Neon,
  Supabase, Aiven and CapyDB from provider-owned roles, schemas, extensions and GUC namespaces, and
  reports the signals that decided it. Hostname classification remains for repo-only scans and now
  also covers PlanetScale, Timescale Cloud, Crunchy Bridge, DigitalOcean, Render, Railway and
  Aiven.

- **Streaming-import readiness.** The scan measures `wal_level`, the replication-slot budget, WAL
  senders and whether the connecting role has `REPLICATION`, then says whether `capydb import
  --follow` can run from this source *as connected* - and, when it cannot, the provider's own
  remediation (`rds.logical_replication` in an instance vs a cluster parameter group,
  `cloudsql.logical_decoding`, `alloydb.logical_decoding`, Azure's `wal_level` server parameter,
  Neon's console toggle). Heroku Postgres is reported as impossible rather than unconfigured: it
  offers neither logical replication nor superuser, so the migration is a dump and restore.

- **Physical inventory.** Table sizes with partitions summed into their parent, row estimates,
  index weight, tables over 100 GiB and 500 GiB, tables with no primary key, tables with neither a
  primary key nor a replica identity (which break a streaming import at the first `UPDATE`, midway
  through the migration rather than at setup), foreign-key cycles, sequences past 80% of their
  range, and indexes never scanned or duplicating another. All from catalog and statistics views -
  no table is read and nothing is counted with `count(*)`.

### Changed

- `capydb migrate scan` prints a graded verdict under the raw scan: a level (`ready` / `planning` /
  `assisted`), the recommended path with its commands, and findings separated into blockers,
  warnings and notes by what actually stops a migration. An extension nothing depends on is a note,
  because the dump drops it; an extension with dependent objects is a blocker, because the schema
  will not restore. No estimated duration is reported - CapyDB has no measured copy rate for an
  arbitrary source over an arbitrary network, and a fabricated one is worse than none because
  people schedule maintenance windows around it.

- `capydbclient` bumped to v1.10.0 (the import preflight's source-provider and
  replication-readiness fields) and `capyrls` to v1.11.0. The previous `capyrls v1.1.0` requirement
  was a tag published before the GitHub organisation rename, so its `go.mod` still declares the
  `capydatabase` module path and no build can resolve it - "module declares its path as
  github.com/capydatabase/capyrls". Pre-rename tags cannot be repaired, so the fix is to require one
  published afterwards. (capyrls v1.11.0 and v1.2.0 are the same commit; v1.11.0 is what
  `go get @latest` resolves to.)

- A scan run without `--source-url` now grades `planning`, never `ready`. It can still find real
  blockers in the repository, but "nothing to worry about" is not a conclusion available from not
  having looked at the database.


### Added

- `capydb advisor index-hygiene` (alias `unused-indexes`): lists indexes the database pays for on
  every write and never reads — no recorded scans, or covered by a wider index on the same table —
  each with a ready-to-run `DROP INDEX CONCURRENTLY`. Needs no extensions, unlike
  `capydb advisor indexes`. Constraints are never listed, and nothing is reported until a week of
  query statistics exists, so an index a monthly job uses is not mistaken for a dead one.

- `capydb sql --read-only`: runs the statement inside a `READ ONLY` transaction so the server
  itself refuses every write (DML, DDL, `TRUNCATE`, `SELECT INTO`, sequence advancement) -
  executor-proven, unlike client-side statement inspection. Contradicts and refuses
  `--allow-unqualified-writes`. `capydb doctor`'s migration-state probe now runs read-only.

### Changed

- `capydb metrics` shows a `SPILL` column on the slow-query table when any statement wrote to
  temporary files, plus the `SET LOCAL work_mem` recipe for fixing it on that one statement rather
  than globally. The column is hidden entirely when nothing spilled, and shows `-` rather than `0`
  on databases whose platform objects predate the counter.

- `capydbclient` bumped to v1.9.0 (approval tokens and read-only SQL).
- `capydb restore --target-kind project` now completes the control plane's approve-then-execute
  flow: after the interactive type-the-name confirmation (or `--confirm`), the CLI mints a
  single-use `project.restore_overwrite` approval token and attaches it to the restore, replacing
  the old `confirm_project_overwrite: true` request field. The command surface is unchanged.

- `capydb migrate squash --validation capydb` now generates a staged baseline,
  provisions one empty isolated preview cell, captures the catalog produced by
  the original migrations, resets the cell, compares the candidate catalog, and
  publishes the output only after equivalence is proven. The preview is deleted
  on success or failure, works within the Vibe plan's one-preview limit, and
  passes its database URL to `pgsquash` through an environment variable rather
  than command-line arguments.
- `--project`, `--output`, `--wait-timeout`, and `--preview-ttl-hours` controls
  for managed squash validation. Local Docker validation remains the default.
- Migration discovery now includes nested SQL files and the conventional
  `prisma/migrations` and `drizzle` directories in addition to Supabase and
  top-level migrations directories.

### Changed

- The missing-engine guidance now points to standalone `pgsquash` GitHub release
  archives, with `go install` retained as the source-build fallback.

## [1.1.0] - 2026-09-01

### Added

- `capydb migrate scan --source-url`: read-only live-database probes alongside the repo scan,
  because the repo and the database routinely disagree (measured on a real assessment: 620 policies
  in migration files vs 483 live; a "vestigial" client library that was the app's only data layer).
  The probes measure the live RLS corpus and classify how policies resolve the caller (direct
  `auth.*` vs app-defined helper functions whose bodies read `auth.jwt()`), count `auth.users` and
  the last sign-in (the zero-user window gate: migrate before onboarding users), split
  not-on-CapyDB extensions into likely-unused (0 dependent objects - filter from the dump) vs
  load-bearing, list storage buckets, detect absolute provider URLs persisted in data columns
  (the storage exit then needs a data backfill, not just an API swap), flag populated
  migration-bookkeeping tables as a possible import in flight (freeze other data movements during
  cutover), and compare the `supabase_realtime` publication against the code's actual
  subscriptions. All probes are best-effort, statement-timeout-capped, and the session is forced
  read-only.
- `capydb migrate scan` now recommends an RLS migration path: server code building anon-key
  clients (authorization delegated to RLS) or a large live corpus (≥50 policies) gets
  "keep the policies - `capydb migrate rls` + per-transaction context"; a small corpus with
  service-role/explicit-filter code style keeps the app-layer-guards recommendation.
- `capydb migrate scan` cross-checks the code's `.rpc()` call sites against local SQL (and, with
  `--source-url`, the live database): an RPC with no `CREATE FUNCTION` anywhere in the repo lives
  only in the provider's database and must be recovered before cutover.
- `capydb migrate rls --source-url`: convert from live-database introspection (capyrls's `live`
  loader) instead of parsing SQL files - migration folders drift from what is actually deployed.
- New dependency: `github.com/jackc/pgx/v5` (database/sql driver for the two `--source-url` modes).
- `capydb migrate squash`: analyze or consolidate a migration history via the open-source
  pgsquash engine (exec wrapper - the engine's SQL parser is cgo and this CLI is cross-compiled
  CGO-free). Read-only ANALYZE by default; `--workflow safe|fast` consolidates. Points at
  `go install github.com/capysquash/pgsquash-engine/cmd/pgsquash@latest` when the binary is
  missing. `migrate scan` now recommends it when a repo carries ≥50 migration files, and the
  `migration_history_not_baselined` lint fix text mentions it.

- capyrls bumped to v1.1.0: the `migrate rls` report now links converted policies to the helper
  functions they authorize through (annotated outcomes, per-routine reference counts, and an
  incomplete-until-ported warning).

## [1.0.0] - 2026-08-31

### Added

- `capydb db sql --allow-unqualified-writes`. The control plane now refuses an `UPDATE` or
  `DELETE` with no `WHERE` and any `TRUNCATE`; the flag opts out. Guarded by default here rather
  than opted out like the dashboard console, because the CLI is as likely to be inside a script as
  under a person, and a script is exactly the caller that should have to say it meant it.

- `capydb export`: queue a logical export (`pg_dump` custom-format archive) of the project
  database, wait for the job, and download the artifact with TTY-aware progress - plus
  `capydb export list` and `capydb export download --export <id>` to re-download within the 7-day
  window. Downloads refuse to overwrite an existing file. (Needs capydbclient v1.7.0.)
- `capydb logs --hours` now accepts up to 720 (30 days); the control plane serves windows older
  than 7 days from the platform log archive where it is configured, and rejects windows beyond the
  deployment's cap.
- `capydb psql` prints a one-line notice on stderr (`Resuming your database (usually under a
  second)...`) when connecting to a paused project database, so the scale-to-zero wake pause is
  explained instead of looking like a hang.
- `capydb restore --wait` ends a successful restore with a plain-language outcome: what the target
  (live database or preview) now contains and the command to verify it or fetch its connection
  string, instead of only the bare job block.
- `capydb import --wait` ends a successful import by stating that the project's live database now
  contains the imported data, with the verify command.
- Cloudflare integration support: `capydb integrations env --target wrangler` prints the
  `wrangler.jsonc` Hyperdrive binding fragment for a linked project, alongside the existing Vercel
  and Netlify payloads.
- `capydb psql` resolves the CapyDB root certificate for `sslmode=verify-full` connection strings,
  so `psql` no longer fails against a verified-TLS connection string.

### Changed

- The API types the CLI used to declare itself are now aliases of the shared `capydbclient` module,
  the single Go mirror of the OpenAPI component schemas. The CLI keeps only three local shapes
  (`Client`, `PreviewDetails`, `ProjectLogsQuery`); everything else comes from one definition shared
  with the Terraform provider. Tracks `capydbclient` v1.6.0.
- Go directive raised to 1.27.1.

### Fixed

- The Go module path is now `github.com/capydatabase/capydb-cli`, matching the repository, so
  `go install github.com/capydatabase/capydb-cli/cmd/capydb@latest` resolves. The old path
  (`github.com/capydatabase/capydb/cli`) named a repository that does not exist and never installed.
- `go.sum` was missing the module hash for `capydbclient` v1.7.0, so a clean checkout could not
  build the CLI.
- The dump-upload progress line (`capydb import --file`) is now TTY-aware: piped/CI runs get plain
  progress lines at most every 5 seconds and a final `Upload complete` line, instead of one
  carriage-return-garbled line in the log.
- `capydb backups list` can now report backup verification (`verified_at`, `verification_error`) —
  the CLI's local `Backup` type had been missing both fields.
- `capydb alerts` and `capydb sql` rendered `observed_value`, `limit_value` and `duration_ms` as
  decimals; the API sends integers. Values now render exactly as the API reports them.
- `capydb import preflight` surfaces the source's event triggers, which the local preflight type had
  been dropping.

## [2026-08-18]

### Fixed

- `capydb psql` builds a connection URL that carries the SSL root certificate, fixing connections to
  cells issued `sslmode=verify-full` strings.

## [2026-08-12]

### Added

- `capydb doctor` config-lint rules for `uuidv7()` defaults and missing `NOT NULL` constraints.

## [2026-08-11]

### Changed

- Tracks `capydbclient` v1.5.0 and `capyrls` v1.0.1.

## [2026-08-05]

### Added

- `capydb advisor` — index suggestions derived from the project's real query predicates, costed as
  hypothetical indexes so nothing is written to the database.
- `capydb migrate rls` — converts Supabase row-level-security policies to vanilla Postgres, backed
  by the `capyrls` engine, plus a matching `configlint` rule.

## [2026-07-29]

### Added

- `capydb upgrade major` with confirm and rollback, and webhook test-delivery support.

## [2026-07-24]

### Added

- `capydb doctor` and the `configlint` package: static inspection of a repo's database
  configuration, including drizzle/prisma migration-state checks that catch the `db:push` then
  `db:migrate` trap.
- `capydb upgrade minor` and `capydb extensions update` for managing Postgres versions and extension
  versions.

## [2026-07-23]

### Added

- Env-shadowing detection across `.env*` files — the same key pointing at different databases is
  reported by `migrate scan`, `link`, `create` and `doctor`.
- `drizzle-kit` table filter that excludes `pg_stat_statements` from push/pull, preventing a
  `DROP VIEW` against cells that still carry the views in `public`.

## [2026-07-22]

### Added

- Detection and warnings for pooler startup parameters that the pooled `:6432` endpoint rejects.
- Documented exit codes (0 success, 2 usage, 3 auth, 4 not found, 5 conflict, 6 timeout) and
  `capydb migrate scan` for planning a move from another provider.

## [2026-07-16]

### Added

- `capydb generate` (TypeScript, Zod, Drizzle types rendered server-side from the live schema),
  `capydb schema dump|diff`, `capydb init drizzle`, and `capydb migrate codemod neon`.

## [2026-06-03]

### Added

- First release: project linking, `env pull`, preview databases, imports, logs, and studio.

[Unreleased]: https://github.com/capydatabase/capydb-cli/compare/v1.8.0...HEAD
[1.8.0]: https://github.com/capydatabase/capydb-cli/compare/v1.7.1...v1.8.0
[1.7.1]: https://github.com/capydatabase/capydb-cli/compare/v1.7.0...v1.7.1
[1.7.0]: https://github.com/capydatabase/capydb-cli/compare/v1.6.0...v1.7.0
[1.6.0]: https://github.com/capydatabase/capydb-cli/compare/v1.5.0...v1.6.0
[1.5.0]: https://github.com/capydatabase/capydb-cli/compare/v1.4.0...v1.5.0
[1.4.0]: https://github.com/capydatabase/capydb-cli/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/capydatabase/capydb-cli/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/capydatabase/capydb-cli/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/capydatabase/capydb-cli/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/capydatabase/capydb-cli/releases/tag/v1.0.0
