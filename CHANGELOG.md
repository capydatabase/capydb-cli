# Changelog

All notable changes to the `capydb` CLI are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[SemVer](https://semver.org/).

Releases are cut with GoReleaser from a git tag; entries under **Unreleased** ship with the next tag.

## [Unreleased]

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

[Unreleased]: https://github.com/capy-base/capydb-cli/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/capy-base/capydb-cli/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/capy-base/capydb-cli/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/capy-base/capydb-cli/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/capy-base/capydb-cli/releases/tag/v1.0.0
