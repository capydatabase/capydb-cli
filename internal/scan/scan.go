// Package scan implements `capydb migrate scan`: a read-only repository scan
// that classifies an app across the three migration axes (source database
// provider x auth system x data-access layer), runs the migration gates
// (consumers, hygiene, coupling), and emits a per-database migration plan.
// Design and scenario vocabulary: docs/capydb-migration-scenarios-spec.md in
// the capydb repo. The repo scan never talks to a network or mutates anything;
// the optional live-source probes (`--source-url`, source.go) connect
// read-only to the OLD database and nothing else.
package scan

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Report is the scan's full result. Databases is the primary output: the unit
// of cutover is the database, not the repo.
type Report struct {
	Path         string        `json:"path"`
	Databases    []Database    `json:"databases"`
	EnvConflicts []EnvConflict `json:"env_conflicts"`
	Repo         RepoFacts     `json:"repo"`
	// Source holds the live-database probe results when the scan ran with
	// --source-url; nil for a repo-only scan.
	Source   *SourceFacts `json:"source,omitempty"`
	Scenario Scenario     `json:"scenario"`
}

// EnvAssignment is one env file's database value for a single env key.
type EnvAssignment struct {
	File     string `json:"file"`
	Hostname string `json:"hostname"`
	Provider string `json:"provider"`
}

// EnvConflict is one env key that points at DIFFERENT databases in different
// env files.
//
// This is the shadowing failure, and it is silent by construction: the
// framework resolves one file (Next: .env.local over .env) while any tool that
// pins a path - `dotenv.config({ path: ".env" })`, `load_dotenv(".env")` -
// resolves another. The app can therefore run against the new database while
// migrations, seeds and ingest scripts still write to the old one, with nothing
// reporting an error. Observed on press-hub 2026-07-23.
type EnvConflict struct {
	Key         string          `json:"key"`
	Assignments []EnvAssignment `json:"assignments"`
}

// EnvLoader is a source file that loads one specific env file by path, thereby
// opting out of the framework's env precedence.
type EnvLoader struct {
	File       string `json:"file"`
	PinnedPath string `json:"pinned_path"`
}

// Database is one distinct database hostname the repo references.
type Database struct {
	Hostname string `json:"hostname"`
	// Provider is one of the Provider* identifiers in provider.go, classified
	// from the hostname. A live scan overrides it with what the server says.
	Provider  string   `json:"provider"`
	Pooled    bool     `json:"pooled"` // provider pooler endpoint (must not be used for dumps)
	EnvKeys   []string `json:"env_keys"`
	EnvFiles  []string `json:"env_files"`
	Consumers []string `json:"consumers,omitempty"` // other portfolio repos referencing this hostname
}

// RepoFacts are the axis-B/C classification inputs plus the hygiene gate.
type RepoFacts struct {
	AuthSystems []string    `json:"auth_systems"`
	DataLayers  []string    `json:"data_layers"`
	CallSites   CallSites   `json:"call_sites"`
	DistTagPins []string    `json:"dist_tag_pins"` // "pkg@tag" - moving pointers; lockfile regen imports drift
	EnvFiles    []string    `json:"env_files"`
	EnvLoaders  []EnvLoader `json:"env_loaders"` // source files pinning a specific env path

	// DrizzleSchemaFilter reports whether a drizzle config already scopes
	// drizzle-kit to specific schemas.
	DrizzleSchemaFilter bool `json:"drizzle_schema_filter"`

	Lockfiles      []string `json:"lockfiles"`
	PackageDirs    []string `json:"package_dirs"`
	SupabaseAssets Supabase `json:"supabase_assets"`

	// RPCNames are the distinct database function names the code calls via
	// .rpc(). Their SOURCE must exist somewhere: an RPC with no CREATE
	// FUNCTION in the repo lives only in the provider's database and must be
	// recovered before cutover (roomie-radar lesson, 2026-09-01).
	RPCNames []string `json:"rpc_names"`
	// LocalSQLFunctions are function names defined in the repo's SQL files.
	LocalSQLFunctions []string `json:"-"`
	// RPCsWithoutLocalSource are RPCNames with no local CREATE FUNCTION.
	RPCsWithoutLocalSource []string `json:"rpcs_without_local_source"`
}

// CallSites counts provider-coupled call sites per KIND - dependency presence
// alone misclassifies (e.g. supabase-js installed only for storage).
type CallSites struct {
	SupabaseData     int      `json:"supabase_data"`     // supabase.from(...)
	SupabaseAuth     int      `json:"supabase_auth"`     // *.auth.getUser / signIn / onAuthStateChange...
	SupabaseStorage  int      `json:"supabase_storage"`  // storage.from(...)
	SupabaseRealtime int      `json:"supabase_realtime"` // .channel( / postgres_changes
	NeonDriverFiles  []string `json:"neon_driver_files"` // files importing @neondatabase/serverless or drizzle-orm/neon-*
	NeonBatchCalls   int      `json:"neon_batch_calls"`  // db.batch( / sql.transaction([ - need transaction rewrites

	// AnonKeyClientFiles counts files constructing supabase clients on the
	// anon/publishable key. When that happens in SERVER code, authorization is
	// delegated to RLS - queries deliberately omit ownership filters - so the
	// RLS corpus is load-bearing far beyond the browser (myroomiev3 lesson:
	// 224 server files on the anon key).
	AnonKeyClientFiles int `json:"anon_key_client_files"`
	// ServiceRoleKeyFiles counts files referencing the service-role secret -
	// those clients bypass RLS.
	ServiceRoleKeyFiles int `json:"service_role_key_files"`
}

