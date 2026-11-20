// Package configlint statically inspects a project's database configuration
// and reports the CapyDB-specific misconfigurations that otherwise surface as
// runtime failures hours later.
//
// Why a linter and not a runtime wrapper: of the things unique to CapyDB, only
// cold-start wake latency is a runtime concern. Everything else - which URL you
// use for migrations, prepared statements through the transaction pooler,
// client pool sizing - is CONFIGURATION. Configuration can be read from disk,
// which means one linter covers every stack (Drizzle, Prisma, Rails, Django,
// SQLAlchemy, raw postgres.js) instead of a per-language wrapper covering one.
// It also cannot break anyone's runtime, because it never runs their code.
//
// The linter is read-only and never dials the network.
package configlint

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/capydatabase/capydb-cli/internal/scan"
)

// Severity separates "this will bite you" from "this is worth knowing".
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is one configuration problem, anchored to the file that causes it.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"`
}

// skipDirs keeps the walk off vendored trees; mirrors the scanner's exclusions.
var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, "build": true, ".next": true,
	"vendor": true, "__pycache__": true, ".venv": true, "venv": true, "target": true,
	".turbo": true, "coverage": true, ".svelte-kit": true,
}

var (
	envAssignPattern = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*["']?([^"'\r\n#]+)`)
	envRefPattern    = regexp.MustCompile(`(?:process\.env\.([A-Za-z_][A-Za-z0-9_]*)|process\.env\[["']([A-Za-z_][A-Za-z0-9_]*)["']\]|env\(["']([A-Za-z_][A-Za-z0-9_]*)["']\)|os\.environ(?:\.get)?[\[(]["']([A-Za-z_][A-Za-z0-9_]*)["'])`)
	postgresLiteral  = regexp.MustCompile(`postgres(?:ql)?://[^\s"'` + "`" + `]+`)
	maxOptionPattern = regexp.MustCompile(`\bmax\s*:\s*(\d+)`)
	poolSizePattern  = regexp.MustCompile(`\b(?:pool_size|POOL_SIZE|pool)\s*[:=]\s*(\d+)`)
)

// pooledPoolCeiling is the largest per-client pool that still makes sense
// against a transaction pooler. The pooled endpoint exists to multiplex many
// small client pools onto few server connections; a large client-side max just
// pins pooler slots.
const pooledPoolCeiling = 10

// Run inspects the project rooted at root and returns findings sorted by file.
func Run(root string) ([]Finding, error) {
	env, err := readEnvURLs(root)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, "drizzle.config."):
			findings = append(findings, lintDrizzleConfig(path, rel, env)...)
		case name == "package.json":
			findings = append(findings, lintPackageScripts(path, rel)...)
		case name == "schema.prisma":
			findings = append(findings, lintPrismaSchema(path, rel, env)...)
		case name == "database.yml":
			findings = append(findings, lintRailsDatabase(path, rel)...)
		case isJSLike(name):
			findings = append(findings, lintJSSource(path, rel, env)...)
		case strings.HasSuffix(name, ".py"):
			findings = append(findings, lintPythonSource(path, rel, env)...)
		case strings.HasSuffix(name, ".sql"):
			findings = append(findings, lintSQLSource(path, rel)...)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// envURL records whether a given env key resolves to a pooled endpoint, based
// on the .env* files in the repo. Config files reference keys, not URLs, so the
// linter has to resolve one to the other before it can judge anything.
type envURL struct {
	pooled bool
	known  bool
}

func readEnvURLs(root string) (map[string]envURL, error) {
	result := map[string]envURL{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if name != ".env" && !strings.HasPrefix(name, ".env.") {
			return nil
		}
		// Example files carry placeholder hosts, not real definitions.
		if strings.Contains(name, "example") || strings.Contains(name, "sample") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			match := envAssignPattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			value := strings.TrimSpace(match[2])
			if !strings.HasPrefix(value, "postgres") {
				continue
			}
			// Last definition wins is not knowable statically; treat a key as
			// pooled if ANY env file points it at a pooler, since that is the
			// configuration that can bite.
			existing := result[match[1]]
			result[match[1]] = envURL{pooled: existing.pooled || scan.IsPooledURL(value), known: true}
		}
		return nil
	})
	return result, err
}

