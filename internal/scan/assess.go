package scan

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/capydatabase/capydbclient"
)

// Grading: facts in, verdict out.
//
// The grading lives here rather than in whatever renders it, so the terminal,
// the JSON artifact and the assessment page cannot disagree about whether a
// migration is safe. That is the whole reason this is a Go package and not a
// pile of thresholds inside a web bundle: a verdict you can only obtain by
// visiting a marketing page is a verdict you cannot put in CI.
//
// Two rules the findings below all obey:
//
//   - No estimated duration is invented. CapyDB has no measured copy rate for
//     an arbitrary source over an arbitrary network, and a fabricated "about
//     four hours" is worse than no number, because people plan maintenance
//     windows around it. What IS reported is the volume that has to move and
//     whether the chosen path makes the window depend on that volume at all.
//   - Severity reflects what actually stops a migration, not what sounds
//     serious. Unused indexes are a note. A source that cannot open a
//     replication slot is a blocker only for the streaming path, and is
//     recorded as such rather than as a blanket failure.

// AssessmentSchemaVersion is the version of the artifact `--out` writes.
// Anything reading the file (the assessment page, a later CLI) must check it:
// a reader that silently accepts an unknown shape is how a stale page starts
// rendering confident nonsense.
const AssessmentSchemaVersion = 1

// Complexity levels, in increasing order of "you should talk to us".
const (
	LevelReady    = "ready"
	LevelPlanning = "planning"
	LevelAssisted = "assisted"
)

// Finding severities.
const (
	SeverityBlocker = "blocker"
	SeverityWarning = "warning"
	SeverityNote    = "note"
)

// Migration path identifiers.
const (
	PathFollow      = "follow"
	PathDumpRestore = "dump-restore"
)

// Assessment is the whole artifact: the scan, graded. This is what `--out`
// writes and what the assessment page reads.
type Assessment struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	CLIVersion    string    `json:"cli_version"`

	// Level is one of LevelReady, LevelPlanning, LevelAssisted.
	Level    string `json:"level"`
	Headline string `json:"headline"`
	// Verified reports whether the live database was probed. A repo-only scan
	// can find real blockers but cannot clear a migration, so it never grades
	// LevelReady - "nothing to worry about" is not a thing you can conclude
	// from not having looked.
	Verified bool `json:"verified"`

	Provider Profile           `json:"provider"`
	Metrics  AssessmentMetrics `json:"metrics"`
	Path     RecommendedPath   `json:"recommended_path"`

	Blockers []Finding `json:"blockers"`
	Warnings []Finding `json:"warnings"`
	Notes    []Finding `json:"notes"`

	// Report is the underlying scan, kept whole so the page can show the
	// evidence behind any finding.
	Report Report `json:"scan"`

	// Preflight is the control plane's own verdict, present only when the scan
	// ran with --project. It is the authoritative half: unlike everything above
	// it simulates the actual restore against a real target rather than
	// grading the source against a table of rules.
	Preflight *capydbclient.ImportPreflightResult `json:"preflight,omitempty"`
}

// AssessmentMetrics are the headline numbers.
type AssessmentMetrics struct {
	ServerVersion   string `json:"server_version"`
	DatabaseBytes   int64  `json:"database_bytes"`
	TableCount      int    `json:"table_count"`
	IndexBytes      int64  `json:"index_bytes"`
	LargeTables     int    `json:"large_tables"`
	VeryLargeTables int    `json:"very_large_tables"`
	// ReclaimableBytes is index weight that would not have to move at all if
	// the unused and duplicated indexes were dropped first.
	ReclaimableBytes    int64 `json:"reclaimable_bytes"`
	ExtensionsInstalled int   `json:"extensions_installed"`
	ExtensionsBlocking  int   `json:"extensions_blocking"`
	RLSPolicies         int   `json:"rls_policies"`
}

// Finding is one thing worth knowing before migrating.
type Finding struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Severity    string   `json:"severity"`
	Remediation string   `json:"remediation,omitempty"`
	Items       []string `json:"items,omitempty"`
}

// RecommendedPath is how CapyDB would move this particular database.
type RecommendedPath struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Summary  string   `json:"summary"`
	Steps    []string `json:"steps"`
	Commands []string `json:"commands"`
	// Downtime describes the cutover window in words. Never a duration - see
	// the note at the top of this file.
	Downtime string `json:"downtime"`
	// Unavailable explains why the streaming path was not chosen, when it was
	// not. Empty when Path is the best available option.
	Unavailable string `json:"unavailable,omitempty"`
}

