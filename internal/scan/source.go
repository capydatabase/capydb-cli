package scan

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Live-source probes for `capydb migrate scan --source-url`. The repo scan
// classifies what the CODE says; these probes classify what the DATABASE says.
// The two disagree in practice (myroomiev3 dogfood, 2026-09-01: 620 policies
// in the repo's migrations vs 483 live, "vestigial" client libraries that were
// the only data layer, an in-flight WordPress import visible only in table
// rows) - so a plan derived from the repo alone routinely mis-sizes the work.
//
// Every probe is best-effort: a failure (missing privilege, provider quirk,
// statement timeout) is recorded as a note and never aborts the scan. All
// statements are plain SELECTs and the session is forced read-only.

const (
	sourceProbeTimeout    = "5s"
	maxPersistedURLProbes = 40
)

// capydbExtensionCatalog is the set of extensions CapyDB can enable.
// KEEP IN LOCKSTEP with the catalog keys in
// backend/internal/service/extensions.go (plpgsql ships built in).
var capydbExtensionCatalog = map[string]bool{
	"plpgsql": true, "pgcrypto": true, "uuid-ossp": true, "pg_uuidv7": true,
	"citext": true, "hstore": true, "ltree": true, "vector": true,
	"vectorscale": true, "pg_trgm": true, "unaccent": true, "fuzzystrmatch": true,
	"rum": true, "pg_similarity": true, "pg_stat_statements": true,
	"pg_qualstats": true, "hypopg": true, "pg_ivm": true, "btree_gin": true,
	"btree_gist": true, "pgaudit": true, "pg_permissions": true, "hll": true,
	"roaringbitmap": true, "tdigest": true, "postgis": true, "pg_cron": true,
	"pgtap": true, "plpgsql_check": true,
}

// providerManagedSchemas are provider-owned schemas that never migrate.
// KEEP IN LOCKSTEP with supabaseManagedSchemas in
// backend/internal/service/preflight.go.
var providerManagedSchemas = []string{
	"auth", "storage", "realtime", "vault", "pgsodium", "supabase_functions",
	"supabase_migrations", "graphql", "graphql_public", "pgbouncer", "extensions", "net", "cron",
}

// authHelperBodyPattern matches Supabase auth helpers inside function bodies -
// the signal that a custom function (e.g. clerk_user_id()) is an auth wrapper.
var authHelperBodyPattern = regexp.MustCompile(`(?i)auth\.(uid|jwt|role|email)\s*\(`)

// importArtifactTablePattern matches table names that migration/import
// tooling leaves behind (wp_migration_map, import_log, etl_state...). Rows in
// such a table are the fingerprint of another data movement in flight.
var importArtifactTablePattern = regexp.MustCompile(`(?i)(^|_)(migration|import|etl)(_|$)|_map$|^wp_`)

// urlColumnNamePattern selects columns likely to hold asset URLs.
var urlColumnNamePattern = regexp.MustCompile(`(?i)(url|image|avatar|photo|media|file|attachment|logo|banner|picture)`)