// urlIsPooled decides whether a config value - an env reference or a literal
// URL - points at the pooled endpoint. ok is false when it cannot be resolved.
func urlIsPooled(value string, env map[string]envURL) (pooled bool, ok bool) {
	if literal := postgresLiteral.FindString(value); literal != "" {
		return scan.IsPooledURL(literal), true
	}
	for _, match := range envRefPattern.FindAllStringSubmatch(value, -1) {
		for _, key := range match[1:] {
			if key == "" {
				continue
			}
			if resolved, found := env[key]; found && resolved.known {
				return resolved.pooled, true
			}
			// A DIRECT-shaped name is a strong signal even without a .env file.
			if strings.Contains(strings.ToUpper(key), "DIRECT") {
				return false, true
			}
		}
	}
	return false, false
}

func lintDrizzleConfig(path, rel string, env map[string]envURL) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	findings := []Finding{}

	for i, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "url") {
			continue
		}
		pooled, ok := urlIsPooled(line, env)
		if ok && pooled {
			findings = append(findings, Finding{
				Rule: "pooled_url_for_migrations", Severity: SeverityError,
				File: rel, Line: i + 1,
				Message: "drizzle-kit is pointed at the pooled endpoint; migrations need session state and advisory locks that transaction pooling cannot provide",
				Fix:     "use the direct URL: url: process.env.DATABASE_DIRECT_URL ?? process.env.DATABASE_URL!",
			})
		}
	}

	// drizzle-kit v1 manages ALL schemas by default, so without a filter it
	// will offer extension-owned schemas (and anything else it did not create)
	// for DROP on the next push.
	if !strings.Contains(body, "schemaFilter") {
		findings = append(findings, Finding{
			Rule: "missing_schema_filter", Severity: SeverityWarning,
			File:    rel,
			Message: "no schemaFilter: drizzle-kit v1 manages every schema by default, so push can offer schemas you do not own for DROP",
			Fix:     `add schemaFilter: ["public"]`,
		})
	}
	return findings
}

// lintPackageScripts flags the drizzle push/migrate mix.
//
// `drizzle-kit push` applies the schema directly and records NOTHING in the
// migrations table. If the live database was built that way and the repo also
// carries generated migrations, the first `drizzle-kit migrate` replays from
// migration #1 against a database that already has those objects and dies on
// `relation "..." already exists`. The database is fine; the migration history
// simply never knew about it.
//
// The fix is to baseline: mark the existing migrations as already applied
// (drizzle-kit's --init / a one-off insert into the migrations table) so
// `migrate` starts from the current state instead of from zero.
func lintPackageScripts(path, rel string) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil || len(pkg.Scripts) == 0 {
		return nil
	}
	hasPush, hasMigrate := false, false
	for _, cmd := range pkg.Scripts {
		lower := strings.ToLower(cmd)
		if !strings.Contains(lower, "drizzle-kit") {
			continue
		}
		if strings.Contains(lower, "push") {
			hasPush = true
		}
		if strings.Contains(lower, "migrate") {
			hasMigrate = true
		}
	}
	if !hasPush || !hasMigrate {
		return nil
	}
	return []Finding{{
		Rule: "drizzle_push_and_migrate_mixed", Severity: SeverityWarning,
		File:    rel,
		Message: "this project has both a drizzle-kit push and a drizzle-kit migrate script; push does not record anything in the migrations table, so if the live database was built with push, the first migrate replays from migration #1 and fails with \"already exists\"",
		Fix:     "pick one per environment, and baseline before the first migrate: mark existing migrations as applied so migrate starts from the current schema instead of from zero",
	}}
}

