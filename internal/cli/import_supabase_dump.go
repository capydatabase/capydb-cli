package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	capyrls "github.com/capydatabase/capyrls"
)

// `capydb import --from-supabase-dump <file>` restores a Supabase database
// dump (pg_dump custom format) into the project in the order that works for
// a tenant role, which is the recipe proven by the myroomiev3 migration:
//
//  1. capyrls prelude: the auth.* compatibility shim (auth.uid(), auth.jwt(),
//     ...) so column defaults and functions that call it resolve
//  2. the app schemas' pre-data (types, tables, functions, views)
//  3. data (COPY; no constraints or triggers exist yet, so order is free)
//  4. post-data (indexes, constraints, triggers)
//  5. capyrls FORCE ROW LEVEL SECURITY, then the converted policies
//
// all in ONE transaction: a failure anywhere leaves the project untouched.
//
// What does not travel: Supabase-managed schemas (auth, storage, realtime,
// ...), the dump's own policies (step 5 replaces them), grants to the
// PostgREST roles, publications, event triggers, and foreign keys that point
// into the managed schemas (auth.users) - each is listed. The dump's
// `extensions.` qualifications are rewritten to `public.` when the project
// keeps its extensions in public (a tenant role cannot move them).

// supabaseManagedSchemas are never restored. KEEP IN LOCKSTEP with
// SUPABASE_MANAGED_SCHEMAS in capydb-import-db.sh.j2 (infrastructure).
var supabaseManagedSchemas = map[string]bool{
	"auth": true, "storage": true, "realtime": true, "_realtime": true, "extensions": true,
	"graphql": true, "graphql_public": true, "pgbouncer": true, "pgsodium": true, "pgsodium_masks": true,
	"vault": true, "supabase_functions": true, "supabase_migrations": true, "net": true, "pgtle": true,
	"_analytics": true, "cron": true, "information_schema": true,
}

// supabasePlatformExtensions are Supabase's own extensions; the app does not
// need them on CapyDB.
var supabasePlatformExtensions = map[string]bool{
	"pg_graphql": true, "pg_net": true, "pgsodium": true, "supabase_vault": true, "wrappers": true,
	"pg_stat_statements": true, "pgjwt": true, "plpgsql": true, "pg_cron": true, "hypopg": true, "index_advisor": true,
}

// droppedTOCTypes never travel: policies come from capyrls, ACLs name
// PostgREST roles, publications are Realtime's, schemas are created
// separately.
var droppedTOCTypes = map[string]bool{
	"POLICY": true, "ACL": true, "DEFAULT ACL": true, "PUBLICATION": true, "PUBLICATION TABLE": true,
	"PUBLICATION TABLES IN SCHEMA": true, "EVENT TRIGGER": true, "SUBSCRIPTION": true, "SCHEMA": true,
	"EXTENSION": true, "DATABASE": true, "DATABASE PROPERTIES": true,
}

// tocTypes are pg_restore --list object types, longest first so a
// multi-word type wins over its first word.
var tocTypes = []string{
	"PUBLICATION TABLES IN SCHEMA", "MATERIALIZED VIEW DATA", "TEXT SEARCH CONFIGURATION", "TEXT SEARCH DICTIONARY",
	"FOREIGN DATA WRAPPER", "DATABASE PROPERTIES", "SEQUENCE OWNED BY", "PUBLICATION TABLE", "PROCEDURAL LANGUAGE",
	"MATERIALIZED VIEW", "CHECK CONSTRAINT", "OPERATOR FAMILY", "STATISTICS DATA", "OPERATOR CLASS", "FOREIGN SERVER",
	"FK CONSTRAINT", "EVENT TRIGGER", "FOREIGN TABLE", "SEQUENCE SET", "ROW SECURITY", "TABLE ATTACH", "INDEX ATTACH",
	"USER MAPPING", "LARGE OBJECT", "DEFAULT ACL", "TABLE DATA", "SHELL TYPE",
}

// tocEntry is one pg_restore --list line.
type tocEntry struct {
	line   string
	kind   string
	schema string
	name   string
}