// SourceFacts is what the live source database says about itself.
type SourceFacts struct {
	ServerVersion     string `json:"server_version"`
	DatabaseSizeBytes int64  `json:"database_size_bytes"`
	PublicTables      int    `json:"public_tables"`

	// Provider is the source provider as the SERVER identifies itself, one of
	// the Provider* identifiers. This beats the hostname classification
	// wherever the two disagree: Cloud SQL and AlloyDB publish no hostname at
	// all, Heroku Postgres answers on an ordinary EC2 name, and anything behind
	// a bastion arrives as an address.
	Provider string `json:"provider"`
	// ProviderSignals are the catalog facts that produced Provider, kept so a
	// surprising classification can be argued with.
	ProviderSignals []string `json:"provider_signals"`

	// Replication is the source's readiness for a streaming (`--follow`)
	// import. Measured, not assumed from the provider.
	Replication SourceReplication `json:"replication"`

	// Inventory is the physical shape of the database - what has to be copied
	// and which parts of it complicate the copy.
	Inventory SourceInventory `json:"inventory"`

	Policies SourcePolicies `json:"policies"`

	// AuthUsers is nil when the source has no readable auth.users table
	// (non-Supabase sources, or a role without SELECT on the auth schema).
	AuthUsers *SourceAuthUsers `json:"auth_users,omitempty"`

	Extensions []SourceExtension `json:"extensions"`

	// RealtimeTables are the tables in the supabase_realtime publication -
	// usually a superset of what the app actually subscribes to.
	RealtimeTables []string `json:"realtime_tables"`

	StorageBuckets []SourceBucket `json:"storage_buckets"`

	// PersistedURLColumns are data columns holding absolute provider storage
	// URLs ("table.column"). Their presence turns the storage exit into a data
	// backfill, not just an API swap.
	PersistedURLColumns []string `json:"persisted_url_columns"`

	// ImportArtifactTables are populated migration/import bookkeeping tables -
	// the signal that another data movement may be in flight.
	ImportArtifactTables []string `json:"import_artifact_tables"`

	// PublicFunctions holds the names of functions in the public schema, for
	// cross-checking the repo's .rpc() call sites.
	PublicFunctions []string `json:"public_functions"`

	// RLSExposure separates "RLS enabled" from "RLS applies to the role the app
	// will connect as" - see rlsgraph.go.
	RLSExposure SourceRLSExposure `json:"rls_exposure"`

	// DefinerFunctions are SECURITY DEFINER functions reaching an RLS table:
	// the owner bypass they rely on disappears on a single-credential
	// destination.
	DefinerFunctions []SourceDefinerFunction `json:"definer_functions"`

	// PolicyCycles are policies that reach their own table through a helper.
	PolicyCycles []SourcePolicyCycle `json:"policy_cycles"`

	// ForeignKeyHazards are NOT VALID foreign keys, which cannot be validated
	// once the destination FORCEs row security on their parent.
	ForeignKeyHazards []SourceForeignKeyHazard `json:"foreign_key_hazards"`

	// Notes records probes that were skipped or failed.
	Notes []string `json:"notes"`
}

// SourcePolicies classifies the live RLS corpus by how policies resolve the
// caller - the classification that decides the RLS migration path.
type SourcePolicies struct {
	Total int `json:"total"`
	// DirectAuthRefs reference auth.uid()/auth.jwt()/auth.role()/auth.email()
	// in the policy expression itself.
	DirectAuthRefs int `json:"direct_auth_refs"`
	// ViaHelpers reference app-defined functions whose bodies read auth.* -
	// invisible to a policy-text grep, but the same PostgREST dependency.
	ViaHelpers  int      `json:"via_helpers"`
	HelperNames []string `json:"helper_names"`
}

type SourceAuthUsers struct {
	Count      int64  `json:"count"`
	LastSignIn string `json:"last_sign_in,omitempty"`
}

type SourceExtension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Dependents counts objects outside the extension that depend on its
	// member objects (indexes over its opclasses, typed columns, generated
	// SQL bodies). Zero means LIKELY unused - plpgsql bodies that merely call
	// extension functions leave no pg_depend trace.
	Dependents int64 `json:"dependents"`
	// Available reports whether CapyDB's extension catalog covers it.
	Available bool `json:"available"`
}