// Supabase inventories provider-project assets in the repo.
type Supabase struct {
	MigrationFiles int  `json:"migration_files"`
	EdgeFunctions  int  `json:"edge_functions"`
	ConfigPresent  bool `json:"config_present"`
}

// Scenario is the derived plan.
type Scenario struct {
	Name     string   `json:"name"`
	Effort   string   `json:"effort"` // S | M | L | XL
	Plan     []string `json:"plan"`
	Warnings []string `json:"warnings"`
}

var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, ".next": true, ".turbo": true,
	"build": true, "vendor": true, "target": true, "__pycache__": true, ".venv": true,
	"venv": true, "coverage": true, "Pods": true, ".vercel": true, "out": true,
	".output": true, ".cache": true, "DerivedData": true,
}

var sourceExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".mts": true,
	".cjs": true, ".svelte": true, ".vue": true, ".py": true, ".go": true,
}

const maxSourceFileBytes = 1 << 20

var (
	postgresURLPattern = regexp.MustCompile(`postgres(?:ql)?(?:\+[a-z]+)?://[^@\s"']+@([a-zA-Z0-9.-]+)(?::(\d+))?/`)
	supabaseRefPattern = regexp.MustCompile(`https://([a-z0-9]+)\.supabase\.co`)
	envKeyPattern      = regexp.MustCompile(`(?m)^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)
	distTagPattern     = regexp.MustCompile(`^(alpha|beta|rc|canary|next|latest|experimental|nightly|dev)$`)

	supabaseDataPattern     = regexp.MustCompile(`\bsupabase(?:Admin|Client|Server)?\s*\.\s*from\s*\(|\.schema\s*\(\s*['"][^'"]+['"]\s*\)\s*\.\s*from\s*\(`)
	supabaseAuthPattern     = regexp.MustCompile(`\.auth\s*\.\s*(getUser|getSession|signIn|signUp|signOut|onAuthStateChange|admin|exchangeCodeForSession|refreshSession|setSession|getClaims)\b`)
	supabaseStoragePattern  = regexp.MustCompile(`\.storage\s*\.\s*from\s*\(`)
	supabaseRealtimePattern = regexp.MustCompile(`\.channel\s*\(|postgres_changes`)
	neonImportPattern       = regexp.MustCompile(`@neondatabase/serverless|drizzle-orm/neon-`)
	neonBatchPattern        = regexp.MustCompile(`\bdb\s*\.\s*batch\s*\(|\bsql\s*\.\s*transaction\s*\(\s*\[`)

	rpcCallPattern        = regexp.MustCompile(`\.rpc\(\s*['"]([A-Za-z0-9_]+)['"]`)
	sqlFunctionPattern    = regexp.MustCompile(`(?i)create\s+(?:or\s+replace\s+)?function\s+(?:"?[a-z0-9_]+"?\.)?"?([a-z0-9_]+)"?`)
	supabaseClientPattern = regexp.MustCompile(`createClient\s*[<(]|createServerClient\s*\(|createBrowserClient\s*\(`)
	anonKeyPattern        = regexp.MustCompile(`SUPABASE_(?:ANON_KEY|PUBLISHABLE)`)
	serviceRolePattern    = regexp.MustCompile(`SUPABASE_(?:SERVICE_ROLE_KEY|SECRET_KEY)`)

	// Loaders that pin ONE env file, bypassing the framework's precedence:
	// dotenv `config({ path: "..." })`, python-dotenv `load_dotenv("...")`,
	// godotenv `Load("...")`.
	dotenvPathPattern   = regexp.MustCompile(`config\s*\(\s*\{[^}]*?path\s*:\s*['"` + "`" + `]([^'"` + "`" + `]+)['"` + "`" + `]`)
	pyDotenvPathPattern = regexp.MustCompile(`load_dotenv\s*\(\s*(?:dotenv_path\s*=\s*)?['"]([^'"]+)['"]`)
	goDotenvPathPattern = regexp.MustCompile(`godotenv\.(?:Load|Overload)\s*\(\s*"([^"]+)"`)
)

// isExampleEnvFile reports whether an env file holds placeholders rather than
// real values. Their values are deliberately fake (`SLUG.db.capydb.dev`) and
// must never count as a conflicting definition.
func isExampleEnvFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	for _, marker := range []string{"example", "sample", "template", "dist", "defaults"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return false
}

// interestingDeps maps dependency names to the classification they signal.
var authDeps = map[string]string{
	"@clerk/nextjs": "clerk", "@clerk/clerk-react": "clerk", "@clerk/backend": "clerk",
	"@clerk/clerk-sdk-node": "clerk", "@clerk/express": "clerk",
	"better-auth": "better-auth",
	"next-auth":   "authjs", "@auth/core": "authjs",
	"lucia": "lucia",
}

var dataLayerDeps = map[string]string{
	"@neondatabase/serverless": "neon-driver",
	"@vercel/postgres":         "neon-driver",
	"postgres":                 "postgres-js",
	"pg":                       "node-postgres",
	"@prisma/client":           "prisma",
	"drizzle-orm":              "drizzle",
	"kysely":                   "kysely",
	"@supabase/supabase-js":    "supabase-js",
	"@supabase/ssr":            "supabase-js",
}

// Run scans root and, when portfolioDir is non-empty, greps sibling repos'
// env files for the same database hostnames (the consumers gate).
func Run(root, portfolioDir string) (Report, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Report{}, err
	}
	report := Report{Path: root}

	databasesByHost := map[string]*Database{}
	supabaseRefs := map[string]bool{}
	envAssignments := map[string][]EnvAssignment{}

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}

		switch {
		case name == ".env" || strings.HasPrefix(name, ".env."):
			report.Repo.EnvFiles = append(report.Repo.EnvFiles, relative)
			scanEnvFile(path, relative, databasesByHost, supabaseRefs)
			collectEnvAssignments(path, relative, envAssignments)
		case name == "package.json":
			report.Repo.PackageDirs = append(report.Repo.PackageDirs, filepath.Dir(relative))
			scanPackageJSON(path, &report.Repo)
		case name == "pnpm-lock.yaml" || name == "package-lock.json" || name == "yarn.lock" || name == "bun.lockb" || name == "go.sum" || name == "uv.lock" || name == "poetry.lock":
			report.Repo.Lockfiles = append(report.Repo.Lockfiles, relative)
		case name == "go.mod":
			scanGoMod(path, &report.Repo)
		case name == "pyproject.toml" || name == "requirements.txt":
			scanPythonDeps(path, &report.Repo)
		}

		if sourceExtensions[filepath.Ext(name)] {
			scanSourceFile(path, relative, &report.Repo)
		}
		if strings.HasSuffix(name, ".sql") {
			scanSQLFile(path, &report.Repo)
		}
		if strings.Contains(relative, "supabase"+string(filepath.Separator)+"migrations") && strings.HasSuffix(name, ".sql") {
			report.Repo.SupabaseAssets.MigrationFiles++
		}
		if name == "config.toml" && strings.Contains(relative, "supabase") {
			report.Repo.SupabaseAssets.ConfigPresent = true
		}
		return nil
	})
	if walkErr != nil {
		return Report{}, walkErr
	}

	// Edge functions: one directory per function under supabase/functions.
	if entries, err := os.ReadDir(filepath.Join(root, "supabase", "functions")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), "_") {
				report.Repo.SupabaseAssets.EdgeFunctions++
			}
		}
	}

	// Supabase project refs referenced only by NEXT_PUBLIC_SUPABASE_URL still
	// mean a Supabase database exists even without a postgres:// URL in env.
	for ref := range supabaseRefs {
		host := "db." + ref + ".supabase.co"
		alreadySeen := false
		for existing := range databasesByHost {
			if strings.Contains(existing, ref) || strings.Contains(existing, "pooler.supabase.com") {
				alreadySeen = true
			}
		}
		if !alreadySeen {
			databasesByHost[host] = &Database{Hostname: host, Provider: "supabase", EnvKeys: []string{"NEXT_PUBLIC_SUPABASE_URL"}}
		}
	}

	for _, database := range databasesByHost {
		sort.Strings(database.EnvKeys)
		database.EnvKeys = dedupe(database.EnvKeys)
		sort.Strings(database.EnvFiles)
		database.EnvFiles = dedupe(database.EnvFiles)
		if portfolioDir != "" && database.Provider != "local" {
			database.Consumers = findConsumers(portfolioDir, root, consumerSearchToken(database.Hostname))
		}
		report.Databases = append(report.Databases, *database)
	}
	sort.Slice(report.Databases, func(i, j int) bool { return report.Databases[i].Hostname < report.Databases[j].Hostname })

	report.EnvConflicts = conflictsFrom(envAssignments)

	normalizeRepoFacts(&report.Repo)
	report.Scenario = deriveScenario(report)
	return report, nil
}

// DetectEnvConflicts walks only the repo's env files and returns every env key
// that resolves to more than one database. It is deliberately cheap (no source
// parsing, no network) so `link`, `create` and `doctor` can all afford to call
// it inline.
func DetectEnvConflicts(root string) ([]EnvConflict, error) {
	assignments := map[string][]EnvAssignment{}
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
		name := entry.Name()
		if name != ".env" && !strings.HasPrefix(name, ".env.") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		collectEnvAssignments(path, relative, assignments)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return conflictsFrom(assignments), nil
}

// collectEnvAssignments records key -> (file, host) for one env file. Example
// files are skipped: their placeholder hosts are not real definitions.
func collectEnvAssignments(path, relative string, assignments map[string][]EnvAssignment) {
	if isExampleEnvFile(relative) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyMatch := envKeyPattern.FindStringSubmatch(line)
		urlMatch := postgresURLPattern.FindStringSubmatch(trimmed)
		if keyMatch == nil || urlMatch == nil {
			continue
		}
		key, host := keyMatch[1], urlMatch[1]
		assignments[key] = append(assignments[key], EnvAssignment{
			File: relative, Hostname: host, Provider: classifyHost(host),
		})
	}
}

// conflictsFrom keeps only keys whose assignments disagree on the hostname.
func conflictsFrom(assignments map[string][]EnvAssignment) []EnvConflict {
	conflicts := []EnvConflict{}
	for key, entries := range assignments {
		hosts := map[string]bool{}
		for _, entry := range entries {
			hosts[entry.Hostname] = true
		}
		if len(hosts) < 2 {
			continue
		}
		sorted := append([]EnvAssignment(nil), entries...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].File < sorted[j].File })
		conflicts = append(conflicts, EnvConflict{Key: key, Assignments: sorted})
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Key < conflicts[j].Key })
	return conflicts
}

