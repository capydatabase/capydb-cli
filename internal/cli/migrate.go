package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for --source-url
	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/scan"
)

// newMigrateCommand groups the provider-migration tooling: `scan` (read-only
// repo classification + plan) and `verify` (post-cutover source watch). Both
// work without CapyDB credentials - they operate on the user's repo and the
// user's OLD database.
func (a *app) newMigrateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "migrate",
		Short: "Plan and verify a migration from another Postgres provider",
	}
	command.AddCommand(a.newMigrateScanCommand())
	command.AddCommand(a.newMigrateVerifyCommand())
	command.AddCommand(a.newMigrateVerifyRLSCommand())
	command.AddCommand(a.newMigrateDepsCommand())
	command.AddCommand(a.newMigrateCodemodCommand())
	command.AddCommand(a.newMigrateRLSCommand())
	command.AddCommand(a.newMigrateSquashCommand())
	return command
}

func (a *app) newMigrateScanCommand() *cobra.Command {
	var portfolioDir string
	var sourceURL string
	var outPath string
	var projectRef string

	command := &cobra.Command{
		Use:   "scan [path]",
		Short: "Classify a repository and emit a CapyDB migration plan (read-only)",
		Long: `Scans a repository (nothing mutated) and classifies it across the three
migration axes - source database provider, auth system, data-access layer -
plus provider-coupled services, then emits a per-database migration plan with
an effort grade. The repo scan is fully offline.

Pass --source-url with the OLD database's DIRECT connection string to also
measure what the repo cannot show: which provider actually runs the database
(the server is asked, not the hostname), whether it can stream changes for a
--follow import, the physical inventory (table sizes, tables that cannot be
replicated, foreign-key cycles, sequences near exhaustion, indexes nothing
reads), the live RLS corpus and how it resolves the caller, whether real users
exist yet, extensions that are installed but unused, provider URLs persisted in
data rows, and signs of another data import in flight. The probes are plain
read-only SELECTs with a statement timeout; anything the role cannot see is
reported as skipped.

Pass --portfolio <dir> to also grep sibling repos' env files for the same
database hostnames: a database's cutover must swap EVERY consumer in one step.

Pass --project to add the control plane's own import preflight, which simulates
the actual restore against your target rather than grading the source against a
table of rules. Pass --out to write the whole assessment as JSON, for CI or for
the assessment page at capydb.dev/switch/check.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := a.cwd
			if len(args) == 1 {
				root = args[0]
			}
			report, err := scan.Run(root, strings.TrimSpace(portfolioDir))
			if err != nil {
				return fmt.Errorf("migrate scan: %w", err)
			}
			url := strings.TrimSpace(sourceURL)
			if url != "" {
				db, err := sql.Open("pgx", url)
				if err != nil {
					return fmt.Errorf("migrate scan: open source database: %w", err)
				}
				defer func() { _ = db.Close() }()
				facts, err := scan.ProbeSource(cmd.Context(), db)
				if err != nil {
					return fmt.Errorf("migrate scan: %w", err)
				}
				report.AttachSource(facts)
			}

			assessment := scan.Assess(report, a.version)

			// The control plane's preflight is the authoritative half: it
			// simulates the restore against the real target instead of grading
			// the source against a table of rules. It needs both an
			// authenticated project and a live source, so it is opt-in.
			if strings.TrimSpace(projectRef) != "" {
				if url == "" {
					return usageErrorf("--project requires --source-url")
				}
				preflight, err := a.scanPreflight(cmd.Context(), projectRef, url)
				if err != nil {
					return err
				}
				// AttachPreflight, not a bare field assignment: the control
				// plane's failures are blockers and have to re-grade the
				// verdict, or a failed restore simulation prints underneath a
				// "Ready to migrate" headline.
				assessment.AttachPreflight(preflight)
			}

			if outPath != "" {
				if err := writeAssessmentFile(outPath, assessment); err != nil {
					return err
				}
				if !a.jsonOutput() {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"Wrote assessment to %s - drop it on https://capydb.dev/switch/check for the full report.\n", outPath)
				}
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"assessment": assessment})
			}
			writeMigrateScan(cmd, report)
			writeAssessment(cmd.OutOrStdout(), assessment)
			return nil
		},
	}

	command.Flags().StringVar(&portfolioDir, "portfolio", "", "Directory of sibling repos to check for other consumers of the same databases")
	command.Flags().StringVar(&sourceURL, "source-url", "", "The OLD provider's DIRECT connection string - adds read-only live-database probes to the plan")
	command.Flags().StringVar(&projectRef, "project", "", "Target project id, slug, or name - adds the control plane's import preflight to the assessment (requires --source-url)")
	command.Flags().StringVar(&outPath, "out", "", "Write the full assessment as JSON to this path")
	return command
}

// scanPreflight runs the control plane's import preflight for the assessment.
// A failing preflight is NOT an error here: the whole point of the scan is to
// report problems, and returning non-zero would hide the rest of the report.
// `capydb import preflight` remains the gate that exits non-zero.
func (a *app) scanPreflight(ctx context.Context, projectRef, sourceURL string) (*api.ImportPreflight, error) {
	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return nil, err
	}
	project, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return nil, err
	}
	preflight, err := client.ImportPreflight(ctx, project.ID, sourceURL)
	if err != nil {
		return nil, fmt.Errorf("migrate scan: import preflight: %w", err)
	}
	return &preflight, nil
}

// writeAssessmentFile writes the assessment artifact. 0o600: the report carries
// hostnames, schema names and sampled column names from a production database.
func writeAssessmentFile(path string, assessment scan.Assessment) error {
	encoded, err := json.MarshalIndent(assessment, "", "  ")
	if err != nil {
		return fmt.Errorf("migrate scan: encode assessment: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("migrate scan: write assessment: %w", err)
	}
	return nil
}

func writeMigrateScan(cmd *cobra.Command, report scan.Report) {
	out := cmd.OutOrStdout()

	_, _ = fmt.Fprintf(out, "scenario: %s\n", report.Scenario.Name)
	_, _ = fmt.Fprintf(out, "effort: %s\n\n", report.Scenario.Effort)

	if len(report.Databases) == 0 {
		_, _ = fmt.Fprintln(out, "databases: none found in env files")
	}
	for _, database := range report.Databases {
		pooled := ""
		if database.Pooled {
			pooled = " [pooler endpoint - do not dump from this]"
		}
		_, _ = fmt.Fprintf(out, "database: %s (%s)%s\n", database.Hostname, database.Provider, pooled)
		if len(database.EnvKeys) > 0 {
			_, _ = fmt.Fprintf(out, "  env keys: %s\n", strings.Join(database.EnvKeys, ", "))
		}
		if len(database.Consumers) > 0 {
			_, _ = fmt.Fprintf(out, "  ALSO CONSUMED BY: %s\n", strings.Join(database.Consumers, ", "))
		}
	}

	_, _ = fmt.Fprintf(out, "\nauth: %s\n", joinOrDash(report.Repo.AuthSystems))
	_, _ = fmt.Fprintf(out, "data layers: %s\n", joinOrDash(report.Repo.DataLayers))
	sites := report.Repo.CallSites
	_, _ = fmt.Fprintf(out, "call sites: supabase data=%d auth=%d storage=%d realtime=%d | neon driver files=%d batch calls=%d\n",
		sites.SupabaseData, sites.SupabaseAuth, sites.SupabaseStorage, sites.SupabaseRealtime,
		len(sites.NeonDriverFiles), sites.NeonBatchCalls)
	if report.Repo.SupabaseAssets.MigrationFiles > 0 || report.Repo.SupabaseAssets.EdgeFunctions > 0 {
		_, _ = fmt.Fprintf(out, "supabase assets: %d migration file(s), %d edge function(s)\n",
			report.Repo.SupabaseAssets.MigrationFiles, report.Repo.SupabaseAssets.EdgeFunctions)
	}
	writeMigrateScanSource(out, report.Source)

	_, _ = fmt.Fprintln(out, "\nplan:")
	for index, step := range report.Scenario.Plan {
		_, _ = fmt.Fprintf(out, "  %d. %s\n", index+1, step)
	}
	if len(report.Scenario.Warnings) > 0 {
		_, _ = fmt.Fprintln(out, "\nwarnings:")
		for _, warning := range report.Scenario.Warnings {
			_, _ = fmt.Fprintf(out, "  - %s\n", warning)
		}
	}
}

// writeMigrateScanSource renders the live-database probe block. The repo says
// what the code does; this block says what the database says - when they
// disagree, the database wins.
func writeMigrateScanSource(out io.Writer, source *scan.SourceFacts) {
	if source == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "\nsource database (live, read-only probes):\n")
	_, _ = fmt.Fprintf(out, "  postgres %s, %s, %d public table(s)\n",
		source.ServerVersion, formatBytes(source.DatabaseSizeBytes), source.PublicTables)
	if len(source.ProviderSignals) > 0 {
		_, _ = fmt.Fprintf(out, "  provider (per the server): %s - %s\n",
			source.Profile().Name, strings.Join(source.ProviderSignals, ", "))
	}
	writeMigrateScanReplication(out, source.Replication)
	writeMigrateScanInventory(out, source.Inventory)
	policies := source.Policies
	if policies.Total > 0 {
		helperSuffix := ""
		if policies.ViaHelpers > 0 {
			helperSuffix = fmt.Sprintf(" via helpers (%s)", strings.Join(policies.HelperNames, ", "))
		}
		_, _ = fmt.Fprintf(out, "  rls policies: %d live - %d direct auth.*, %d%s\n",
			policies.Total, policies.DirectAuthRefs, policies.ViaHelpers, helperSuffix)
	}
	writeMigrateScanRLSRisk(out, source)
	if users := source.AuthUsers; users != nil {
		lastSignIn := users.LastSignIn
		if lastSignIn == "" {
			lastSignIn = "never"
		}
		_, _ = fmt.Fprintf(out, "  auth users: %d (last sign-in: %s)\n", users.Count, lastSignIn)
	}
	for _, extension := range source.Extensions {
		if extension.Available {
			continue
		}
		usage := "likely unused (0 dependent objects)"
		if extension.Dependents > 0 {
			usage = fmt.Sprintf("%d dependent object(s)", extension.Dependents)
		}
		_, _ = fmt.Fprintf(out, "  extension not on CapyDB: %s %s - %s\n", extension.Name, extension.Version, usage)
	}
	for _, bucket := range source.StorageBuckets {
		_, _ = fmt.Fprintf(out, "  storage bucket: %s (%d object(s), %s)\n", bucket.Name, bucket.Objects, formatBytes(bucket.Bytes))
	}
	for _, note := range source.Notes {
		_, _ = fmt.Fprintf(out, "  note: %s\n", note)
	}
}

// writeMigrateScanRLSRisk renders the three checks that answer "what stops
// working when the app stops connecting as a role that bypasses RLS". Each was
// hand-written mid-migration on myroomiev3 and each changed the plan.
func writeMigrateScanRLSRisk(out io.Writer, source *scan.SourceFacts) {
	exposure := source.RLSExposure
	if exposure.RLSEnabled > 0 {
		owner := "unknown"
		if len(exposure.Owners) == 1 {
			owner = exposure.Owners[0]
		} else if len(exposure.Owners) > 1 {
			owner = fmt.Sprintf("%d owners (%s)", len(exposure.Owners), strings.Join(exposure.Owners, ", "))
		}
		_, _ = fmt.Fprintf(out, "  rls force: %d of %d rls-enabled table(s) FORCEd, owner %s\n",
			exposure.RLSForced, exposure.RLSEnabled, owner)
		if inert := exposure.InertPolicyRisk(); inert > 0 {
			_, _ = fmt.Fprintf(out,
				"    ! %d table(s) have policies that will NOT apply on a single-credential\n"+
					"      destination: the app connects as the owner, and an owner bypasses row\n"+
					"      security unless the table is FORCEd. No error is raised - the rows just\n"+
					"      come back. Convert with --no-service-escape (FORCEs every table) and\n"+
					"      prove it with a two-user denial test.\n", inert)
		}
	}

	if len(source.DefinerFunctions) > 0 {
		writes, silent := 0, 0
		for _, fn := range source.DefinerFunctions {
			if fn.Writes {
				writes++
			}
			if fn.SwallowsErrors {
				silent++
			}
		}
		_, _ = fmt.Fprintf(out,
			"  security definer: %d function(s) reach an rls table (%d write, %d swallow errors)\n",
			len(source.DefinerFunctions), writes, silent)
		_, _ = fmt.Fprintf(out,
			"    ! these run as the owner today and rely on the bypass FORCE removes. The\n"+
				"      writers fail loudly on WITH CHECK; the %d that end in EXCEPTION WHEN\n"+
				"      OTHERS fail silently. Classify each before estimating.\n", silent)
	}

	if len(source.PolicyCycles) > 0 {
		tables := map[string]bool{}
		for _, cycle := range source.PolicyCycles {
			tables[cycle.Table] = true
		}
		_, _ = fmt.Fprintf(out,
			"  policy cycles: %d policy/helper pair(s) across %d table(s) reach their own table\n",
			len(source.PolicyCycles), len(tables))
		for _, cycle := range source.PolicyCycles {
			_, _ = fmt.Fprintf(out, "    - %s.%s -> %s() reads %s\n",
				cycle.Table, cycle.Policy, cycle.Helper, cycle.Table)
		}
		_, _ = fmt.Fprintf(out,
			"    ! under FORCE RLS these are a STATIC error (\"infinite recursion detected in\n"+
				"      policy\"). Fix: inline the predicate into that table's own policy and keep\n"+
				"      the helper for the cross-table callers it was written for - no bypass needed.\n")
	}
}

// writeAssessment renders the graded verdict under the raw scan. The scan block
// above it is evidence; this block is the answer. Keeping both means a reader
// who disagrees with a finding can see exactly which fact produced it.
func writeAssessment(out io.Writer, assessment scan.Assessment) {
	_, _ = fmt.Fprintf(out, "\nassessment: %s\n", assessment.Headline)
	_, _ = fmt.Fprintf(out, "source provider: %s\n", assessment.Provider.Name)

	metrics := assessment.Metrics
	if metrics.TableCount > 0 || metrics.DatabaseBytes > 0 {
		_, _ = fmt.Fprintf(out, "size: %s across %d table(s), %s of indexes\n",
			formatBytes(metrics.DatabaseBytes), metrics.TableCount, formatBytes(metrics.IndexBytes))
		if metrics.ReclaimableBytes > 0 {
			_, _ = fmt.Fprintf(out, "  %s of that is indexes nothing reads or duplicates - dropping them first shrinks the copy\n",
				formatBytes(metrics.ReclaimableBytes))
		}
	}

	path := assessment.Path
	_, _ = fmt.Fprintf(out, "\nrecommended path: %s\n", path.Name)
	_, _ = fmt.Fprintf(out, "  %s\n", path.Summary)
	_, _ = fmt.Fprintf(out, "  cutover: %s\n", path.Downtime)
	if path.Unavailable != "" {
		_, _ = fmt.Fprintf(out, "  why not a streaming import: %s\n", path.Unavailable)
	}
	for _, command := range path.Commands {
		_, _ = fmt.Fprintf(out, "    $ %s\n", command)
	}

	writeFindings(out, "blockers", assessment.Blockers)
	writeFindings(out, "warnings", assessment.Warnings)
	writeFindings(out, "notes", assessment.Notes)

	if assessment.Preflight != nil {
		_, _ = fmt.Fprintln(out, "\ncontrol-plane preflight (simulated against your target):")
		writeImportPreflight(out, *assessment.Preflight)
	}
}

func writeFindings(out io.Writer, label string, findings []scan.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n%s (%d):\n", label, len(findings))
	for _, finding := range findings {
		_, _ = fmt.Fprintf(out, "  - %s\n    %s\n", finding.Title, finding.Detail)
		if finding.Remediation != "" {
			_, _ = fmt.Fprintf(out, "    fix: %s\n", finding.Remediation)
		}
		for _, item := range finding.Items {
			_, _ = fmt.Fprintf(out, "      * %s\n", item)
		}
	}
}

// writeMigrateScanReplication reports whether a streaming import is possible
// from this source AS CONNECTED - the provider's documented default is not
// evidence about the database in front of us.
func writeMigrateScanReplication(out io.Writer, replication scan.SourceReplication) {
	if replication.WALLevel == "" {
		return
	}
	verdict := "cannot stream changes yet"
	if replication.Ready {
		verdict = "ready to stream changes (`capydb import --follow`)"
	}
	_, _ = fmt.Fprintf(out, "  replication: %s - wal_level=%s, %d/%d slots free, %d wal sender(s)\n",
		verdict, replication.WALLevel,
		replication.MaxReplicationSlots-replication.UsedSlots, replication.MaxReplicationSlots,
		replication.MaxWALSenders)
	for _, blocker := range replication.Blockers {
		_, _ = fmt.Fprintf(out, "    - %s\n", blocker)
	}
}

func writeMigrateScanInventory(out io.Writer, inventory scan.SourceInventory) {
	if inventory.TableCount == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "  inventory: %d table(s), %s total, %s of indexes",
		inventory.TableCount, formatBytes(inventory.TotalTableBytes), formatBytes(inventory.IndexBytes))
	if inventory.PartitionedTables > 0 {
		_, _ = fmt.Fprintf(out, ", %d partitioned", inventory.PartitionedTables)
	}
	_, _ = fmt.Fprintln(out)
	if inventory.LargeTables > 0 {
		_, _ = fmt.Fprintf(out, "    %d table(s) over 100GiB (%d over 500GiB)\n",
			inventory.LargeTables, inventory.VeryLargeTables)
	}
	if count := len(inventory.TablesWithoutPrimaryKey); count > 0 {
		_, _ = fmt.Fprintf(out, "    %d table(s) without a primary key\n", count)
	}
	if count := len(inventory.ForeignKeyCycles); count > 0 {
		_, _ = fmt.Fprintf(out, "    %d foreign-key cycle(s)\n", count)
	}
	if bytes := inventory.ReclaimableBytes(); bytes > 0 {
		_, _ = fmt.Fprintf(out, "    %s of unused/duplicate indexes\n", formatBytes(bytes))
	}
	if count := len(inventory.SequencesNearExhaustion); count > 0 {
		_, _ = fmt.Fprintf(out, "    %d sequence(s) near the end of their range\n", count)
	}
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}

// placeholderSecretPattern flags env values that were clearly never replaced.
var placeholderSecretPattern = regexp.MustCompile(`(?i)^(|replace([-_ ]?me)?.*|change[-_ ]?me.*|placeholder.*|your[-_].*|<[^>]*>|xxx+|dummy.*|example.*|todo.*|fixme.*)$`)

// secretKeyPattern selects env keys whose values must be real secrets.
var secretKeyPattern = regexp.MustCompile(`(?i)(SECRET|PASSWORD|_KEY$|_TOKEN)`)

// newMigrateVerifyRLSCommand proves the converted policy corpus behaves the
// same as the original, which is the one property nobody can eyeball: 500
// policies across 170 tables, failing silently as correct-looking rows that
// belong to someone else.
func (a *app) newMigrateVerifyRLSCommand() *cobra.Command {
	var sourceURL, targetURL, contextsPath, tableList string
	var probeWrites bool

	command := &cobra.Command{
		Use:   "verify-rls",
		Short: "Prove the migrated RLS behaves identically: same reads, same identities, both databases",
		Long: "Reads every RLS-enabled table as every caller identity you supply, against BOTH the old\n" +
			"database and the new one, and diffs the answers. A row count that matches is evidence; a\n" +
			"denial that matches is evidence too, so errors are compared by SQLSTATE.\n" +
			"\n" +
			"Every read runs in its own transaction and is rolled back - claims are transaction-local,\n" +
			"so the transaction is the identity boundary, and nothing can alter the databases it audits.\n" +
			"\n" +
			"Contexts are a JSON array of the identities your app actually serves:\n" +
			`  [{"label":"anon","claims":{}},` + "\n" +
			`   {"label":"user_a","claims":{"sub":"user_123","role":"authenticated"}},` + "\n" +
			`   {"label":"admin","claims":{"sub":"user_9","role":"authenticated","app_role":"admin"}}]` + "\n" +
			"\n" +
			"Include the anonymous caller. It is the context most likely to regress and the one nobody\n" +
			"tests by logging in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if strings.TrimSpace(sourceURL) == "" || strings.TrimSpace(targetURL) == "" {
				return usageErrorf("--source-url and --target-url are both required")
			}
			if strings.TrimSpace(contextsPath) == "" {
				return usageErrorf("--contexts is required: the identities to read as")
			}
			raw, err := os.ReadFile(contextsPath)
			if err != nil {
				return fmt.Errorf("read contexts: %w", err)
			}
			contexts, err := scan.ParseBatteryContexts(raw)
			if err != nil {
				return err
			}

			source, err := sql.Open("pgx", strings.TrimSpace(sourceURL))
			if err != nil {
				return fmt.Errorf("open source database: %w", err)
			}
			defer func() { _ = source.Close() }()
			target, err := sql.Open("pgx", strings.TrimSpace(targetURL))
			if err != nil {
				return fmt.Errorf("open target database: %w", err)
			}
			defer func() { _ = target.Close() }()

			var tables []string
			if list := strings.TrimSpace(tableList); list != "" {
				for _, name := range strings.Split(list, ",") {
					if name = strings.TrimSpace(name); name != "" {
						tables = append(tables, name)
					}
				}
			} else {
				// Discover from the TARGET: it is the database whose policies
				// are under test, and a table missing there is itself a finding.
				tables, err = scan.ListRLSTables(ctx, target)
				if err != nil {
					return fmt.Errorf("list rls tables on target: %w", err)
				}
			}
			if len(tables) == 0 {
				return fmt.Errorf("no RLS-enabled tables found: pass --tables, or check that the bundle was applied")
			}

			modes := 1
			if probeWrites {
				modes = 2
				_, _ = fmt.Fprintln(out,
					"write probes ON: each table is also read FOR UPDATE, which applies the UPDATE\n"+
						"policy's USING clause. Nothing is written and every probe is rolled back, but it\n"+
						"does take row locks for the life of each probe - avoid it on a busy production source.")
			}
			_, _ = fmt.Fprintf(out, "battery: %d table(s) x %d context(s) x %d mode(s) = %d checks per side\n",
				len(tables), len(contexts), modes, len(tables)*len(contexts)*modes)

			sourceCells, err := scan.RunBattery(ctx, source, contexts, tables, probeWrites)
			if err != nil {
				return fmt.Errorf("run battery on source: %w", err)
			}
			_, _ = fmt.Fprintln(out, "  source: done")
			targetCells, err := scan.RunBattery(ctx, target, contexts, tables, probeWrites)
			if err != nil {
				return fmt.Errorf("run battery on target: %w", err)
			}
			_, _ = fmt.Fprintln(out, "  target: done")

			report := scan.DiffBattery(sourceCells, targetCells)
			if a.jsonOutput() {
				return printJSON(out, report)
			}
			writeBatteryReport(out, report)
			if !report.Equivalent() {
				cmd.SilenceUsage = true
				return fmt.Errorf("rls behaviour differs in %d check(s)", len(report.Divergences))
			}
			return nil
		},
	}

	command.Flags().StringVar(&sourceURL, "source-url", "", "The OLD database (read-only)")
	command.Flags().StringVar(&targetURL, "target-url", "", "The NEW CapyDB database")
	command.Flags().StringVar(&contextsPath, "contexts", "", "JSON file of caller identities to read as")
	command.Flags().StringVar(&tableList, "tables", "", "Comma-separated tables (default: every RLS-enabled table on the target)")
	command.Flags().BoolVar(&probeWrites, "writes", false, "Also probe write reachability (SELECT ... FOR UPDATE; writes nothing, but takes row locks)")
	return command
}

// sampleSourceConnections runs one pg_stat_activity sample over the held
// connection, folding what it finds into leftovers and returning this sample's
// count.
func sampleSourceConnections(ctx context.Context, conn *sql.Conn, query string, leftovers map[string]bool) (int, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	found := 0
	for rows.Next() {
		var connection string
		if err := rows.Scan(&connection); err != nil {
			return 0, err
		}
		if connection = strings.TrimSpace(connection); connection != "" {
			leftovers[connection] = true
			found++
		}
	}
	return found, rows.Err()
}

func writeBatteryReport(out io.Writer, report scan.BatteryReport) {
	if report.Equivalent() {
		_, _ = fmt.Fprintf(out, "\nIDENTICAL: %d checks, every table read the same as every identity on both sides\n",
			report.Checks)
		return
	}
	if len(report.Divergences) > 0 {
		_, _ = fmt.Fprintf(out, "\nDIVERGED in %d of %d check(s) (table / context: old -> new):\n",
			len(report.Divergences), report.Checks)
		for _, divergence := range report.Divergences {
			_, _ = fmt.Fprintf(out, "  %s / %s [%s]: %s -> %s\n",
				divergence.Table, divergence.Context, divergence.Mode, divergence.Source, divergence.Target)
		}
		_, _ = fmt.Fprintln(out,
			"a count that GREW is the dangerous direction - the new policies are letting that identity\n"+
				"see rows the old ones hid")
	}
	for _, table := range report.SourceOnly {
		_, _ = fmt.Fprintf(out, "  only on the old database: %s\n", table)
	}
	for _, table := range report.TargetOnly {
		_, _ = fmt.Fprintf(out, "  only on the new database: %s\n", table)
	}
}

func (a *app) newMigrateVerifyCommand() *cobra.Command {
	var sourceURL string
	var watch time.Duration
	var interval time.Duration
	var envPaths []string

	command := &cobra.Command{
		Use:   "verify",
		Short: "Verify a completed cutover: watch the OLD source for leftover consumers",
		Long: `After swapping every consumer to CapyDB, point this at the OLD database.
It samples pg_stat_activity over the watch window: zero client connections
(other than this check) means every consumer moved; anything still connecting
is a deployable you missed. Optionally validates env files for placeholder
secrets (--env-file, repeatable).

Opens ONE read-only connection for the whole watch and reuses it, so a slow
pooler costs its connect latency once rather than once per sample. Connects
only to the URL you pass.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			leftovers := map[string]bool{}

			if strings.TrimSpace(sourceURL) != "" {
				db, err := sql.Open("pgx", strings.TrimSpace(sourceURL))
				if err != nil {
					return fmt.Errorf("open source database: %w", err)
				}
				defer func() { _ = db.Close() }()
				// One connection, held for the whole watch. Each sample used to
				// be a fresh psql process, so the run cost `watch` plus a connect
				// per sample; through a slow pooler that turned a 10m watch into
				// 25m. Capping the pool at 1 also keeps this check from being the
				// leftover connection it is looking for.
				db.SetMaxOpenConns(1)
				db.SetMaxIdleConns(1)
				conn, err := db.Conn(cmd.Context())
				if err != nil {
					return fmt.Errorf("connect to source: %w", err)
				}
				defer func() { _ = conn.Close() }()
				if _, err := conn.ExecContext(cmd.Context(), "SET default_transaction_read_only = on"); err != nil {
					return fmt.Errorf("prepare read-only session: %w", err)
				}
				deadline := time.Now().Add(watch)
				_, _ = fmt.Fprintf(out, "watching source pg_stat_activity until %s (every %s, Ctrl-C for the verdict so far)\n",
					watch, interval)
				// Provider-internal services (Supabase's PostgREST, exporters,
				// admin roles, pooler auth probes) hold connections on every
				// project regardless of app consumers - filter them so the
				// verdict reflects YOUR deployables only.
				query := `SELECT usename || '|' || COALESCE(application_name,'') || '|' || COALESCE(client_addr::text,'local')
FROM pg_stat_activity
WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'
  AND COALESCE(application_name,'') NOT LIKE 'capydb_%'
  AND usename NOT IN ('supabase_admin', 'supabase_auth_admin', 'supabase_storage_admin',
                      'supabase_replication_admin', 'supabase_read_only_user',
                      'authenticator', 'pgbouncer', 'rdsadmin', 'azuresu', 'cloudsqladmin')
  AND COALESCE(application_name,'') NOT IN ('postgres_exporter', 'pg_cron scheduler')
  AND COALESCE(application_name,'') NOT LIKE 'PostgREST%'
  AND COALESCE(application_name,'') NOT LIKE 'Supavisor (auth_query)%'`
				// Bounded by WALL CLOCK, not by a sample count. A sample count
				// plus a per-sample sleep means the run takes `watch` PLUS the
				// query time of every sample - through a slow pooler that turned
				// a 10m watch into 25m of silence.
				started := time.Now()
				interrupted := false
			sampling:
				for sample := 0; ; sample++ {
					if sample > 0 {
						remaining := time.Until(deadline)
						if remaining <= 0 {
							break
						}
						wait := min(interval, remaining)
						select {
						case <-cmd.Context().Done():
							interrupted = true
							break sampling
						case <-time.After(wait):
						}
					}
					found, err := sampleSourceConnections(cmd.Context(), conn, query, leftovers)
					if err != nil {
						if cmd.Context().Err() != nil {
							interrupted = true
							break sampling
						}
						return fmt.Errorf("query source pg_stat_activity: %w", err)
					}
					// Report each sample as it lands: a watch that prints nothing
					// for minutes is indistinguishable from a hung one.
					_, _ = fmt.Fprintf(out, "  t+%-6s %d app connection(s), %d distinct so far\n",
						time.Since(started).Round(time.Second), found, len(leftovers))
					if time.Now().After(deadline) {
						break
					}
				}
				if interrupted {
					_, _ = fmt.Fprintf(out, "interrupted after %s - verdict below is from the samples taken\n",
						time.Since(started).Round(time.Second))
				}
				if len(leftovers) == 0 {
					_, _ = fmt.Fprintln(out, "source connections: none - every consumer has moved")
				} else {
					_, _ = fmt.Fprintln(out, "source connections STILL ACTIVE (user|application|address):")
					for connection := range leftovers {
						_, _ = fmt.Fprintf(out, "  - %s\n", connection)
					}
					_, _ = fmt.Fprintln(out, "a deployable is still using the old database - find and swap it before decommissioning")
				}
			}

			placeholderFindings := 0
			for _, envPath := range envPaths {
				resolved := envPath
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(a.cwd, envPath)
				}
				findings, err := findPlaceholderSecrets(resolved)
				if err != nil {
					return err
				}
				for _, finding := range findings {
					placeholderFindings++
					_, _ = fmt.Fprintf(out, "placeholder secret: %s in %s\n", finding, envPath)
				}
			}
			if len(envPaths) > 0 && placeholderFindings == 0 {
				_, _ = fmt.Fprintln(out, "env secrets: no placeholder values detected")
			}

			if len(leftovers) > 0 || placeholderFindings > 0 {
				cmd.SilenceUsage = true
				return fmt.Errorf("verify found %d leftover connection(s) and %d placeholder secret(s)", len(leftovers), placeholderFindings)
			}
			if strings.TrimSpace(sourceURL) == "" && len(envPaths) == 0 {
				return fmt.Errorf("nothing to verify: pass --source-url and/or --env-file")
			}
			_, _ = fmt.Fprintln(out, "verify: ok")
			return nil
		},
	}

	command.Flags().StringVar(&sourceURL, "source-url", "", "The OLD provider's connection string (direct endpoint)")
	command.Flags().DurationVar(&watch, "watch", 30*time.Second, "How long to watch the source for connections")
	command.Flags().DurationVar(&interval, "interval", 5*time.Second, "Sampling interval")
	command.Flags().StringArrayVar(&envPaths, "env-file", nil, "Env file(s) to check for placeholder secrets (repeatable)")
	return command
}