func lintPrismaSchema(path, rel string, env map[string]envURL) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	findings := []Finding{}

	urlPooled := false
	urlLine := 0
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "url") {
			continue
		}
		if pooled, ok := urlIsPooled(line, env); ok && pooled {
			urlPooled = true
			urlLine = i + 1
		}
	}
	if !urlPooled {
		return findings
	}

	// Prisma runs migrations over directUrl; without it, `prisma migrate` goes
	// through the pooler and can hang or fail on advisory locks.
	if !strings.Contains(body, "directUrl") {
		findings = append(findings, Finding{
			Rule: "prisma_missing_direct_url", Severity: SeverityError,
			File: rel, Line: urlLine,
			Message: "datasource uses the pooled URL with no directUrl, so prisma migrate runs through the transaction pooler",
			Fix:     `add directUrl = env("DATABASE_DIRECT_URL") to the datasource block`,
		})
	}
	// Prisma uses named prepared statements, which transaction pooling does not
	// support; the flag makes Prisma skip them and its DEALLOCATE ALL assumption.
	if !strings.Contains(body, "pgbouncer=true") {
		findings = append(findings, Finding{
			Rule: "prisma_missing_pgbouncer_flag", Severity: SeverityWarning,
			File: rel, Line: urlLine,
			Message: "pooled URL without ?pgbouncer=true; Prisma's prepared-statement cache is not compatible with transaction pooling",
			Fix:     "append ?pgbouncer=true to the pooled connection string",
		})
	}
	return findings
}

func lintJSSource(path, rel string, env map[string]envURL) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	if !strings.Contains(body, "postgres(") && !strings.Contains(body, "new Pool(") {
		return checkBulkTransaction(body, rel)
	}
	findings := []Finding{}
	lines := strings.Split(body, "\n")

	for i, line := range lines {
		if !strings.Contains(line, "postgres(") {
			continue
		}
		pooled, ok := urlIsPooled(line, env)
		if !ok || !pooled {
			continue
		}
		// postgres.js caches named prepared statements by default, which the
		// transaction pooler cannot route. Look at a small window because the
		// options object is usually on following lines.
		window := strings.Join(lines[i:min(i+6, len(lines))], "\n")
		if !strings.Contains(window, "prepare") {
			findings = append(findings, Finding{
				Rule: "missing_prepare_false", Severity: SeverityError,
				File: rel, Line: i + 1,
				Message: "postgres.js on the pooled endpoint without prepare: false; every query will eventually fail with 'prepared statement already exists'",
				Fix:     "postgres(url, { prepare: false, max: 1 }) - or use createDb() from @capydb/drizzle, which applies this for you",
			})
		}
		if match := maxOptionPattern.FindStringSubmatch(window); match != nil {
			if size, convErr := strconv.Atoi(match[1]); convErr == nil && size > pooledPoolCeiling {
				findings = append(findings, Finding{
					Rule: "oversized_pool", Severity: SeverityWarning,
					File: rel, Line: i + 1,
					Message: "client pool of " + match[1] + " against the pooled endpoint; the pooler exists to multiplex small client pools, and a large max just pins its slots",
					Fix:     "max: 1 in serverless, a small number in a long-lived server",
				})
			}
		}
	}
	return append(findings, checkBulkTransaction(body, rel)...)
}

func lintPythonSource(path, rel string, env map[string]envURL) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	findings := []Finding{}
	for i, line := range strings.Split(body, "\n") {
		match := poolSizePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pooled, ok := urlIsPooled(body, env)
		if !ok || !pooled {
			continue
		}
		if size, convErr := strconv.Atoi(match[1]); convErr == nil && size > pooledPoolCeiling {
			findings = append(findings, Finding{
				Rule: "oversized_pool", Severity: SeverityWarning,
				File: rel, Line: i + 1,
				Message: "pool_size of " + match[1] + " against the pooled endpoint",
				Fix:     "use a small pool, or poolclass=NullPool in serverless so the server-side pooler does the pooling",
			})
		}
	}
	return append(findings, checkBulkTransaction(body, rel)...)
}