// parseTOC parses pg_restore --list output ("ID; OID OID TYPE SCHEMA NAME
// OWNER"), skipping comments and blank lines.
func parseTOC(listing string) []tocEntry {
	var entries []tocEntry
	for _, line := range strings.Split(listing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") {
			continue
		}
		_, rest, ok := strings.Cut(trimmed, "; ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			continue
		}
		body := strings.Join(fields[2:], " ")
		kind := ""
		for _, candidate := range tocTypes {
			if strings.HasPrefix(body, candidate+" ") {
				kind = candidate
				break
			}
		}
		if kind == "" {
			kind, _, _ = strings.Cut(body, " ")
		}
		remaining := strings.Fields(strings.TrimPrefix(body, kind))
		entry := tocEntry{line: trimmed, kind: kind}
		if len(remaining) > 0 {
			entry.schema = remaining[0]
		}
		switch {
		case len(remaining) > 2:
			// The last field is the owner.
			entry.name = strings.Join(remaining[1:len(remaining)-1], " ")
		case len(remaining) == 2:
			// Entries without an owner (EXTENSION).
			entry.name = remaining[1]
		}
		entries = append(entries, entry)
	}
	return entries
}

// supabaseRestorePlan is what the restore keeps and drops.
type supabaseRestorePlan struct {
	AppSchemas         []string   `json:"app_schemas"`
	Kept               []tocEntry `json:"-"`
	KeptCount          int        `json:"kept_entries"`
	Dropped            []string   `json:"dropped"`
	DroppedAuthFKs     []string   `json:"dropped_foreign_keys"`
	DumpExtensions     []string   `json:"extensions"`
	MissingExtensions  []string   `json:"missing_extensions"`
	RewriteExtensions  bool       `json:"rewrite_extensions_to_public"`
	ResidualReferences []string   `json:"residual_references"`
}

// planSupabaseRestore filters the TOC to the app schemas.
func planSupabaseRestore(entries []tocEntry) supabaseRestorePlan {
	plan := supabaseRestorePlan{AppSchemas: []string{}, Dropped: []string{}, DroppedAuthFKs: []string{}, DumpExtensions: []string{}, MissingExtensions: []string{}, ResidualReferences: []string{}}
	schemas := map[string]bool{"public": true}
	for _, entry := range entries {
		if entry.kind == "SCHEMA" && !supabaseManagedSchemas[entry.name] && !strings.HasPrefix(entry.name, "pg_") {
			schemas[entry.name] = true
		}
		if entry.kind == "EXTENSION" && !supabasePlatformExtensions[entry.name] {
			plan.DumpExtensions = append(plan.DumpExtensions, entry.name)
		}
	}
	for schema := range schemas {
		plan.AppSchemas = append(plan.AppSchemas, schema)
	}
	sort.Strings(plan.AppSchemas)
	sort.Strings(plan.DumpExtensions)

	droppedKinds := map[string]int{}
	for _, entry := range entries {
		switch {
		case droppedTOCTypes[entry.kind]:
			droppedKinds[entry.kind]++
		case !schemas[entry.schema]:
			droppedKinds["objects in Supabase-managed schemas"]++
		default:
			plan.Kept = append(plan.Kept, entry)
		}
	}
	for kind, count := range droppedKinds {
		plan.Dropped = append(plan.Dropped, fmt.Sprintf("%d %s", count, kind))
	}
	sort.Strings(plan.Dropped)
	plan.KeptCount = len(plan.Kept)
	return plan
}

func tocListing(entries []tocEntry) string {
	var b strings.Builder
	for _, entry := range entries {
		b.WriteString(entry.line)
		b.WriteByte('\n')
	}
	return b.String()
}

const sqlIdent = `("(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)`

// foreignKeyStatement matches pg_dump's FK constraint statement.
var foreignKeyStatement = regexp.MustCompile(`ALTER TABLE (?:ONLY )?` + sqlIdent + `\.` + sqlIdent + `\s+ADD CONSTRAINT ` + sqlIdent + ` FOREIGN KEY [^;]*? REFERENCES ` + sqlIdent + `\.` + sqlIdent)

func unquoteIdent(value string) string {
	if strings.HasPrefix(value, `"`) {
		return strings.ReplaceAll(strings.Trim(value, `"`), `""`, `"`)
	}
	return value
}