type SourceBucket struct {
	Name    string `json:"name"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

// ProbeSource connects to the source database and runs the read-only probe
// battery. It returns an error only when the connection itself fails; every
// individual probe degrades to a note in Facts.Notes.
func ProbeSource(ctx context.Context, db *sql.DB) (*SourceFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to source: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// One session for every probe so the guards below hold throughout. Two
	// statements: pgx's extended protocol rejects multi-statement strings.
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = '"+sourceProbeTimeout+"'"); err != nil {
		return nil, fmt.Errorf("prepare read-only session: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET default_transaction_read_only = on"); err != nil {
		return nil, fmt.Errorf("prepare read-only session: %w", err)
	}

	facts := &SourceFacts{
		RealtimeTables: []string{}, StorageBuckets: []SourceBucket{},
		PersistedURLColumns: []string{}, ImportArtifactTables: []string{},
		Extensions: []SourceExtension{}, PublicFunctions: []string{}, Notes: []string{},
		ProviderSignals: []string{}, Provider: ProviderOther,
		DefinerFunctions: []SourceDefinerFunction{}, PolicyCycles: []SourcePolicyCycle{},
		RLSExposure:       SourceRLSExposure{Owners: []string{}},
		ForeignKeyHazards: []SourceForeignKeyHazard{},
	}
	note := func(probe string, err error) {
		facts.Notes = append(facts.Notes, fmt.Sprintf("%s probe skipped: %s", probe, compactError(err)))
	}

	if err := probeBasics(ctx, conn, facts); err != nil {
		note("basics", err)
	}
	if err := probePolicies(ctx, conn, facts); err != nil {
		note("policies", err)
	}
	if err := probeRLSExposure(ctx, conn, facts); err != nil {
		note("RLS force/owner exposure", err)
	}
	if err := probeRLSGraph(ctx, conn, facts); err != nil {
		note("definer/cycle graph", err)
	}
	if err := probeForeignKeyHazards(ctx, conn, facts); err != nil {
		note("unvalidated foreign keys", err)
	}
	if err := probeAuthUsers(ctx, conn, facts); err != nil {
		note("auth.users", err)
	}
	if err := probeExtensions(ctx, conn, facts); err != nil {
		note("extensions", err)
	}
	if err := probeRealtime(ctx, conn, facts); err != nil {
		note("realtime publication", err)
	}
	if err := probeStorage(ctx, conn, facts); err != nil {
		note("storage buckets", err)
	}
	probePersistedURLs(ctx, conn, facts, note)
	if err := probeImportArtifacts(ctx, conn, facts); err != nil {
		note("import artifacts", err)
	}
	if err := probePublicFunctions(ctx, conn, facts); err != nil {
		note("public functions", err)
	}
	if err := probeProvider(ctx, conn, facts); err != nil {
		note("provider identification", err)
	}
	if err := probeReplication(ctx, conn, facts); err != nil {
		note("replication readiness", err)
	}
	probeInventory(ctx, conn, facts, note)
	facts.refreshReplicationReadiness()
	return facts, nil
}

func probeBasics(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	return conn.QueryRowContext(ctx, `
		select current_setting('server_version'),
		       pg_database_size(current_database()),
		       (select count(*) from pg_catalog.pg_tables where schemaname = 'public')`).
		Scan(&facts.ServerVersion, &facts.DatabaseSizeBytes, &facts.PublicTables)
}

// probePolicies classifies every app-schema policy: direct auth.* references
// vs references to app-defined helper functions whose bodies read auth.*.
func probePolicies(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	helperSet := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `
		select p.proname
		from pg_catalog.pg_proc p
		join pg_catalog.pg_namespace n on n.oid = p.pronamespace
		where n.nspname not in (`+quotedSchemaList()+`, 'pg_catalog', 'information_schema')
		  and p.prosrc ~* 'auth\.(uid|jwt|role|email)\s*\('`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		helperSet[name] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = conn.QueryContext(ctx, `
		select coalesce(qual, '') || ' ' || coalesce(with_check, '')
		from pg_catalog.pg_policies
		where schemaname not in (`+quotedSchemaList()+`)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	usedHelpers := map[string]bool{}
	for rows.Next() {
		var expression string
		if err := rows.Scan(&expression); err != nil {
			return err
		}
		facts.Policies.Total++
		if authHelperBodyPattern.MatchString(expression) {
			facts.Policies.DirectAuthRefs++
		}
		for helper := range helperSet {
			if strings.Contains(expression, helper+"(") {
				facts.Policies.ViaHelpers++
				usedHelpers[helper] = true
				break
			}
		}
	}
	for helper := range usedHelpers {
		facts.Policies.HelperNames = append(facts.Policies.HelperNames, helper)
	}
	sort.Strings(facts.Policies.HelperNames)
	return rows.Err()
}