func lintRailsDatabase(path, rel string) []Finding {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	findings := []Finding{}
	body := string(data)
	if !strings.Contains(body, "prepared_statements") && strings.Contains(body, "6432") {
		findings = append(findings, Finding{
			Rule: "missing_prepare_false", Severity: SeverityError,
			File:    rel,
			Message: "Rails against the pooled endpoint without prepared_statements: false",
			Fix:     "set prepared_statements: false on the pooled configuration; run migrations on the direct URL",
		})
	}
	for i, line := range strings.Split(body, "\n") {
		match := poolSizePattern.FindStringSubmatch(line)
		if match == nil || !strings.Contains(body, "6432") {
			continue
		}
		if size, convErr := strconv.Atoi(match[1]); convErr == nil && size > pooledPoolCeiling {
			findings = append(findings, Finding{
				Rule: "oversized_pool", Severity: SeverityWarning,
				File: rel, Line: i + 1,
				Message: "Rails pool of " + match[1] + " against the pooled endpoint",
				Fix:     "keep the client pool small; the pooled endpoint is already doing the multiplexing",
			})
		}
	}
	return findings
}

// checkBulkTransaction flags a long-running write loop wrapped in a single
// transaction. Plans cap idle_in_transaction_session_timeout (60-120s), so a
// script that opens one transaction and then does per-row client work gets its
// session cut partway through - the failure mode is a half-finished migration.
func checkBulkTransaction(body, rel string) []Finding {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		opensTx := strings.Contains(lower, ".begin(") ||
			strings.Contains(lower, "begin transaction") ||
			strings.Contains(lower, "begin;")
		if !opensTx {
			continue
		}
		// A loop containing a write, inside the transaction body.
		window := strings.Join(lines[i:min(i+40, len(lines))], "\n")
		hasLoop := strings.Contains(window, "for ") || strings.Contains(window, "while ") ||
			strings.Contains(window, ".map(") || strings.Contains(window, "forEach")
		hasWrite := strings.Contains(strings.ToUpper(window), "INSERT ") ||
			strings.Contains(strings.ToUpper(window), "UPDATE ") ||
			strings.Contains(strings.ToUpper(window), "COPY ")
		if hasLoop && hasWrite {
			return []Finding{{
				Rule: "unbatched_bulk_transaction", Severity: SeverityWarning,
				File: rel, Line: i + 1,
				Message: "a write loop inside a single transaction; plans cap idle_in_transaction_session_timeout at 60-120s, so a large run can be cut partway through",
				Fix:     "commit in batches (one transaction per batch) with keyset pagination so the script is restartable",
			}}
		}
	}
	return nil
}