// managedForeignKeys finds FK constraints in post-data SQL that reference a
// Supabase-managed schema, as "schema table constraint" keys matching the TOC
// (schema, "table constraint") of their FK CONSTRAINT entries.
func managedForeignKeys(postData string) map[string]string {
	found := map[string]string{}
	for _, match := range foreignKeyStatement.FindAllStringSubmatch(postData, -1) {
		schema, table, constraint := unquoteIdent(match[1]), unquoteIdent(match[2]), unquoteIdent(match[3])
		refSchema, refTable := unquoteIdent(match[4]), unquoteIdent(match[5])
		if supabaseManagedSchemas[refSchema] {
			found[schema+" "+table+" "+constraint] = fmt.Sprintf("%s.%s %s -> %s.%s", schema, table, constraint, refSchema, refTable)
		}
	}
	return found
}

// residualReference finds references into managed schemas left in the SQL.
var residualReference = regexp.MustCompile(`\b(auth|storage|realtime|vault|pgsodium|supabase_functions|graphql|net)\.[a-z_]+`)

// extensionQualifier matches the dump's extension-schema qualification.
var extensionQualifier = regexp.MustCompile(`\bextensions\.`)

func runTool(ctx context.Context, name string, args ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found in PATH; install the Postgres client tools (e.g. `brew install libpq` or `apt install postgresql-client`)", name)
	}
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// supabaseDumpSQL renders one section of the filtered dump as SQL.
func supabaseDumpSQL(ctx context.Context, dump, listFile, section string) (string, error) {
	args := []string{"--no-owner", "--no-privileges", "--file", "-"}
	if section != "" {
		args = append(args, "--section", section)
	}
	if listFile != "" {
		args = append(args, "--use-list", listFile)
	}
	return runTool(ctx, "pg_restore", append(args, dump)...)
}

// cleanRestoreSQL drops SET lines a newer pg_dump emits that an older server
// rejects, and optionally rewrites extension qualifications.
func cleanRestoreSQL(sql string, rewriteExtensions bool) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(sql, "\n") {
		if strings.HasPrefix(line, "SET default_table_access_method") || strings.HasPrefix(line, "SET transaction_timeout") {
			continue
		}
		b.WriteString(line)
	}
	if rewriteExtensions {
		return extensionQualifier.ReplaceAllString(b.String(), "public.")
	}
	return b.String()
}