// Assess grades a scan report. cliVersion is stamped into the artifact so a
// report can be traced to the binary that produced it.
func Assess(report Report, cliVersion string) Assessment {
	assessment := Assessment{
		SchemaVersion: AssessmentSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		CLIVersion:    cliVersion,
		Report:        report,
		Blockers:      []Finding{},
		Warnings:      []Finding{},
		Notes:         []Finding{},
	}
	assessment.Provider = ProfileFor(resolveProvider(report))
	assessment.Metrics = metricsFrom(report)

	for _, finding := range collectFindings(report, assessment.Provider) {
		switch finding.Severity {
		case SeverityBlocker:
			assessment.Blockers = append(assessment.Blockers, finding)
		case SeverityWarning:
			assessment.Warnings = append(assessment.Warnings, finding)
		default:
			assessment.Notes = append(assessment.Notes, finding)
		}
	}

	assessment.Verified = report.Source != nil
	assessment.Path = recommendPath(report, assessment.Provider)
	assessment.regrade()
	return assessment
}

// AttachPreflight records the control plane's verdict and re-grades.
//
// Re-grading is the point. The preflight is the authoritative half - it
// simulates the actual restore against the actual target - so a failing
// restore-plan check must not end up printed underneath a "Ready to migrate"
// headline. Its failures become blockers; its warnings stay in the preflight
// block rather than being duplicated as findings.
func (a *Assessment) AttachPreflight(preflight *capydbclient.ImportPreflightResult) {
	a.Preflight = preflight
	if preflight == nil {
		return
	}
	for _, check := range preflight.Checks {
		if !strings.EqualFold(check.Status, "fail") {
			continue
		}
		a.Blockers = append(a.Blockers, Finding{
			ID:          "preflight_" + check.Name,
			Severity:    SeverityBlocker,
			Title:       "Import preflight: " + check.Name,
			Detail:      check.Detail,
			Remediation: "The control plane simulated the restore against your target and this check failed; the import will not succeed until it passes.",
		})
	}
	a.regrade()
}

func (a *Assessment) regrade() {
	a.Level = gradeLevel(*a)
	a.Headline = headlineFor(a.Level, a.Verified)
}

// resolveProvider prefers what the server said about itself over what the
// hostname implied - see the comment at the top of identify.go for why. The
// hostname is still the answer for a repo-only scan, and for the case the
// server is a plain Postgres the signals cannot name (Heroku behind a proxy,
// PlanetScale, Timescale Cloud - all identified by their hostname alone).
func resolveProvider(report Report) string {
	if report.Source != nil && report.Source.Provider != ProviderOther && report.Source.Provider != "" {
		return report.Source.Provider
	}
	for _, database := range report.Databases {
		switch database.Provider {
		case ProviderCapyDB, ProviderLocal, ProviderOther, "":
			continue
		default:
			return database.Provider
		}
	}
	return ProviderOther
}

func metricsFrom(report Report) AssessmentMetrics {
	metrics := AssessmentMetrics{}
	source := report.Source
	if source == nil {
		return metrics
	}
	metrics.ServerVersion = source.ServerVersion
	metrics.DatabaseBytes = source.DatabaseSizeBytes
	metrics.TableCount = source.Inventory.TableCount
	metrics.IndexBytes = source.Inventory.IndexBytes
	metrics.LargeTables = source.Inventory.LargeTables
	metrics.VeryLargeTables = source.Inventory.VeryLargeTables
	metrics.ReclaimableBytes = source.Inventory.ReclaimableBytes()
	metrics.ExtensionsInstalled = len(source.Extensions)
	metrics.ExtensionsBlocking = len(source.BlockingExtensions())
	metrics.RLSPolicies = source.Policies.Total
	return metrics
}

// collectFindings produces every finding, in a stable order. Split by source of
// evidence: the repo half runs on any scan, the live half only when the source
// was probed.
func collectFindings(report Report, profile Profile) []Finding {
	findings := repoFindings(report)
	if report.Source != nil {
		findings = append(findings, sourceFindings(*report.Source, profile)...)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
	})
	return findings
}