func probeAuthUsers(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, `
		select exists(select 1 from pg_catalog.pg_tables where schemaname = 'auth' and tablename = 'users')`).
		Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	users := &SourceAuthUsers{}
	var lastSignIn sql.NullString
	if err := conn.QueryRowContext(ctx,
		`select count(*), max(last_sign_in_at)::text from auth.users`).
		Scan(&users.Count, &lastSignIn); err != nil {
		return err
	}
	users.LastSignIn = lastSignIn.String
	facts.AuthUsers = users
	return nil
}

// probeExtensions counts, per extension, the objects OUTSIDE it that depend on
// its member objects - the closest thing Postgres has to a usage signal.
// KEEP IN LOCKSTEP with countExtensionDependents in
// backend/internal/service/preflight.go: the two NOT EXISTS clauses are
// load-bearing - extensions' own operator-family rows (pg_amop/pg_amproc) and
// member-domain constraints are 'n'/'a' dependents of members without being
// members, so the naive count reports dozens of false dependents on a
// completely unused extension (verified empirically against PG17).
func probeExtensions(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	dependents := map[string]int64{}
	rows, err := conn.QueryContext(ctx, `
		WITH members AS (
			SELECT ext.extname, dep.classid, dep.objid
			FROM pg_depend dep
			JOIN pg_extension ext ON ext.oid = dep.refobjid
			WHERE dep.refclassid = 'pg_extension'::regclass
			  AND dep.deptype = 'e'
		)
		SELECT member.extname, COUNT(DISTINCT (dep.classid, dep.objid))
		FROM members member
		JOIN pg_depend dep
		  ON dep.refclassid = member.classid AND dep.refobjid = member.objid
		WHERE dep.deptype IN ('n', 'a')
		  AND NOT EXISTS (
			SELECT 1 FROM members other
			WHERE other.classid = dep.classid AND other.objid = dep.objid
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM pg_depend sub
			JOIN members other ON other.classid = sub.refclassid AND other.objid = sub.refobjid
			WHERE sub.classid = dep.classid AND sub.objid = dep.objid
			  AND sub.deptype IN ('i', 'a')
		  )
		GROUP BY member.extname`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			_ = rows.Close()
			return err
		}
		dependents[name] = count
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = conn.QueryContext(ctx, `
		select e.extname, e.extversion
		from pg_catalog.pg_extension e
		where e.extname <> 'plpgsql'
		order by e.extname`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var extension SourceExtension
		if err := rows.Scan(&extension.Name, &extension.Version); err != nil {
			return err
		}
		extension.Dependents = dependents[extension.Name]
		extension.Available = capydbExtensionCatalog[extension.Name]
		facts.Extensions = append(facts.Extensions, extension)
	}
	return rows.Err()
}

