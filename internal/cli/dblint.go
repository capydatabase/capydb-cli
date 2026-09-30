package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

// lintWarning is the finding severity --exit-code fails on; the other is
// "info".
const lintWarning = "warning"

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

The checks run server-side as catalog queries in a read-only transaction and read no table data,
so an API key with the schema:read scope is enough - the same scope type generation needs.
Unused and redundant indexes need about a week of query statistics before they are reported and
are skipped for a preview; a check that cannot run is listed as skipped, with the reason.
--exit-code exits 1 when a warning is found, for CI.`,
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
			var report api.LintReport
			if target.previewID != "" {
				report, err = client.LintPreview(ctx, target.previewID)
			} else {
				report, err = client.LintProject(ctx, target.project.ID)
			}
			if err != nil {
				return fmt.Errorf("lint: %w", err)
			}

			if a.jsonOutput() {
				if err := printJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				writeLintReport(cmd.OutOrStdout(), report)
			}

			if warnings := countWarnings(report.Findings); exitOnFindings && warnings > 0 {
				cmd.SilenceUsage = true
				return exitcode.Errorf(exitcode.GenericError, "%d lint warning(s)", warnings)
			}
			return nil
		},
	}

	command.Flags().BoolVar(&exitOnFindings, "exit-code", false, "Exit with status 1 when a warning is found")
	command.Flags().StringVar(&previewID, "preview", "", "Lint a preview database instead of the project database")
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func countWarnings(findings []api.LintFinding) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == lintWarning {
			count++
		}
	}
	return count
}

func writeLintReport(out io.Writer, report api.LintReport) {
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