// prepareSupabaseRestore reads the dump and produces the ordered SQL files
// in workDir plus the plan. Nothing touches the target.
func prepareSupabaseRestore(ctx context.Context, dump, workDir string, target *pgx.Conn, rls capyrls.Options) (supabaseRestorePlan, *capyrls.Result, []string, error) {
	listing, err := runTool(ctx, "pg_restore", "--list", dump)
	if err != nil {
		return supabaseRestorePlan{}, nil, nil, fmt.Errorf("read the dump's table of contents (a custom-format pg_dump file is required): %w", err)
	}
	plan := planSupabaseRestore(parseTOC(listing))

	// Foreign keys into auth/storage cannot be restored: their targets do not
	// travel. Find them in the post-data SQL and drop their TOC entries.
	listFile := filepath.Join(workDir, "keep.list")
	if err := os.WriteFile(listFile, []byte(tocListing(plan.Kept)), 0o600); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	postData, err := supabaseDumpSQL(ctx, dump, listFile, "post-data")
	if err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	if managed := managedForeignKeys(postData); len(managed) > 0 {
		kept := plan.Kept[:0]
		for _, entry := range plan.Kept {
			if description, ok := managed[entry.schema+" "+entry.name]; ok && entry.kind == "FK CONSTRAINT" {
				plan.DroppedAuthFKs = append(plan.DroppedAuthFKs, description)
				continue
			}
			kept = append(kept, entry)
		}
		plan.Kept = kept
		plan.KeptCount = len(kept)
		sort.Strings(plan.DroppedAuthFKs)
		if err := os.WriteFile(listFile, []byte(tocListing(plan.Kept)), 0o600); err != nil {
			return supabaseRestorePlan{}, nil, nil, err
		}
	}

	// Extensions: the ones the app uses must already be on the project, and
	// the dump's `extensions.` qualifications must point where they live.
	installed := map[string]string{}
	rows, err := target.Query(ctx, "SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace")
	if err != nil {
		return supabaseRestorePlan{}, nil, nil, fmt.Errorf("list project extensions: %w", err)
	}
	for rows.Next() {
		var name, schema string
		if err := rows.Scan(&name, &schema); err != nil {
			rows.Close()
			return supabaseRestorePlan{}, nil, nil, err
		}
		installed[name] = schema
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	plan.RewriteExtensions = true
	for _, extension := range plan.DumpExtensions {
		schema, ok := installed[extension]
		if !ok {
			plan.MissingExtensions = append(plan.MissingExtensions, extension)
			continue
		}
		if schema == "extensions" {
			plan.RewriteExtensions = false
		}
	}

	// Policies: convert the dump's full schema (including the POLICY entries
	// the restore itself drops).
	fullSchema, err := runTool(ctx, "pg_restore", "--schema-only", "--no-owner", "--file", "-", dump)
	if err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	converted, err := capyrls.Convert([]capyrls.Source{{Name: filepath.Base(dump), SQL: fullSchema}}, rls)
	if err != nil {
		return supabaseRestorePlan{}, nil, nil, fmt.Errorf("convert policies: %w", err)
	}

	var files []string
	write := func(name, content string) error {
		path := filepath.Join(workDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		files = append(files, path)
		return nil
	}
	bundle := map[string]string{}
	for _, file := range converted.Files {
		bundle[file.Name] = file.SQL
	}
	if err := write("01_prelude.sql", bundle["capyrls_01_prelude.sql"]); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	var schemas strings.Builder
	for _, schema := range plan.AppSchemas {
		if schema != "public" {
			fmt.Fprintf(&schemas, "CREATE SCHEMA IF NOT EXISTS %s;\n", quoteIdent(schema))
		}
	}
	if err := write("02_schemas.sql", schemas.String()); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	var residual []string
	for i, section := range []string{"pre-data", "data", "post-data"} {
		sql, err := supabaseDumpSQL(ctx, dump, listFile, section)
		if err != nil {
			return supabaseRestorePlan{}, nil, nil, err
		}
		sql = cleanRestoreSQL(sql, plan.RewriteExtensions)
		if section != "data" {
			residual = append(residual, residualReference.FindAllString(sql, -1)...)
		}
		if err := write(fmt.Sprintf("%02d_%s.sql", i+3, section), sql); err != nil {
			return supabaseRestorePlan{}, nil, nil, err
		}
	}
	if err := write("06_force_rls.sql", bundle["capyrls_02_force_rls.sql"]); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	if err := write("07_policies.sql", bundle["capyrls_03_policies.sql"]); err != nil {
		return supabaseRestorePlan{}, nil, nil, err
	}
	plan.ResidualReferences = uniqueSorted(residual)
	return plan, converted, files, nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		// auth.uid() and friends are provided by the shim.
		if strings.HasPrefix(value, "auth.") && !strings.HasPrefix(value, "auth.users") && !strings.HasPrefix(value, "auth.identities") && !strings.HasPrefix(value, "auth.sessions") {
			continue
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// applySupabaseRestore runs every file in one psql transaction.
func applySupabaseRestore(ctx context.Context, stdout, stderr io.Writer, connectionURL string, files []string) error {
	path, err := psqlPath()
	if err != nil {
		return err
	}
	args := []string{psqlConnectionURL(connectionURL), "--no-psqlrc", "--quiet", "--single-transaction", "--set", "ON_ERROR_STOP=1"}
	for _, file := range files {
		args = append(args, "--file", file)
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("restore rolled back, nothing was written (psql: %w)", err)
	}
	return nil
}

func (a *app) runSupabaseDumpImport(cmd *cobra.Command, options importExtensionOptions) error {
	ctx := cmd.Context()
	flags := cmd.Flags()
	sourceURL, _ := flags.GetString("source-url")
	dumpFile, _ := flags.GetString("file")
	follow, _ := flags.GetBool("follow")
	recreate, _ := flags.GetBool("recreate")
	confirm, _ := flags.GetBool("confirm")
	projectRef, _ := flags.GetString("project")
	switch {
	case strings.TrimSpace(sourceURL) != "" || strings.TrimSpace(dumpFile) != "":
		return usageErrorf("--from-supabase-dump restores a dump file; it cannot be combined with --source-url or --file")
	case follow, recreate:
		return usageErrorf("--from-supabase-dump cannot be combined with --follow or --recreate")
	}
	dump := strings.TrimSpace(options.supabaseDump)
	if !fileExists(dump) {
		return usageErrorf("%s does not exist", dump)
	}
	rlsOptions := capyrls.Options{Mode: capyrls.ModeCompat, RoleModel: capyrls.RoleSingle}
	switch options.uidType {
	case "uuid":
		rlsOptions.UIDType = capyrls.UIDUUID
	case "text":
		rlsOptions.UIDType = capyrls.UIDText
	default:
		return usageErrorf("unknown --uid-type %q (uuid or text)", options.uidType)
	}
	progress := cmd.OutOrStdout()
	if a.jsonOutput() {
		progress = cmd.ErrOrStderr()
	}

	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return err
	}
	project, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return err
	}
	connections, err := client.GetProjectConnection(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("fetch project connections: %w", err)
	}
	target, err := pgx.Connect(ctx, connections.DirectURL)
	if err != nil {
		return fmt.Errorf("connect to project %s: %w", project.Name, err)
	}
	defer func() { _ = target.Close(context.Background()) }()

	workDir, err := os.MkdirTemp("", "capydb-supabase-restore-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	plan, converted, files, err := prepareSupabaseRestore(ctx, dump, workDir, target, rlsOptions)
	if err != nil {
		return err
	}
	written, err := writeRLSBundle(converted, options.rlsOut)
	if err != nil {
		return fmt.Errorf("write the policy bundle: %w", err)
	}

	var existing int
	if err := target.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')`, plan.AppSchemas).Scan(&existing); err != nil {
		return fmt.Errorf("inspect project %s: %w", project.Name, err)
	}

	writeSupabaseRestorePlan(progress, plan, written)
	if len(plan.MissingExtensions) > 0 {
		return fmt.Errorf("the dump uses extension(s) the project does not have: %s; enable them first (`capydb extensions enable <name>`)", strings.Join(plan.MissingExtensions, ", "))
	}
	if existing > 0 {
		return fmt.Errorf("project %s already has %d table(s) in %s; --from-supabase-dump restores into an empty project (create a fresh one)", project.Name, existing, strings.Join(plan.AppSchemas, ", "))
	}
	if options.dryRun {
		if a.jsonOutput() {
			return printJSON(cmd.OutOrStdout(), map[string]any{"dry_run": true, "plan": plan, "rls_bundle": written})
		}
		_, _ = fmt.Fprintln(progress, "Dry run - nothing was written to the project.")
		return nil
	}

	confirmed, err := confirmProjectDestructiveAction(cmd, project, confirm,
		"This will restore the Supabase dump into the LIVE database for project %q (%s).\n")
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("import not confirmed; pass --confirm or confirm interactively")
	}
	_, _ = fmt.Fprintf(progress, "Restoring into %s (one transaction: shim, schema, data, constraints, policies)...\n", project.Name)
	if err := applySupabaseRestore(ctx, progress, cmd.ErrOrStderr(), connections.DirectURL, files); err != nil {
		return err
	}
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{"imported": true, "plan": plan, "rls_bundle": written})
	}
	_, _ = fmt.Fprintf(progress, "Restore complete. Your app must now set the session context the policies read - see %s.\n", filepath.Join(options.rlsOut, "capyrls_report.md"))
	return nil
}

