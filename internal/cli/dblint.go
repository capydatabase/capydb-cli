package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

const (
	lintWarning = "warning"
	lintInfo    = "info"
)

// Dead-tuple thresholds for the bloat rule: enough dead rows to matter, and a
// large share of the table, so a busy table with healthy autovacuum is quiet.
const (
	lintBloatMinDeadTuples = 10000
	lintBloatDeadRatio     = 0.2
)

// lintFinding is one schema or index problem.
type lintFinding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Object   string `json:"object"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

type lintReport struct {
	Findings []lintFinding `json:"findings"`
	// Skipped names the checks that could not run and why.
	Skipped []string `json:"skipped"`
}

func (a *app) newDBCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "db",
		Short: "Check the database schema and indexes",
	}
	command.AddCommand(a.newDBLintCommand())
	return command
}

func (a *app) newDBLintCommand() *cobra.Command {
	var (
		exitOnFindings bool
		previewID      string
		projectRef     string
	)

	command := &cobra.Command{
		Use:   "lint",
		Short: "Find schema and index problems: missing primary keys, unindexed foreign keys, unused and duplicate indexes, bloat",
		Long: `Checks the database for problems that are cheap to fix early and expensive later:

  missing_primary_key     a table without a primary key (logical replication, follow imports,
                          and most ORMs need one to identify a row)
  unindexed_foreign_key   a foreign key whose columns do not lead any index: every delete or key
                          update on the referenced table scans this one
  duplicate_index         two indexes with the same definition - every write maintains both
  unused_index            an index with no recorded scans over the observation window
  redundant_index         an index whose columns are a leading subset of a wider index
  table_bloat             a table where dead rows are a large share of the total

Everything is read-only. Unused and redundant indexes need about a week of query statistics
before they are reported. With --preview only the schema checks run (the index and statistics
checks read the project database). --exit-code exits 1 when a warning is found, for CI.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			target, err := a.resolveSchemaTarget(ctx, client, previewID, projectRef)
			if err != nil {
				return err
			}
			schema, err := fetchTargetSchema(ctx, client, target)
			if err != nil {
				return err
			}

			report := lintReport{Findings: lintSchema(schema), Skipped: []string{}}
			if target.previewID != "" {
				report.Skipped = append(report.Skipped, "index and statistics checks read the project database and do not run against a preview")
			} else {
				findings, skipped := lintLiveDatabase(ctx, client, target.project.ID, schema)
				report.Findings = append(report.Findings, findings...)
				report.Skipped = append(report.Skipped, skipped...)
			}
			sortLintFindings(report.Findings)

			if a.jsonOutput() {
				if err := printJSON(cmd.OutOrStdout(), map[string]any{
					"findings": jsonList(report.Findings),
					"skipped":  jsonList(report.Skipped),
				}); err != nil {
					return err
				}
			} else {
				writeLintReport(cmd.OutOrStdout(), report)
			}

			if exitOnFindings && countWarnings(report.Findings) > 0 {
				cmd.SilenceUsage = true
				return exitcode.Errorf(exitcode.GenericError, "%d lint warning(s)", countWarnings(report.Findings))
			}
			return nil
		},
	}

	command.Flags().BoolVar(&exitOnFindings, "exit-code", false, "Exit with status 1 when a warning is found")
	command.Flags().StringVar(&previewID, "preview", "", "Lint a preview database's schema instead of the project database")
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

// lintSchema runs the checks the schema document alone can answer.
func lintSchema(schema api.DatabaseSchema) []lintFinding {
	findings := []lintFinding{}
	for _, namespace := range schema.Schemas {
		for _, table := range namespace.Tables {
			// Foreign tables live elsewhere; views have no keys of their own.
			if table.Kind != "table" && table.Kind != "partitioned_table" {
				continue
			}
			if len(table.PrimaryKey) > 0 {
				continue
			}
			object := namespace.Name + "." + table.Name
			message := "no primary key"
			if unique := notNullUnique(table); unique != nil {
				message += fmt.Sprintf("; the unique constraint %s on NOT NULL columns could be promoted", unique.Name)
			}
			findings = append(findings, lintFinding{
				Rule:     "missing_primary_key",
				Severity: lintWarning,
				Object:   object,
				Message:  message,
				Fix:      fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (...);", qualifiedName(namespace.Name, table.Name)),
			})
		}
	}
	return findings
}