func isJSLike(name string) bool {
	for _, suffix := range []string{".ts", ".mts", ".cts", ".js", ".mjs", ".cjs", ".tsx"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// supabaseAuthHelperPattern matches the Supabase auth helpers inside SQL
// (policy expressions, column defaults, function bodies).
var supabaseAuthHelperPattern = regexp.MustCompile(`\bauth\.(uid|jwt|role|email)\s*\(`)

// rawUUIDv7Pattern matches the two major-specific spellings of a time-sortable
// UUID generator: the built-in uuidv7() of Postgres 18 and newer and the
// pg_uuidv7 extension's uuid_generate_v7(), which is what 16/17 have. A capydb-qualified
// call is excluded by the negative lookbehind stand-in below (Go's regexp has
// no lookbehind, so the schema prefix is captured and checked by the caller).
var rawUUIDv7Pattern = regexp.MustCompile(`(?i)([a-z0-9_]+\s*\.\s*)?\b(uuidv7|uuid_generate_v7)\s*\(`)

// notNullAlterPattern matches ALTER TABLE ... SET NOT NULL without a following
// NOT VALID on the same statement.
var notNullAlterPattern = regexp.MustCompile(`(?is)alter\s+table\s+[^;]*?\bset\s+not\s+null\b[^;]*;?`)

// lintSQLSource runs every SQL-level rule over one file.
func lintSQLSource(path, rel string) []Finding {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	findings := lintSupabaseAuthHelpers(content, rel)
	findings = append(findings, lintRawUUIDv7(content, rel)...)
	return append(findings, lintUnguardedSetNotNull(content, rel)...)
}

// lintSupabaseAuthHelpers flags SQL that still calls Supabase's auth helpers.
// Those functions do not exist outside Supabase: applying such a migration to a
// CapyDB cell aborts the restore, and a policy that survives never matches a
// row. The converter exists precisely for this.
func lintSupabaseAuthHelpers(content []byte, rel string) []Finding {
	loc := supabaseAuthHelperPattern.FindIndex(content)
	if loc == nil {
		return nil
	}
	return []Finding{{
		Rule: "supabase_rls_unconverted", Severity: SeverityWarning,
		File: rel, Line: lineOf(content, loc[0]),
		Message: "SQL calls Supabase auth helpers (auth.uid()/auth.jwt()); they do not exist outside Supabase, so this aborts an import or the policy never matches",
		Fix:     "run `capydb migrate rls` to convert the policies to portable, vanilla Postgres",
	}}
}

// lintRawUUIDv7 flags a schema pinned to one major's spelling of uuidv7.
//
// Postgres 18 and newer ship uuidv7() in pg_catalog; 16 and 17 only get it from the
// pg_uuidv7 extension, which names it uuid_generate_v7(). Either raw name in a
// column DEFAULT is a schema that breaks on the other major - and a CapyDB
// major upgrade is a logical dump and restore, so the qualified name travels
// with the schema and fails on arrival. capydb.uuidv7() resolves to whatever
// the local major provides and is stable across the upgrade.
func lintRawUUIDv7(content []byte, rel string) []Finding {
	for _, match := range rawUUIDv7Pattern.FindAllSubmatchIndex(content, -1) {
		// Group 1 is the optional schema qualifier; a capydb-qualified call is
		// the fix, not the problem.
		if match[2] >= 0 {
			qualifier := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(
				strings.TrimSpace(string(content[match[2]:match[3]])), ".")))
			if qualifier == "capydb" {
				continue
			}
		}
		return []Finding{{
			Rule: "uuidv7_not_portable", Severity: SeverityWarning,
			File: rel, Line: lineOf(content, match[0]),
			Message: "SQL calls uuidv7()/uuid_generate_v7() directly; the built-in exists only on Postgres 18 and newer and the extension spelling only on 16/17, so this schema breaks on the other major",
			Fix:     "call capydb.uuidv7() instead - it resolves to the built-in on 18 and newer and to an equivalent on 16/17",
		}}
	}
	return nil
}

// lintUnguardedSetNotNull flags SET NOT NULL without NOT VALID.
//
// Adding NOT NULL to a populated table takes ACCESS EXCLUSIVE and scans every
// row before it returns; on a live table that is a write outage for the
// duration. Postgres 18 and newer allow the constraint to be added NOT VALID and
// validated afterwards under a weaker lock, which turns the outage into two
// cheap steps.
func lintUnguardedSetNotNull(content []byte, rel string) []Finding {
	for _, loc := range notNullAlterPattern.FindAllIndex(content, -1) {
		statement := strings.ToLower(string(content[loc[0]:loc[1]]))
		if strings.Contains(statement, "not valid") {
			continue
		}
		return []Finding{{
			Rule: "set_not_null_locks_table", Severity: SeverityWarning,
			File: rel, Line: lineOf(content, loc[0]),
			Message: "ALTER TABLE ... SET NOT NULL takes ACCESS EXCLUSIVE and scans the whole table, blocking writes until it finishes",
			Fix:     "on Postgres 18 and newer, add the constraint NOT VALID first and run ALTER TABLE ... VALIDATE CONSTRAINT afterwards, which does not block writes",
		}}
	}
	return nil
}

// lineOf converts a byte offset into a 1-based line number.
func lineOf(content []byte, offset int) int {
	return 1 + strings.Count(string(content[:offset]), "\n")
}