// pinnedLoadersFor returns "file (pins <env file>)" for every loader that pins
// one of the env files taking part in a conflict - i.e. the tools that will
// silently resolve the losing value.
func pinnedLoadersFor(loaders []EnvLoader, conflict EnvConflict) []string {
	involved := map[string]bool{}
	for _, assignment := range conflict.Assignments {
		involved[filepath.Base(assignment.File)] = true
	}
	pins := []string{}
	for _, loader := range loaders {
		if involved[filepath.Base(loader.PinnedPath)] {
			pins = append(pins, fmt.Sprintf("%s (pins %s)", loader.File, loader.PinnedPath))
		}
	}
	sort.Strings(pins)
	return dedupe(pins)
}

// Describe renders a conflict as a single actionable line.
func (c EnvConflict) Describe() string {
	parts := make([]string, 0, len(c.Assignments))
	for _, assignment := range c.Assignments {
		parts = append(parts, fmt.Sprintf("%s -> %s", assignment.File, assignment.Hostname))
	}
	return fmt.Sprintf("%s is defined in %d env files with different databases (%s)",
		c.Key, len(c.Assignments), strings.Join(parts, ", "))
}

func scanEnvFile(path, relative string, databases map[string]*Database, supabaseRefs map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key := ""
		if match := envKeyPattern.FindStringSubmatch(line); match != nil {
			key = match[1]
		}
		if match := postgresURLPattern.FindStringSubmatch(trimmed); match != nil {
			host, port := match[1], match[2]
			database, ok := databases[host]
			if !ok {
				database = &Database{Hostname: host, Provider: classifyHost(host)}
				databases[host] = database
			}
			if key != "" {
				database.EnvKeys = append(database.EnvKeys, key)
			}
			database.EnvFiles = append(database.EnvFiles, relative)
			if isPooledEndpoint(host, port) {
				database.Pooled = true
			}
		}
		if match := supabaseRefPattern.FindStringSubmatch(trimmed); match != nil {
			supabaseRefs[match[1]] = true
		}
	}
}