func findPlaceholderSecrets(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	var findings []string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if secretKeyPattern.MatchString(key) && placeholderSecretPattern.MatchString(value) {
			findings = append(findings, key)
		}
	}
	return findings, nil
}

func (a *app) newMigrateDepsCommand() *cobra.Command {
	var dir string
	var dryRun bool

	command := &cobra.Command{
		Use:   "deps",
		Short: "Swap Neon driver dependencies for postgres-js in package.json files",
		Long: `The mechanical half of the Neon driver swap: removes @neondatabase/serverless
(and @vercel/postgres) from every package.json in the tree and adds
"postgres" where one was removed. For the code half (imports, client
construction, drizzle adapter, env cleanup) run 'capydb migrate codemod neon';
'capydb migrate scan' still gives the per-repo plan.

Run your package manager's install afterwards, capture a typecheck baseline
first if the repo pins dist-tags (see scan warnings).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := a.cwd
			if strings.TrimSpace(dir) != "" {
				root = dir
			}
			out := cmd.OutOrStdout()
			changed := 0
			walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if entry.IsDir() {
					if entry.Name() == "node_modules" || entry.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}
				if entry.Name() != "package.json" {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				content := string(data)
				next, changedFile := swapNeonPackageJSON(content)
				if !changedFile {
					return nil
				}
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					relative = path
				}
				if dryRun {
					_, _ = fmt.Fprintf(out, "would update %s\n", relative)
					changed++
					return nil
				}
				if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", relative, err)
				}
				_, _ = fmt.Fprintf(out, "updated %s (removed neon driver, ensured \"postgres\")\n", relative)
				changed++
				return nil
			})
			if walkErr != nil {
				return walkErr
			}
			if changed == 0 {
				_, _ = fmt.Fprintln(out, "no package.json references @neondatabase/serverless or @vercel/postgres")
				return nil
			}
			_, _ = fmt.Fprintln(out, "next: run your package manager install, then apply the code changes from `capydb migrate scan`")
			return nil
		},
	}

	command.Flags().StringVar(&dir, "dir", "", "Directory to scan (defaults to the current directory)")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Report the files that would change without writing")
	return command
}