func severityRank(severity string) int {
	switch severity {
	case SeverityBlocker:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// repoFindings are the ones only a repository scan can produce. No competing
// assessment tool has them, because none of them ever sees the code: a database
// consumed by three services cannot be cut over by swapping one service's
// environment variable, and nothing in the database says so.
func repoFindings(report Report) []Finding {
	var findings []Finding

	for _, database := range report.Databases {
		if len(database.Consumers) == 0 {
			continue
		}
		findings = append(findings, Finding{
			ID:       "shared_database",
			Severity: SeverityBlocker,
			Title:    "This database has other consumers",
			Detail: fmt.Sprintf(
				"%s is also referenced by %d other repository/repositories. A database cutover has to swap every consumer in the same step - one left behind keeps writing to the old database, and the two diverge silently.",
				database.Hostname, len(database.Consumers)),
			Remediation: "Line up the environment change in every consumer before cutting over, and run `capydb doctor` in each afterwards.",
			Items:       database.Consumers,
		})
	}

	for _, database := range report.Databases {
		if !database.Pooled || database.Provider == ProviderCapyDB {
			continue
		}
		findings = append(findings, Finding{
			ID:       "pooled_endpoint",
			Severity: SeverityBlocker,
			Title:    "The configured endpoint is a connection pooler",
			Detail: fmt.Sprintf(
				"%s is a transaction pooler. It cannot hold the session state a consistent dump needs, and it cannot open a replication slot.",
				database.Hostname),
			Remediation: "Use the provider's direct endpoint for the migration. The pooled endpoint stays correct for the application.",
		})
	}

	for _, conflict := range report.EnvConflicts {
		files := make([]string, 0, len(conflict.Assignments))
		for _, assignment := range conflict.Assignments {
			files = append(files, assignment.File+" -> "+assignment.Hostname)
		}
		findings = append(findings, Finding{
			ID:       "env_shadowing",
			Severity: SeverityBlocker,
			Title:    "The same variable points at different databases",
			Detail: fmt.Sprintf(
				"%s resolves to more than one database depending on which environment file wins. During a migration this is how an application ends up reading the new database while its migrations and seed scripts still write to the old one, with nothing reporting an error.",
				conflict.Key),
			Remediation: "Collapse the duplicates to one value before starting, and re-run `capydb migrate scan` to confirm.",
			Items:       files,
		})
	}

	if names := report.Repo.RPCsWithoutLocalSource; len(names) > 0 {
		findings = append(findings, Finding{
			ID:       "rpc_without_source",
			Severity: SeverityWarning,
			Title:    "Database functions the repository calls but does not define",
			Detail: fmt.Sprintf(
				"%d function(s) are called through .rpc() with no CREATE FUNCTION anywhere in the repository, so their definitions exist only inside the provider's database. A dump carries them; a hand-rebuilt schema does not.",
				len(names)),
			Remediation: "Confirm they arrive with the import, or recover their source before cutting over.",
			Items:       firstNames(names, 10),
		})
	}
	return findings
}

// sourceFindings are the ones that need the live database.
func sourceFindings(source SourceFacts, profile Profile) []Finding {
	var findings []Finding

	if names := source.BlockingExtensions(); len(names) > 0 {
		findings = append(findings, Finding{
			ID:       "extensions_unavailable",
			Severity: SeverityBlocker,
			Title:    "Extensions in use that CapyDB does not offer",
			Detail: fmt.Sprintf(
				"%d installed extension(s) are not in CapyDB's catalogue and objects on the source depend on them, so the schema will not restore as it stands.",
				len(names)),
			Remediation: "Replace or remove them before importing, or ask us to add them - the catalogue grows on request.",
			Items:       names,
		})
	}
	if names := source.VestigialExtensions(); len(names) > 0 {
		findings = append(findings, Finding{
			ID:       "extensions_vestigial",
			Severity: SeverityNote,
			Title:    "Extensions installed but apparently unused",
			Detail: fmt.Sprintf(
				"%d installed extension(s) are not in CapyDB's catalogue, and nothing on the source depends on them. Dropping them before the dump removes them from the migration entirely.",
				len(names)),
			Remediation: "Confirm nothing calls them from a function body - that is the one usage a dependency check cannot see - then DROP EXTENSION.",
			Items:       names,
		})
	}

	if source.Policies.Total > 0 && source.Policies.DirectAuthRefs+source.Policies.ViaHelpers > 0 {
		coupled := source.Policies.DirectAuthRefs + source.Policies.ViaHelpers
		detail := fmt.Sprintf(
			"%d of %d row-level security policies resolve the caller through the provider's auth functions. Those functions do not exist on plain Postgres, so the policies are inert after the move and every query they were protecting returns whatever the query asks for.",
			coupled, source.Policies.Total)
		if len(source.Policies.HelperNames) > 0 {
			detail += fmt.Sprintf(" %d of them go through helper functions (%s), which a search of the policy text alone would miss.",
				source.Policies.ViaHelpers, strings.Join(firstNames(source.Policies.HelperNames, 5), ", "))
		}
		findings = append(findings, Finding{
			ID:          "provider_auth_policies",
			Severity:    SeverityBlocker,
			Title:       "Row-level security is bound to the provider's auth system",
			Detail:      detail,
			Remediation: "Convert the policies with `capydb migrate rls`, which rewrites them onto a session variable your application sets, or move authorization into the data layer.",
		})
	}

	if users := source.AuthUsers; users != nil && users.Count > 0 {
		findings = append(findings, Finding{
			ID:       "provider_auth_users",
			Severity: SeverityBlocker,
			Title:    "Identities live in the provider's auth system",
			Detail: fmt.Sprintf(
				"%d user(s) exist in the provider-managed auth schema (last sign-in: %s). That schema is not part of the database CapyDB imports, so the identities need a destination before the application can work against the new database.",
				users.Count, orNever(users.LastSignIn)),
			Remediation: "Move authentication to a provider-independent system first, then migrate the data.",
		})
	}

	if count := len(source.Inventory.TablesWithoutReplicaIdentity); count > 0 {
		findings = append(findings, Finding{
			ID:       "no_replica_identity",
			Severity: SeverityWarning,
			Title:    "Tables that cannot be streamed",
			Detail: fmt.Sprintf(
				"%d table(s) have neither a primary key nor a replica identity. A dump and restore copies them fine; a streaming import cannot replicate their updates, and the failure arrives mid-migration rather than at setup.",
				count),
			Remediation: "Add a primary key, or set REPLICA IDENTITY FULL on each - which replicates correctly at the cost of sending the whole previous row per change.",
			Items:       firstNames(source.Inventory.TablesWithoutReplicaIdentity, 10),
		})
	}

	if count := source.Inventory.VeryLargeTables; count > 0 {
		findings = append(findings, Finding{
			ID:       "very_large_tables",
			Severity: SeverityWarning,
			Title:    "Single tables large enough to dictate the schedule",
			Detail: fmt.Sprintf(
				"%d table(s) are over 500 GiB. One table that size sets the length of the copy on its own, because it cannot be split across workers the way a wide schema can.",
				count),
			Remediation: "Use a streaming import so the cutover window does not depend on how long the copy takes, and consider partitioning the table before the move.",
			Items:       largeTableNames(source.Inventory),
		})
	} else if count := source.Inventory.LargeTables; count > 0 {
		findings = append(findings, Finding{
			ID:          "large_tables",
			Severity:    SeverityWarning,
			Title:       "Large tables in the copy",
			Detail:      fmt.Sprintf("%d table(s) are over 100 GiB and will dominate the copy time.", count),
			Remediation: "A streaming import keeps the cutover window independent of the copy: the base copy runs while the application stays on the old database.",
			Items:       largeTableNames(source.Inventory),
		})
	}

	if cycles := source.Inventory.CycleSummary(); len(cycles) > 0 {
		findings = append(findings, Finding{
			ID:       "foreign_key_cycles",
			Severity: SeverityWarning,
			Title:    "Tables that reference each other",
			Detail: fmt.Sprintf(
				"%d group(s) of tables form a foreign-key cycle, so no insertion order satisfies every constraint. The restore has to defer the constraints, and any tool that copies table-by-table with them enforced will fail on these.",
				len(cycles)),
			Remediation: "Resolving the cycle before migrating is cheaper than working around it, but a dump/restore handles it either way.",
			Items:       cycles,
		})
	}

	if sequences := source.Inventory.SequencesNearExhaustion; len(sequences) > 0 {
		items := make([]string, 0, len(sequences))
		for _, sequence := range sequences {
			items = append(items, fmt.Sprintf("%s.%s (%s, %.0f%% consumed)",
				sequence.Schema, sequence.Name, sequence.DataType, sequence.UsedRatio*100))
		}
		findings = append(findings, Finding{
			ID:       "sequences_near_exhaustion",
			Severity: SeverityWarning,
			Title:    "Sequences near the end of their range",
			Detail: fmt.Sprintf(
				"%d sequence(s) have consumed over %.0f%% of their range. Widening the column is a table rewrite - far cheaper on the source before the move than on a freshly imported database under production load.",
				len(sequences), sequenceExhaustionRatio*100),
			Remediation: "Widen the affected columns to bigint before migrating.",
			Items:       items,
		})
	}

	if bytes := source.Inventory.ReclaimableBytes(); bytes > 0 {
		items := make([]string, 0, len(source.Inventory.UnusedIndexes)+len(source.Inventory.DuplicateIndexes))
		for _, index := range source.Inventory.UnusedIndexes {
			items = append(items, fmt.Sprintf("%s.%s -> %s (never scanned, %s)",
				index.Schema, index.Table, index.Name, humanBytes(index.Bytes)))
		}
		for _, index := range source.Inventory.DuplicateIndexes {
			items = append(items, fmt.Sprintf("%s.%s -> %s (duplicates %s, %s)",
				index.Schema, index.Table, index.Name, index.DuplicateOf, humanBytes(index.Bytes)))
		}
		findings = append(findings, Finding{
			ID:       "reclaimable_indexes",
			Severity: SeverityNote,
			Title:    "Indexes that would be copied for nothing",
			Detail: fmt.Sprintf(
				"%s of indexes have never been scanned or duplicate another index. Every one of them costs twice in a migration: the bytes cross the wire and the index is rebuilt on arrival.",
				humanBytes(bytes)),
			Remediation: "Index scan counts accumulate since the last statistics reset, so check the source has been up long enough to be representative before dropping anything.",
			Items:       firstNames(items, 15),
		})
	}

	if count := source.Inventory.EmptyTables; count > 0 {
		findings = append(findings, Finding{
			ID:          "empty_tables",
			Severity:    SeverityNote,
			Title:       "Empty tables",
			Detail:      fmt.Sprintf("%d table(s) hold no rows. They cost nothing to migrate, but they often mark features that were abandoned.", count),
			Remediation: "Optional cleanup - leaving them in place is perfectly safe.",
		})
	}

	if columns := source.PersistedURLColumns; len(columns) > 0 {
		findings = append(findings, Finding{
			ID:       "persisted_provider_urls",
			Severity: SeverityWarning,
			Title:    "Provider storage URLs stored in the data",
			Detail: fmt.Sprintf(
				"%d column(s) hold absolute URLs pointing at the provider's object storage. Those URLs keep working only while the old account does, so leaving the storage behind turns into a data backfill rather than a configuration change.",
				len(columns)),
			Remediation: "Move the objects and rewrite the stored URLs, or store keys rather than absolute URLs.",
			Items:       firstNames(columns, 10),
		})
	}

	if tables := source.ImportArtifactTables; len(tables) > 0 {
		findings = append(findings, Finding{
			ID:          "import_in_flight",
			Severity:    SeverityWarning,
			Title:       "Another data movement may be in progress",
			Detail:      "Populated migration or import bookkeeping tables are present. Two data movements over the same database at once is how rows get lost.",
			Remediation: "Finish or stop the other movement before starting this one.",
			Items:       tables,
		})
	}

	for _, note := range profile.Notes {
		findings = append(findings, Finding{
			ID:       "provider_note",
			Severity: SeverityNote,
			Title:    profile.Name,
			Detail:   note,
		})
	}
	return findings
}

// recommendPath chooses between a streaming import and a dump/restore, and says
// plainly why when the streaming path is not available.
func recommendPath(report Report, profile Profile) RecommendedPath {
	follow := RecommendedPath{
		ID:      PathFollow,
		Name:    "Streaming import with a short cutover",
		Summary: "CapyDB copies the database while the application keeps running against the old one, then streams every subsequent change until you cut over. The window is the time it takes to swap a connection string, not the time it takes to copy the data.",
		Steps: []string{
			"Create the CapyDB project and enable the extensions the source uses.",
			"Run the preflight against the source to confirm the restore will succeed.",
			"Start the import in follow mode; the base copy runs while the application stays on the old database.",
			"Run the test suite against the new cell while the stream keeps it current.",
			"Cut over once replication lag is near zero, then swap the connection string in every consumer.",
		},
		Commands: []string{
			"capydb import preflight --project <project> --source-url \"$OLD_DATABASE_URL\"",
			"capydb import --project <project> --source-url \"$OLD_DATABASE_URL\" --follow",
			"capydb import follow-status --project <project>",
			"capydb import cutover --project <project>",
		},
		Downtime: "A brief pause at cutover, independent of the size of the database.",
	}

	dump := RecommendedPath{
		ID:      PathDumpRestore,
		Name:    "Dump and restore inside a maintenance window",
		Summary: "Writes stop, the database is exported and loaded into the new cell, and the application comes back pointing at CapyDB. Simple and predictable, at the cost of a window that grows with the size of the data.",
		Steps: []string{
			"Create the CapyDB project and enable the extensions the source uses.",
			"Stop writes to the source so the export is consistent.",
			"Export with pg_dump in custom format and import the file.",
			"Verify the row counts and the objects you care about.",
			"Swap the connection string in every consumer and resume writes.",
		},
		Commands: []string{
			"pg_dump -Fc -d \"$OLD_DATABASE_URL\" -f source.dump",
			"capydb import --project <project> --file source.dump --recreate",
			"capydb doctor",
		},
		Downtime: "A window that scales with the size of the database - rehearse it against a preview cell before committing to a date.",
	}

	if profile.Logical == LogicalNever {
		dump.Unavailable = fmt.Sprintf(
			"%s offers neither logical replication nor superuser access, so a streaming import cannot be set up from it at all.",
			profile.Name)
		return dump
	}

	source := report.Source
	if source == nil {
		// Repo-only scan: recommend the better path, and say it is unverified.
		follow.Unavailable = "Not yet verified - re-run with --source-url to check the source can actually stream."
		return follow
	}
	if source.Replication.Ready {
		return follow
	}

	dump.Unavailable = "The source cannot stream changes as currently configured: " +
		strings.Join(source.Replication.Blockers, "; ") + "."
	// The provider's remediation is about server settings, so it only belongs
	// here when a server setting is actually what is missing.
	if profile.LogicalFix != "" && source.Replication.NeedsServerConfig() {
		dump.Unavailable += " " + profile.LogicalFix
	}
	dump.Unavailable += " Fix that and re-run this scan; the streaming path becomes available."
	return dump
}

// gradeLevel maps findings to the three-way verdict. Blockers decide it; size
// alone escalates only to "planning", because a large database that is
// otherwise clean is a scheduling problem, not a risky one.
func gradeLevel(assessment Assessment) string {
	switch {
	case len(assessment.Blockers) > 0:
		return LevelAssisted
	case assessment.Metrics.VeryLargeTables > 0:
		return LevelAssisted
	case len(assessment.Warnings) > 0, assessment.Metrics.LargeTables > 0:
		return LevelPlanning
	case !assessment.Verified:
		// Nothing looked at the database. Findings the repository produced are
		// still real, but a clean bill of health is not something you can
		// conclude from not having looked.
		return LevelPlanning
	default:
		return LevelReady
	}
}

func headlineFor(level string, verified bool) string {
	switch {
	case level == LevelReady:
		return "Ready to migrate: nothing here needs a decision before you start."
	case level == LevelPlanning && !verified:
		return "Not yet assessed: re-run with --source-url to check the database itself, not just the code."
	case level == LevelPlanning:
		return "Migratable with planning: a handful of things are worth settling first."
	default:
		return "Worth doing together: this database has blockers that a self-service import will not resolve."
	}
}

func largeTableNames(inventory SourceInventory) []string {
	var names []string
	for _, table := range inventory.Tables {
		if table.Bytes <= largeTableBytes {
			continue
		}
		names = append(names, fmt.Sprintf("%s.%s (%s, %s)",
			table.Schema, table.Name, humanBytes(table.Bytes), rowEstimate(table.Rows)))
	}
	return names
}

// rowEstimate renders a planner row estimate, where -1 means the table has
// never been analysed rather than "minus one rows".
func rowEstimate(rows int64) string {
	if rows < 0 {
		return "row count unknown"
	}
	return fmt.Sprintf("~%d rows", rows)
}

func orNever(value string) string {
	if strings.TrimSpace(value) == "" {
		return "never"
	}
	return value
}

// humanBytes renders a byte count in binary units. Duplicated rather than
// shared with the CLI's formatBytes: this package is the one that must not
// depend on the command layer.
func humanBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTP"[exp])
}