// IsPooledURL reports whether a postgres connection string points at a
// transaction pooler rather than the direct port. Exported so the config
// linter and the scanner agree on one definition of "pooled" - the rules that
// depend on it (no migrations, no prepared statements, small client pools) are
// only correct if that classification is shared.
func IsPooledURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return false
	}
	return isPooledEndpoint(parsed.Hostname(), parsed.Port())
}

func isPooledEndpoint(host, port string) bool {
	if strings.Contains(host, "-pooler.") || strings.Contains(host, "pooler.supabase.com") {
		// Supabase's session pooler on :5432 is dump-safe; the transaction
		// pooler on :6543 is not. Neon -pooler endpoints are never dump-safe.
		if strings.Contains(host, "pooler.supabase.com") && port == "5432" {
			return false
		}
		return true
	}
	return port == "6543" || port == "6432"
}

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func scanPackageJSON(path string, facts *RepoFacts) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var parsed packageJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return
	}
	all := map[string]string{}
	maps.Copy(all, parsed.Dependencies)
	maps.Copy(all, parsed.DevDependencies)
	for name, version := range all {
		if system, ok := authDeps[name]; ok {
			facts.AuthSystems = append(facts.AuthSystems, system)
		}
		if strings.HasPrefix(name, "@clerk/") {
			facts.AuthSystems = append(facts.AuthSystems, "clerk")
		}
		if layer, ok := dataLayerDeps[name]; ok {
			facts.DataLayers = append(facts.DataLayers, layer)
		}
		if distTagPattern.MatchString(strings.TrimSpace(version)) {
			facts.DistTagPins = append(facts.DistTagPins, name+"@"+version)
		}
	}
}

func scanGoMod(path string, facts *RepoFacts) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	if strings.Contains(content, "jackc/pgx") || strings.Contains(content, "lib/pq") {
		facts.DataLayers = append(facts.DataLayers, "go-pgx")
	}
}

func scanPythonDeps(path string, facts *RepoFacts) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	if strings.Contains(content, "asyncpg") || strings.Contains(content, "psycopg") {
		facts.DataLayers = append(facts.DataLayers, "python-postgres")
	}
	if strings.Contains(content, "sqlalchemy") || strings.Contains(content, "SQLAlchemy") {
		facts.DataLayers = append(facts.DataLayers, "sqlalchemy")
	}
	if strings.Contains(content, "supabase") {
		facts.DataLayers = append(facts.DataLayers, "supabase-py")
	}
}