// notNullUnique returns a unique constraint whose columns are all NOT NULL -
// a primary key in everything but name.
func notNullUnique(table api.SchemaTable) *api.SchemaUniqueConstraint {
	nullable := map[string]bool{}
	for _, column := range table.Columns {
		nullable[column.Name] = column.IsNullable
	}
	for i := range table.UniqueConstraints {
		constraint := &table.UniqueConstraints[i]
		allNotNull := len(constraint.Columns) > 0
		for _, column := range constraint.Columns {
			if nullable[column] {
				allNotNull = false
			}
		}
		if allNotNull {
			return constraint
		}
	}
	return nil
}

// lintCatalogQuery finds unindexed foreign keys, exact duplicate indexes and
// dead-tuple bloat in one read-only statement, scoped to the schemas the
// schema document covers (the same scope as every other schema tool).
//
// A foreign key is covered when its columns, in any order, are the leading
// columns of a valid, non-partial index. Duplicate indexes compare the whole
// definition: columns, operator classes, collations, sort options,
// expressions and predicate.
func lintCatalogQuery(schemas []string) string {
	quoted := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		quoted = append(quoted, quoteLiteral(schema))
	}
	scope := "ARRAY[" + strings.Join(quoted, ", ") + "]::text[]"
	return fmt.Sprintf(`SELECT 'unindexed_foreign_key' AS rule, n.nspname AS schema_name, c.relname AS table_name,
       con.conname AS object_name,
       (SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY k.ord)
          FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum) AS detail,
       0::bigint AS amount
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE con.contype = 'f'
   AND n.nspname = ANY(%[1]s)
   AND NOT EXISTS (
         SELECT 1 FROM pg_index i
          WHERE i.indrelid = con.conrelid
            AND i.indisvalid
            AND i.indpred IS NULL
            AND (i.indkey::int2[])[0:cardinality(con.conkey) - 1] @> con.conkey)
UNION ALL
SELECT 'duplicate_index', n.nspname, c.relname,
       string_agg(ic.relname, ', ' ORDER BY i.indisprimary DESC, i.indisunique DESC, ic.relname),
       NULL, count(*)
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
  JOIN pg_class c ON c.oid = i.indrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = ANY(%[1]s)
 GROUP BY n.nspname, c.relname, i.indrelid, i.indkey::text, i.indclass::text, i.indcollation::text, i.indoption::text,
          COALESCE(pg_get_expr(i.indexprs, i.indrelid), ''), COALESCE(pg_get_expr(i.indpred, i.indrelid), '')
HAVING count(*) > 1
UNION ALL
SELECT 'table_bloat', s.schemaname, s.relname, NULL,
       round(100.0 * s.n_dead_tup / NULLIF(s.n_live_tup + s.n_dead_tup, 0))::text, s.n_dead_tup
  FROM pg_stat_user_tables s
 WHERE s.schemaname = ANY(%[1]s)
   AND s.n_dead_tup >= %[2]d
   AND s.n_dead_tup > %[3]g * (s.n_live_tup + s.n_dead_tup)
 ORDER BY 1, 2, 3, 4`, scope, lintBloatMinDeadTuples, lintBloatDeadRatio)
}

// lintLiveDatabase runs the checks that need the live database: the catalog
// query (through the read-only SQL endpoint) and the index hygiene report.
func lintLiveDatabase(ctx context.Context, client *api.Client, projectID string, schema api.DatabaseSchema) ([]lintFinding, []string) {
	findings := []lintFinding{}
	skipped := []string{}

	schemas := make([]string, 0, len(schema.Schemas))
	for _, namespace := range schema.Schemas {
		schemas = append(schemas, namespace.Name)
	}
	if len(schemas) > 0 {
		result, err := client.RunSQL(ctx, projectID, lintCatalogQuery(schemas), 1000, false, true)
		if err != nil {
			skipped = append(skipped, "foreign-key index, duplicate index and bloat checks: "+err.Error())
		} else {
			findings = append(findings, lintCatalogFindings(result.Rows)...)
			if result.Truncated {
				skipped = append(skipped, "catalog checks returned more than 1000 findings; only the first 1000 are shown")
			}
		}
	}

	hygiene, err := client.GetProjectIndexHygiene(ctx, projectID)
	switch {
	case err != nil:
		skipped = append(skipped, "unused and redundant index checks: "+err.Error())
	case !hygiene.Available:
		skipped = append(skipped, "unused and redundant index checks: "+firstNonEmpty(hygiene.Reason, "not enough query statistics yet"))
	default:
		for _, index := range hygiene.UnusedIndexes {
			findings = append(findings, lintFinding{
				Rule: "unused_index", Severity: lintInfo,
				Object:  index.Schema + "." + index.Index,
				Message: fmt.Sprintf("no scans in %d days on %s; costs %s and slows every write", hygiene.ObservationWindowSeconds/86400, index.Table, formatBytes(index.SizeBytes)),
				Fix:     index.DropStatement,
			})
		}
		for _, index := range hygiene.RedundantIndexes {
			findings = append(findings, lintFinding{
				Rule: "redundant_index", Severity: lintWarning,
				Object:  index.Schema + "." + index.Index,
				Message: fmt.Sprintf("covered by %s on %s; costs %s", index.CoveredBy, index.Table, formatBytes(index.SizeBytes)),
				Fix:     index.DropStatement,
			})
		}
	}
	return findings, skipped
}