func probeRealtime(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	rows, err := conn.QueryContext(ctx, `
		select schemaname || '.' || tablename
		from pg_catalog.pg_publication_tables
		where pubname = 'supabase_realtime'
		order by 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return err
		}
		facts.RealtimeTables = append(facts.RealtimeTables, table)
	}
	return rows.Err()
}

func probeStorage(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, `
		select exists(select 1 from pg_catalog.pg_tables where schemaname = 'storage' and tablename = 'objects')`).
		Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := conn.QueryContext(ctx, `
		select bucket_id, count(*), coalesce(sum((metadata ->> 'size')::bigint), 0)
		from storage.objects
		group by 1 order by 2 desc`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var bucket SourceBucket
		if err := rows.Scan(&bucket.Name, &bucket.Objects, &bucket.Bytes); err != nil {
			return err
		}
		facts.StorageBuckets = append(facts.StorageBuckets, bucket)
	}
	return rows.Err()
}

// probePersistedURLs samples URL-shaped public columns for absolute provider
// storage URLs. Bounded (column-name heuristic, capped candidate list, the
// session statement timeout) so a huge table degrades to a note, not a hang.
func probePersistedURLs(ctx context.Context, conn *sql.Conn, facts *SourceFacts, note func(string, error)) {
	rows, err := conn.QueryContext(ctx, `
		select c.table_name, c.column_name
		from information_schema.columns c
		join pg_catalog.pg_tables t on t.schemaname = c.table_schema and t.tablename = c.table_name
		where c.table_schema = 'public'
		  and c.data_type in ('text', 'character varying', 'jsonb')
		order by c.table_name, c.column_name`)
	if err != nil {
		note("persisted URLs", err)
		return
	}
	type column struct{ table, name string }
	var candidates []column
	for rows.Next() {
		var candidate column
		if err := rows.Scan(&candidate.table, &candidate.name); err != nil {
			_ = rows.Close()
			note("persisted URLs", err)
			return
		}
		if urlColumnNamePattern.MatchString(candidate.name) {
			candidates = append(candidates, candidate)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		note("persisted URLs", err)
		return
	}
	if len(candidates) > maxPersistedURLProbes {
		note("persisted URLs", fmt.Errorf("only the first %d of %d URL-shaped columns were sampled", maxPersistedURLProbes, len(candidates)))
		candidates = candidates[:maxPersistedURLProbes]
	}
	for _, candidate := range candidates {
		var found bool
		query := fmt.Sprintf(
			`select exists(select 1 from public.%s where %s::text like '%%.supabase.co/storage/%%')`,
			quoteIdentifier(candidate.table), quoteIdentifier(candidate.name))
		if err := conn.QueryRowContext(ctx, query).Scan(&found); err != nil {
			note("persisted URLs ("+candidate.table+"."+candidate.name+")", err)
			continue
		}
		if found {
			facts.PersistedURLColumns = append(facts.PersistedURLColumns, candidate.table+"."+candidate.name)
		}
	}
}

func probeImportArtifacts(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	rows, err := conn.QueryContext(ctx, `
		select relname, n_live_tup
		from pg_catalog.pg_stat_user_tables
		where schemaname = 'public' and n_live_tup > 0
		order by n_live_tup desc`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var liveRows int64
		if err := rows.Scan(&name, &liveRows); err != nil {
			return err
		}
		if importArtifactTablePattern.MatchString(name) {
			facts.ImportArtifactTables = append(facts.ImportArtifactTables, fmt.Sprintf("%s (%d rows)", name, liveRows))
		}
	}
	return rows.Err()
}

func probePublicFunctions(ctx context.Context, conn *sql.Conn, facts *SourceFacts) error {
	rows, err := conn.QueryContext(ctx, `
		select distinct p.proname
		from pg_catalog.pg_proc p
		join pg_catalog.pg_namespace n on n.oid = p.pronamespace
		where n.nspname = 'public'
		order by 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		facts.PublicFunctions = append(facts.PublicFunctions, name)
	}
	return rows.Err()
}

// VestigialExtensions returns installed extensions CapyDB does not offer that
// nothing on the source appears to use - the safe-to-skip set.
func (f *SourceFacts) VestigialExtensions() []string {
	var names []string
	for _, extension := range f.Extensions {
		if !extension.Available && extension.Dependents == 0 {
			names = append(names, extension.Name)
		}
	}
	return names
}

// BlockingExtensions returns installed extensions CapyDB does not offer that
// DO have dependent objects - the ones that need a real decision.
func (f *SourceFacts) BlockingExtensions() []string {
	var names []string
	for _, extension := range f.Extensions {
		if !extension.Available && extension.Dependents > 0 {
			names = append(names, fmt.Sprintf("%s (%d dependent objects)", extension.Name, extension.Dependents))
		}
	}
	return names
}

func quotedSchemaList() string {
	quoted := make([]string, len(providerManagedSchemas))
	for index, schema := range providerManagedSchemas {
		quoted[index] = "'" + schema + "'"
	}
	return strings.Join(quoted, ", ")
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// compactError trims driver noise down to a single readable line.
func compactError(err error) string {
	message := strings.TrimSpace(err.Error())
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		message = message[:index]
	}
	return message
}