func scanSourceFile(path, relative string, facts *RepoFacts) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSourceFileBytes {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)

	// Env loaders that pin a path opt out of the framework's precedence, which
	// is what makes a duplicate DATABASE_URL dangerous rather than merely
	// untidy. Record every pin so a conflict can name the tools it misroutes.
	for _, pattern := range []*regexp.Regexp{dotenvPathPattern, pyDotenvPathPattern, goDotenvPathPattern} {
		for _, match := range pattern.FindAllStringSubmatch(content, -1) {
			pinned := strings.TrimSpace(match[1])
			if pinned == "" || !strings.Contains(filepath.Base(pinned), ".env") {
				continue
			}
			facts.EnvLoaders = append(facts.EnvLoaders, EnvLoader{File: relative, PinnedPath: pinned})
		}
	}

	if strings.HasPrefix(filepath.Base(relative), "drizzle.config.") && strings.Contains(content, "schemaFilter") {
		facts.DrizzleSchemaFilter = true
	}

	sites := &facts.CallSites
	sites.SupabaseData += len(supabaseDataPattern.FindAllStringIndex(content, -1))
	sites.SupabaseAuth += len(supabaseAuthPattern.FindAllStringIndex(content, -1))
	sites.SupabaseStorage += len(supabaseStoragePattern.FindAllStringIndex(content, -1))
	sites.SupabaseRealtime += len(supabaseRealtimePattern.FindAllStringIndex(content, -1))
	sites.NeonBatchCalls += len(neonBatchPattern.FindAllStringIndex(content, -1))
	if neonImportPattern.MatchString(content) {
		sites.NeonDriverFiles = append(sites.NeonDriverFiles, relative)
	}

	for _, match := range rpcCallPattern.FindAllStringSubmatch(content, -1) {
		facts.RPCNames = append(facts.RPCNames, match[1])
	}
	if supabaseClientPattern.MatchString(content) && anonKeyPattern.MatchString(content) {
		sites.AnonKeyClientFiles++
	}
	if serviceRolePattern.MatchString(content) {
		sites.ServiceRoleKeyFiles++
	}
}

// scanSQLFile collects function names defined in the repo's SQL, so .rpc()
// call sites can be cross-checked against a local CREATE FUNCTION.
func scanSQLFile(path string, facts *RepoFacts) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSourceFileBytes {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, match := range sqlFunctionPattern.FindAllStringSubmatch(string(data), -1) {
		facts.LocalSQLFunctions = append(facts.LocalSQLFunctions, strings.ToLower(match[1]))
	}
}

// consumerSearchToken picks the string to grep sibling repos for. Supabase
// projects are referenced by ref in several URL shapes (db.<ref>.supabase.co,
// https://<ref>.supabase.co, postgres.<ref>@pooler...), so match on the bare
// project ref rather than one synthesized hostname.
func consumerSearchToken(hostname string) string {
	if before, ok := strings.CutSuffix(hostname, ".supabase.co"); ok {
		trimmed := before
		trimmed = strings.TrimPrefix(trimmed, "db.")
		if trimmed != "" && !strings.Contains(trimmed, ".") {
			return trimmed
		}
	}
	return hostname
}

// findConsumers greps sibling repos' env files for the token - the
// consumers gate. Shallow by design: env files up to 3 levels deep per repo.
func findConsumers(portfolioDir, selfRoot, token string) []string {
	entries, err := os.ReadDir(portfolioDir)
	if err != nil {
		return nil
	}
	var consumers []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		repoPath := filepath.Join(portfolioDir, entry.Name())
		if same, _ := filepath.Abs(repoPath); same == selfRoot {
			continue
		}
		if repoReferencesHost(repoPath, token, 0) {
			consumers = append(consumers, entry.Name())
		}
	}
	sort.Strings(consumers)
	return consumers
}

func repoReferencesHost(dir, token string, depth int) bool {
	if depth > 3 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				continue
			}
			if repoReferencesHost(filepath.Join(dir, name), token, depth+1) {
				return true
			}
			continue
		}
		if name == ".env" || strings.HasPrefix(name, ".env.") {
			if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil && strings.Contains(string(data), token) {
				return true
			}
		}
	}
	return false
}

func normalizeRepoFacts(facts *RepoFacts) {
	facts.AuthSystems = dedupe(facts.AuthSystems)
	facts.DataLayers = dedupe(facts.DataLayers)
	facts.DistTagPins = dedupe(facts.DistTagPins)
	sort.Strings(facts.AuthSystems)
	sort.Strings(facts.DataLayers)
	sort.Strings(facts.DistTagPins)
	sort.Strings(facts.EnvFiles)
	sort.Strings(facts.Lockfiles)
	sort.Strings(facts.PackageDirs)
	sort.Strings(facts.CallSites.NeonDriverFiles)
	if facts.AuthSystems == nil {
		facts.AuthSystems = []string{}
	}
	if facts.DataLayers == nil {
		facts.DataLayers = []string{}
	}
	if facts.DistTagPins == nil {
		facts.DistTagPins = []string{}
	}

	sort.Strings(facts.RPCNames)
	facts.RPCNames = dedupe(facts.RPCNames)
	sort.Strings(facts.LocalSQLFunctions)
	facts.LocalSQLFunctions = dedupe(facts.LocalSQLFunctions)
	facts.RPCsWithoutLocalSource = []string{}
	for _, name := range facts.RPCNames {
		if !has(facts.LocalSQLFunctions, strings.ToLower(name)) {
			facts.RPCsWithoutLocalSource = append(facts.RPCsWithoutLocalSource, name)
		}
	}
	if facts.RPCNames == nil {
		facts.RPCNames = []string{}
	}
}