func writeSupabaseRestorePlan(out io.Writer, plan supabaseRestorePlan, bundle []string) {
	_, _ = fmt.Fprintf(out, "schemas: %s\n", strings.Join(plan.AppSchemas, ", "))
	_, _ = fmt.Fprintf(out, "dump entries restored: %d\n", plan.KeptCount)
	for _, dropped := range plan.Dropped {
		_, _ = fmt.Fprintf(out, "not restored: %s\n", dropped)
	}
	for _, fk := range plan.DroppedAuthFKs {
		_, _ = fmt.Fprintf(out, "not restored (points into a Supabase-managed schema): %s\n", fk)
	}
	if len(plan.DumpExtensions) > 0 {
		_, _ = fmt.Fprintf(out, "extensions used: %s\n", strings.Join(plan.DumpExtensions, ", "))
	}
	if plan.RewriteExtensions {
		_, _ = fmt.Fprintln(out, "rewrite: extensions.<name> -> public.<name> (the project keeps its extensions in public)")
	}
	for _, reference := range plan.ResidualReferences {
		_, _ = fmt.Fprintf(out, "warning: the schema still references %s, which does not exist here; the restore fails if a view or default needs it\n", reference)
	}
	_, _ = fmt.Fprintf(out, "policy bundle and report: %s\n", filepath.Dir(bundle[0]))
}