func lintCatalogFindings(rows []map[string]any) []lintFinding {
	findings := []lintFinding{}
	for _, row := range rows {
		rule := stringFromRow(row, "rule")
		schemaName := stringFromRow(row, "schema_name")
		tableName := stringFromRow(row, "table_name")
		objectName := stringFromRow(row, "object_name")
		detail := stringFromRow(row, "detail")
		table := qualifiedName(schemaName, tableName)
		switch rule {
		case "unindexed_foreign_key":
			findings = append(findings, lintFinding{
				Rule: rule, Severity: lintWarning,
				Object:  schemaName + "." + tableName + " (" + objectName + ")",
				Message: fmt.Sprintf("foreign key on (%s) has no index leading with those columns; deletes and key updates on the referenced table scan %s", detail, tableName),
				Fix:     fmt.Sprintf("CREATE INDEX CONCURRENTLY ON %s (%s);", table, detail),
			})
		case "duplicate_index":
			names := strings.Split(objectName, ", ")
			fix := ""
			if len(names) > 1 {
				// The first name is the one to keep: the query orders the
				// primary key and unique indexes (constraints) first.
				drops := make([]string, 0, len(names)-1)
				for _, name := range names[1:] {
					drops = append(drops, fmt.Sprintf("DROP INDEX CONCURRENTLY %s;", qualifiedName(schemaName, name)))
				}
				fix = strings.Join(drops, " ")
			}
			findings = append(findings, lintFinding{
				Rule: rule, Severity: lintWarning,
				Object:  schemaName + "." + tableName,
				Message: fmt.Sprintf("identical indexes: %s; keep %s", objectName, names[0]),
				Fix:     fix + " (if a dropped name backs a constraint, drop the constraint instead)",
			})
		case "table_bloat":
			findings = append(findings, lintFinding{
				Rule: rule, Severity: lintInfo,
				Object:  schemaName + "." + tableName,
				Message: fmt.Sprintf("%d dead rows, %s%% of the table; autovacuum is not keeping up", intFromRow(row, "amount"), detail),
				Fix:     fmt.Sprintf("VACUUM (ANALYZE) %s;", table),
			})
		}
	}
	return findings
}

func stringFromRow(row map[string]any, key string) string {
	switch value := row[key].(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

var lintRuleOrder = map[string]int{
	"missing_primary_key":   0,
	"unindexed_foreign_key": 1,
	"duplicate_index":       2,
	"redundant_index":       3,
	"unused_index":          4,
	"table_bloat":           5,
}

func sortLintFindings(findings []lintFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Rule != findings[j].Rule {
			return lintRuleOrder[findings[i].Rule] < lintRuleOrder[findings[j].Rule]
		}
		return findings[i].Object < findings[j].Object
	})
}

func countWarnings(findings []lintFinding) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == lintWarning {
			count++
		}
	}
	return count
}

func writeLintReport(out io.Writer, report lintReport) {
	if len(report.Findings) == 0 {
		_, _ = fmt.Fprintln(out, "No schema or index problems found.")
	}
	for _, finding := range report.Findings {
		_, _ = fmt.Fprintf(out, "[%s] %s %s: %s\n", finding.Severity, finding.Rule, finding.Object, finding.Message)
		if finding.Fix != "" {
			_, _ = fmt.Fprintf(out, "    fix: %s\n", finding.Fix)
		}
	}
	for _, skipped := range report.Skipped {
		_, _ = fmt.Fprintf(out, "[skip] %s\n", skipped)
	}
}