// AttachSource merges the live-source probe results into the report and
// re-derives the scenario: several plan decisions (the RLS path above all)
// change once the database itself has been measured.
func (r *Report) AttachSource(facts *SourceFacts) {
	r.Source = facts
	r.Scenario = deriveScenario(*r)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func has(values []string, value string) bool {
	return slices.Contains(values, value)
}

// deriveScenario maps the classification onto the scenario playbooks from the
// migration-scenarios spec, with an effort grade calibrated on the 2026-07
// dogfood migrations.
func deriveScenario(report Report) Scenario {
	scenario := Scenario{Warnings: []string{}}
	sites := report.Repo.CallSites

	provider := "plain-postgres"
	for _, database := range report.Databases {
		switch database.Provider {
		case "supabase":
			provider = "supabase"
		case "neon":
			if provider != "supabase" {
				provider = "neon"
			}
		case "azure", "rds":
			if provider == "plain-postgres" {
				provider = database.Provider
			}
		}
	}

	clerk := has(report.Repo.AuthSystems, "clerk")
	dumpSafeAuth := has(report.Repo.AuthSystems, "better-auth") || has(report.Repo.AuthSystems, "authjs") || has(report.Repo.AuthSystems, "lucia")
	supabaseAuthCoupled := provider == "supabase" && sites.SupabaseAuth > 3 && !clerk
	supabaseDataCoupled := sites.SupabaseData > 5

	switch {
	case supabaseAuthCoupled:
		scenario.Name = "supabase + supabase-auth (auth migration first)"
		scenario.Effort = "XL"
		scenario.Plan = []string{
			fmt.Sprintf("HARD BLOCKER: ~%d supabase.auth call sites and no Clerk - Supabase Auth is the identity system.", sites.SupabaseAuth),
			"Migrate auth first (e.g. to Clerk); a DB-only move buys nothing and breaks login.",
			"Re-run this scan after the auth migration; the remaining work becomes the supabase+clerk scenario.",
			"Run `capydb import preflight` now anyway: it reports FKs into auth.users and auth-bound RLS, which size the auth migration.",
		}
	case provider == "supabase" && supabaseDataCoupled:
		scenario.Name = "supabase + " + authLabel(clerk, dumpSafeAuth) + " + supabase-js data layer (rewrite, then import)"
		scenario.Effort = "L"
		if sites.SupabaseData <= 40 {
			scenario.Effort = "M"
		}
		scenario.Plan = []string{
			fmt.Sprintf("Rewrite ~%d supabase.from() call sites to drizzle/postgres-js AGAINST THE CURRENT SUPABASE DB first (it is plain Postgres) - this decouples the code rewrite from the infra cutover.", sites.SupabaseData),
			rlsPathStep(report),
			serviceReplacementStep(sites),
			"Then: create project, preflight, import (dump public + app schemas via the DIRECT endpoint), swap DATABASE_URL, redeploy every consumer at once.",
		}
	case provider == "supabase":
		scenario.Name = "supabase + " + authLabel(clerk, dumpSafeAuth) + " + direct data layer (env swap + services)"
		scenario.Effort = "M"
		scenario.Plan = []string{
			"Data layer already speaks plain Postgres - the DB move is an env swap.",
			serviceReplacementStep(sites),
			"Create project, `capydb import preflight` (check the auth-FK / RLS / schema warnings), import via the direct endpoint, swap DATABASE_URL (pooled :6432 for app traffic, direct :5432 for DDL), redeploy all consumers.",
		}
	case provider == "neon":
		scenario.Name = "neon + " + authLabel(clerk, dumpSafeAuth) + " (driver codemod + import)"
		scenario.Effort = "S"
		scenario.Plan = []string{
			fmt.Sprintf("Run `capydb migrate codemod neon --write` to swap @neondatabase/serverless -> postgres-js in %d file(s): %s (dry-run first without --write).", len(sites.NeonDriverFiles), strings.Join(firstN(sites.NeonDriverFiles, 4), ", ")),
			"The codemod applies `postgres(url, { max: 1, prepare: false })` for CapyDB's pooled :6432 endpoint and points drizzle-kit at DATABASE_DIRECT_URL (:5432); review its needs-manual-attention list.",
			neonBatchStep(sites),
			"Strip Neon params (`channel_binding=require` - the codemod handles .env files), then: create project, preflight, import from the DIRECT (non `-pooler`) endpoint, `capydb link --overwrite-env`, redeploy all consumers.",
		}
	default:
		scenario.Name = provider + " + " + authLabel(clerk, dumpSafeAuth) + " (standard import + env swap)"
		scenario.Effort = "S"
		scenario.Plan = []string{
			"Driver already speaks plain Postgres: create project, `capydb import preflight`, import, swap DATABASE_URL, redeploy all consumers.",
		}
	}

	if dumpSafeAuth {
		scenario.Warnings = append(scenario.Warnings, "auth tables (better-auth/auth.js/lucia) live in the app database and ride the dump - verify their schema is included, nothing else to do")
	}
	if report.Repo.SupabaseAssets.EdgeFunctions > 0 {
		scenario.Warnings = append(scenario.Warnings, fmt.Sprintf("%d supabase edge function(s) need a new home (route handlers / workers)", report.Repo.SupabaseAssets.EdgeFunctions))
	}
	if len(report.Repo.DistTagPins) > 0 {
		scenario.Warnings = append(scenario.Warnings, "dist-tag dependency pins ("+strings.Join(firstN(report.Repo.DistTagPins, 5), ", ")+"): regenerating the lockfile re-resolves these and can import unrelated API drift - capture a typecheck baseline first and audit the lockfile diff after")
	}
	for _, database := range report.Databases {
		if database.Pooled && database.Provider != "capydb" {
			scenario.Warnings = append(scenario.Warnings, database.Hostname+" is a pooler endpoint - dumps/imports must use the provider's DIRECT endpoint")
		}
		if len(database.Consumers) > 0 {
			scenario.Warnings = append(scenario.Warnings, database.Hostname+" is also referenced by: "+strings.Join(database.Consumers, ", ")+" - the cutover must swap ALL consumers in one step, then run `capydb migrate verify` against the old source")
		}
	}
	// Env shadowing outranks the other warnings: it makes a migration look
	// finished while half the tooling still writes to the old database.
	for _, conflict := range report.EnvConflicts {
		warning := "ENV SHADOWING: " + conflict.Describe() +
			" - the framework resolves one, path-pinned loaders resolve another. Keep DB credentials in exactly ONE env file"
		if pins := pinnedLoadersFor(report.Repo.EnvLoaders, conflict); len(pins) > 0 {
			warning += "; these bypass framework precedence: " + strings.Join(pins, ", ")
		}
		scenario.Warnings = append(scenario.Warnings, warning)
	}

	appendCallSiteWarnings(&scenario, report)
	appendSourceWarnings(&scenario, report)

	if len(report.Repo.Lockfiles) == 0 && len(report.Repo.PackageDirs) > 0 {
		scenario.Warnings = append(scenario.Warnings, "no lockfile found - dependency state is not reproducible; resolve before migrating")
	}
	if has(report.Repo.DataLayers, "drizzle") && !report.Repo.DrizzleSchemaFilter {
		// drizzle-kit v1 hygiene: push/pull manage every schema by default, and
		// migration verification moved into the kit. Suppressed once the config
		// actually sets schemaFilter - a warning that survives its own fix
		// trains people to ignore the list.
		scenario.Warnings = append(scenario.Warnings,
			"drizzle-kit v1 manages ALL schemas by default - set schemaFilter: [\"public\"] in drizzle.config.ts so extension schemas (e.g. cron from pg_cron) are never offered for DROP",
			"after the move, verify migrations with `drizzle-kit check` (branch-conflict detection) and preview DDL with `drizzle-kit push --explain` before applying",
		)
	}
	return scenario
}

// rlsPathStep decides the RLS migration path. The primary discriminator is
// code STYLE, not policy count: server code on the anon key delegates
// authorization to RLS (queries deliberately omit ownership filters), so
// dropping the policies silently returns other users' rows. The live policy
// count is the secondary signal. Calibration: myroomiev3 (483 live policies,
// anon-key server clients) made app-guard rewriting infeasible; realtyiq
// (~30 policies, explicit-filter code) made it trivial.
func rlsPathStep(report Report) string {
	sites := report.Repo.CallSites
	source := report.Source
	if source == nil || source.Policies.Total == 0 {
		return "Map every RLS policy to an app-layer guard in the new data modules before dropping policies (run `capydb import preflight` for the authoritative policy list, or re-run this scan with --source-url to measure the live corpus and get a path recommendation)."
	}
	policies := source.Policies
	anonServerClients := sites.AnonKeyClientFiles > 0 && sites.AnonKeyClientFiles >= sites.ServiceRoleKeyFiles
	if anonServerClients || policies.Total >= 50 {
		detail := fmt.Sprintf("%d live policies", policies.Total)
		if policies.ViaHelpers > 0 {
			detail += fmt.Sprintf(", %d of them via helper function(s) %s", policies.ViaHelpers, strings.Join(firstN(policies.HelperNames, 3), ", "))
		}
		reason := "a corpus this size cannot be rewritten as app guards safely"
		if anonServerClients {
			reason = fmt.Sprintf("%d file(s) build anon-key clients, so the code relies on RLS for row scoping", sites.AnonKeyClientFiles)
		}
		return fmt.Sprintf("KEEP the policies (%s; %s): run `capydb migrate rls --source-url <direct-url>` and set the per-transaction context in the new data layer (@capydb/drizzle withAuthContext) - dropping RLS here silently returns other users' rows.", detail, reason)
	}
	return fmt.Sprintf("Map the %d live RLS policies to app-layer guards in the new data modules before dropping them (small corpus, explicit-filter code style - app guards are simpler than the GUC plumbing).", policies.Total)
}

// appendCallSiteWarnings covers the gates that need only the repo: RPCs whose
// source exists nowhere locally, and grep-count honesty at scale.
func appendCallSiteWarnings(scenario *Scenario, report Report) {
	if missing := report.Repo.RPCsWithoutLocalSource; len(missing) > 0 {
		warning := ".rpc() calls reference database function(s) with no CREATE FUNCTION in this repo: " + strings.Join(firstN(missing, 6), ", ")
		if source := report.Source; source != nil {
			var absentLive []string
			for _, name := range missing {
				if !has(source.PublicFunctions, strings.ToLower(name)) {
					absentLive = append(absentLive, name)
				}
			}
			if len(absentLive) > 0 {
				warning += " - " + strings.Join(firstN(absentLive, 4), ", ") + " do(es) not exist in the live database either: dead call sites or a different schema"
			} else {
				warning += " - they exist only in the live database: recover their source (pg_dump --schema-only) before cutover"
			}
		} else {
			warning += " - they live only in the provider's database: recover their source before cutover"
		}
		scenario.Warnings = append(scenario.Warnings, warning)
	}
	if report.Repo.CallSites.SupabaseData > 100 {
		scenario.Warnings = append(scenario.Warnings, fmt.Sprintf(
			"call-site counts are raw text matches - at this size (%d data call sites) verify liveness (e.g. with knip) before scheduling the rewrite; dead exports skew estimates in both directions",
			report.Repo.CallSites.SupabaseData))
	}
	if files := report.Repo.SupabaseAssets.MigrationFiles; files >= 50 {
		scenario.Warnings = append(scenario.Warnings, fmt.Sprintf(
			"%d migration files - a history this long has usually drifted from the deployed schema (compare with the live policy/table counts): consolidate it into a clean baseline before the move with `capydb migrate squash` (wraps the open-source capysquash engine)",
			files))
	}
}

// appendSourceWarnings turns the live-source probe results into gates. Each
// one exists because its absence mis-sized or endangered a real migration
// (myroomiev3 assessment, 2026-09-01).
func appendSourceWarnings(scenario *Scenario, report Report) {
	source := report.Source
	if source == nil {
		return
	}
	if users := source.AuthUsers; users != nil {
		if users.Count == 0 {
			scenario.Warnings = append(scenario.Warnings,
				"auth.users is EMPTY - zero-user window: migrate BEFORE onboarding users and the cutover stays a no-PII move with a free rollback")
		} else {
			lastSignIn := users.LastSignIn
			if lastSignIn == "" {
				lastSignIn = "never"
			}
			scenario.Warnings = append(scenario.Warnings, fmt.Sprintf(
				"auth.users holds %d account(s) (last sign-in: %s) - live production: plan a write freeze and keep the source paused as rollback", users.Count, lastSignIn))
		}
	}
	if vestigial := source.VestigialExtensions(); len(vestigial) > 0 {
		scenario.Warnings = append(scenario.Warnings,
			"extensions installed but LIKELY unused (0 dependent objects) and not offered on CapyDB: "+strings.Join(vestigial, ", ")+
				" - filter their CREATE EXTENSION lines from the dump instead of blocking on them")
	}
	if blocking := source.BlockingExtensions(); len(blocking) > 0 {
		scenario.Warnings = append(scenario.Warnings,
			"extensions NOT offered on CapyDB with live dependents: "+strings.Join(blocking, ", ")+" - resolve before import")
	}
	if len(source.ImportArtifactTables) > 0 {
		scenario.Warnings = append(scenario.Warnings,
			"possible data import IN FLIGHT (populated migration bookkeeping tables: "+strings.Join(firstN(source.ImportArtifactTables, 3), ", ")+
				") - freeze every other data movement during the cutover so the two migrations cannot interleave")
	}
	if len(source.PersistedURLColumns) > 0 {
		scenario.Warnings = append(scenario.Warnings,
			"absolute provider storage URLs are persisted in data columns: "+strings.Join(firstN(source.PersistedURLColumns, 5), ", ")+
				" - the storage exit needs a data backfill (rewrite stored URLs), not just an API swap")
	}
	if len(source.RealtimeTables) > 0 {
		scenario.Warnings = append(scenario.Warnings, fmt.Sprintf(
			"supabase_realtime publication covers %d table(s) (%s) but the code has %d realtime call site(s) - replace only what is actually subscribed",
			len(source.RealtimeTables), strings.Join(firstN(source.RealtimeTables, 5), ", "), report.Repo.CallSites.SupabaseRealtime))
	}
	if source.Policies.ViaHelpers > 0 {
		scenario.Warnings = append(scenario.Warnings, fmt.Sprintf(
			"%d of %d live policies resolve through helper function(s) (%s) whose bodies read auth.* - policy-text greps undercount them; `capydb migrate rls` converts the policies and its report lists the helper bodies to port",
			source.Policies.ViaHelpers, source.Policies.Total, strings.Join(firstN(source.Policies.HelperNames, 3), ", ")))
	}
}

func authLabel(clerk, dumpSafe bool) string {
	switch {
	case clerk:
		return "clerk"
	case dumpSafe:
		return "db-native auth"
	default:
		return "no detected auth"
	}
}

func serviceReplacementStep(sites CallSites) string {
	var parts []string
	if sites.SupabaseStorage > 0 {
		parts = append(parts, fmt.Sprintf("storage: %d storage.from() call site(s) -> S3-compatible bucket (R2) + object copy", sites.SupabaseStorage))
	}
	if sites.SupabaseRealtime > 0 {
		parts = append(parts, fmt.Sprintf("realtime: %d channel/postgres_changes site(s) -> SSE or polling", sites.SupabaseRealtime))
	}
	if len(parts) == 0 {
		return "No storage/realtime coupling detected - nothing beyond the database moves."
	}
	return "Replace provider services BEFORE the DB cutover (each ships independently): " + strings.Join(parts, "; ") + "."
}

func neonBatchStep(sites CallSites) string {
	if sites.NeonBatchCalls == 0 {
		return "No db.batch()/sql.transaction([]) call sites detected - the driver swap is mechanical."
	}
	return fmt.Sprintf("Rewrite %d db.batch()/sql.transaction([]) call site(s) to db.transaction()/sql.begin() - rebind inner statements to the tx client (a db-bound query inside a max:1 transaction deadlocks).", sites.NeonBatchCalls)
}

func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return append(append([]string{}, values[:n]...), fmt.Sprintf("+%d more", len(values)-n))
}
