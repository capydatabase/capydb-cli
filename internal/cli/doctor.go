package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/configlint"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
	"github.com/capydatabase/capydb-cli/internal/scan"
)

const (
	doctorPass = "pass"
	doctorFail = "fail"
	doctorSkip = "skip"
)

// doctorCheck is the result of one environment diagnostic.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func (a *app) newDoctorCommand() *cobra.Command {
	var fix, yes bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local CapyDB CLI environment",
		Long: "Checks the saved config, API reachability, authentication, the local project link and its env vars, psql availability, " +
			"env files that point the same keys at different databases, database configuration in the repo, and the live migration history. " +
			"Exits non-zero when any check fails.\n\n" +
			"--fix applies the fixes that are mechanical before checking: missing database env vars are added to the linked env file, " +
			"drizzle-kit gets schemaFilter and the direct URL, Prisma gets directUrl - nothing existing is overwritten. " +
			"Fixes that remove something (a link to a deleted project, database vars in other env files) are asked one by one; " +
			"--yes applies them without asking, and without a terminal they are skipped. The rest is listed as manual.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if yes && !fix {
				return usageErrorf("--yes only applies with --fix")
			}
			var fixes []doctorFix
			if fix {
				applied, err := a.runDoctorFixes(cmd, a.newFixConfirmer(cmd, yes))
				if err != nil {
					return err
				}
				fixes = applied
			}
			return a.runDoctor(cmd, fixes)
		},
	}
	command.Flags().BoolVar(&fix, "fix", false, "Apply the mechanical fixes, then check")
	command.Flags().BoolVar(&yes, "yes", false, "With --fix: also apply fixes that remove something, without asking")
	return command
}

func (a *app) runDoctor(cmd *cobra.Command, fixes []doctorFix) error {
	ctx := cmd.Context()
	checks := make([]doctorCheck, 0, 5)

	// 1. Config readability.
	configPath, configErr := config.UserConfigPath()
	if configErr == nil {
		_, configErr = config.LoadUserConfig()
	}
	if configErr != nil {
		checks = append(checks, doctorCheck{Name: "config", Status: doctorFail, Detail: configErr.Error()})
	} else {
		checks = append(checks, doctorCheck{Name: "config", Status: doctorPass, Detail: configPath})
	}

	// 2. API reachability via the unauthenticated public status endpoint.
	apiURL := a.resolveAPIURL("")
	anonymousClient, err := a.newAPIClient(apiURL, "")
	if err != nil {
		checks = append(checks, doctorCheck{Name: "api", Status: doctorFail, Detail: err.Error()})
	} else if publicStatus, statusErr := anonymousClient.GetPublicStatus(ctx); statusErr != nil {
		checks = append(checks, doctorCheck{Name: "api", Status: doctorFail, Detail: fmt.Sprintf("%s unreachable: %v", apiURL, statusErr)})
	} else {
		checks = append(checks, doctorCheck{Name: "api", Status: doctorPass, Detail: fmt.Sprintf("%s (%s)", apiURL, firstNonEmpty(publicStatus.Status, "unknown"))})
	}

	// 3. Auth validity via the viewer endpoint.
	authValid := false
	authConfig, authErr := a.resolveAuth(false)
	switch {
	case authErr != nil:
		checks = append(checks, doctorCheck{Name: "auth", Status: doctorFail, Detail: authErr.Error()})
	default:
		client, clientErr := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
		if clientErr != nil {
			checks = append(checks, doctorCheck{Name: "auth", Status: doctorFail, Detail: clientErr.Error()})
			break
		}
		viewer, viewerErr := client.GetViewer(ctx)
		if viewerErr != nil {
			checks = append(checks, doctorCheck{Name: "auth", Status: doctorFail, Detail: fmt.Sprintf("api key rejected: %v", viewerErr)})
			break
		}
		authValid = true
		detail := "api key accepted"
		if viewer.Organization != nil {
			detail = fmt.Sprintf("api key accepted for organization %s", firstNonEmpty(viewer.Organization.Name, viewer.Organization.ID))
		}
		checks = append(checks, doctorCheck{Name: "auth", Status: doctorPass, Detail: detail})
	}

	// 4. Local project link validity.
	linkConfig, linkErr := config.LoadProjectConfig(a.cwd)
	switch {
	case errors.Is(linkErr, os.ErrNotExist):
		checks = append(checks, doctorCheck{Name: "project_link", Status: doctorSkip, Detail: "no local project link (run `capydb link` to create one)"})
	case linkErr != nil:
		checks = append(checks, doctorCheck{Name: "project_link", Status: doctorFail, Detail: linkErr.Error()})
	case strings.TrimSpace(linkConfig.ProjectID) == "":
		checks = append(checks, doctorCheck{Name: "project_link", Status: doctorFail, Detail: "project link has no project_id; run `capydb link` again"})
	case !authValid:
		checks = append(checks, doctorCheck{Name: "project_link", Status: doctorSkip, Detail: fmt.Sprintf("linked to %s, but auth is unavailable to verify it", linkConfig.ProjectID)})
	default:
		client, clientErr := a.newAPIClient(a.resolveAPIURL(linkConfig.APIURL), authConfig.APIKey)
		if clientErr != nil {
			checks = append(checks, doctorCheck{Name: "project_link", Status: doctorFail, Detail: clientErr.Error()})
			break
		}
		project, _, projectErr := client.GetProject(ctx, linkConfig.ProjectID)
		if projectErr != nil {
			checks = append(checks, doctorCheck{Name: "project_link", Status: doctorFail, Detail: fmt.Sprintf("linked project %s: %v", linkConfig.ProjectID, projectErr)})
			break
		}
		checks = append(checks, doctorCheck{Name: "project_link", Status: doctorPass, Detail: fmt.Sprintf("linked to %s (%s)", project.Name, project.ID)})
	}

	// 4b. The linked env file carries the database variables the link wrote.
	if linkErr == nil && strings.TrimSpace(linkConfig.ProjectID) != "" {
		_, envFile := linkedEnvFile(a.cwd, linkConfig)
		switch missing, err := missingLinkedEnvVars(a.cwd, linkConfig); {
		case err != nil:
			checks = append(checks, doctorCheck{Name: "env_vars", Status: doctorSkip, Detail: err.Error()})
		case len(missing) > 0:
			checks = append(checks, doctorCheck{Name: "env_vars", Status: doctorFail, Detail: fmt.Sprintf("%s is missing %s; run `capydb env pull` or `capydb doctor --fix`", envFile, strings.Join(missing, ", "))})
		default:
			checks = append(checks, doctorCheck{Name: "env_vars", Status: doctorPass, Detail: envFile + " has the database variables"})
		}
	}

	// 5. psql on PATH.
	if psqlPath, lookErr := exec.LookPath("psql"); lookErr != nil {
		checks = append(checks, doctorCheck{Name: "psql", Status: doctorFail, Detail: "psql not found in PATH; install the Postgres client tools (e.g. `brew install libpq` or `apt install postgresql-client`)"})
	} else {
		checks = append(checks, doctorCheck{Name: "psql", Status: doctorPass, Detail: psqlPath})
	}

	// 6. Env shadowing: the same key pointing at two databases across env files.
	// A half-migrated repo is otherwise silent - the app reads one file, tools
	// that pin a path read another - so this belongs in a health check, not
	// only in the one-shot migration scan.
	if conflicts, conflictErr := scan.DetectEnvConflicts(a.cwd); conflictErr != nil {
		checks = append(checks, doctorCheck{Name: "env_shadowing", Status: doctorSkip, Detail: conflictErr.Error()})
	} else if len(conflicts) == 0 {
		checks = append(checks, doctorCheck{Name: "env_shadowing", Status: doctorPass, Detail: "no env key points at more than one database"})
	} else {
		details := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			details = append(details, conflict.Describe())
		}
		checks = append(checks, doctorCheck{Name: "env_shadowing", Status: doctorFail, Detail: strings.Join(details, "; ")})
	}

	// 7. Database configuration across every stack in the repo. Of the things
	// unique to CapyDB, only cold-start wake latency is a runtime concern -
	// which URL migrations use, prepared statements through the pooler, and
	// client pool sizing are all CONFIGURATION, readable from disk. That makes
	// one linter cover Drizzle, Prisma, Rails, Django and raw postgres.js
	// instead of a per-language wrapper covering one, and it cannot break
	// anyone's runtime because it never executes their code.
	if findings, lintErr := configlint.Run(a.cwd); lintErr != nil {
		checks = append(checks, doctorCheck{Name: "db_config", Status: doctorSkip, Detail: lintErr.Error()})
	} else if len(findings) == 0 {
		checks = append(checks, doctorCheck{Name: "db_config", Status: doctorPass, Detail: "no connection or migration misconfiguration found"})
	} else {
		errorCount := 0
		details := make([]string, 0, len(findings))
		for _, finding := range findings {
			if finding.Severity == configlint.SeverityError {
				errorCount++
			}
			location := finding.File
			if finding.Line > 0 {
				location = fmt.Sprintf("%s:%d", finding.File, finding.Line)
			}
			detail := fmt.Sprintf("%s [%s] %s", location, finding.Rule, finding.Message)
			if finding.Fix != "" {
				detail += " -> " + finding.Fix
			}
			details = append(details, detail)
		}
		// Warnings alone must not fail the command: `doctor` is run in CI by
		// people who cannot act on every advisory finding immediately.
		status := doctorPass
		if errorCount > 0 {
			status = doctorFail
		}
		checks = append(checks, doctorCheck{Name: "db_config", Status: status, Detail: strings.Join(details, "; ")})
	}

	// 8. Migration history against the LIVE database. This is the one check a
	// file-only linter cannot make: the repo knows how many migrations exist,
	// but only the database knows how many were ever applied. A schema applied
	// with `push` records nothing, so the first `migrate` replays from the first
	// migration into a populated database and fails on existing objects.
	// Needs a linked project and working auth; degrades to skip otherwise.
	local := configlint.DetectLocalMigrations(a.cwd)
	switch {
	case local.Tool == "":
		// no migrations on disk - nothing to compare
	case strings.TrimSpace(linkConfig.ProjectID) == "" || !authValid:
		checks = append(checks, doctorCheck{Name: "migration_history", Status: doctorSkip, Detail: "needs a linked project and authentication to inspect the live database"})
	default:
		client, clientErr := a.newAPIClient(a.resolveAPIURL(linkConfig.APIURL), authConfig.APIKey)
		if clientErr != nil {
			checks = append(checks, doctorCheck{Name: "migration_history", Status: doctorSkip, Detail: clientErr.Error()})
			break
		}
		query := configlint.MigrationStateQuery(local.Tool)
		// Read-only introspection, so it runs under the server-enforced READ
		// ONLY transaction and is never a candidate for the unqualified-write
		// opt-out.
		result, sqlErr := client.RunSQL(ctx, linkConfig.ProjectID, query, 1, false, true)
		switch {
		case sqlErr != nil:
			// A paused cell, a permissions quirk, or a missing migrations table
			// all land here; none is worth failing the command over.
			checks = append(checks, doctorCheck{Name: "migration_history", Status: doctorSkip, Detail: "could not read migration state: " + sqlErr.Error()})
		case len(result.Rows) == 0:
			checks = append(checks, doctorCheck{Name: "migration_history", Status: doctorSkip, Detail: "live database returned no migration state"})
		default:
			applied := intFromRow(result.Rows[0], "applied")
			tables := intFromRow(result.Rows[0], "user_tables")
			if finding := configlint.EvaluateMigrationState(local, applied, tables); finding != nil {
				checks = append(checks, doctorCheck{
					Name:   "migration_history",
					Status: doctorFail,
					Detail: fmt.Sprintf("%s [%s] %s -> %s", finding.File, finding.Rule, finding.Message, finding.Fix),
				})
			} else {
				checks = append(checks, doctorCheck{
					Name:   "migration_history",
					Status: doctorPass,
					Detail: fmt.Sprintf("%s: %d local migrations, %d applied on the live database", local.Tool, local.Count, applied),
				})
			}
		}
	}

	failed := 0
	for _, check := range checks {
		if check.Status == doctorFail {
			failed++
		}
	}

	if a.jsonOutput() {
		payload := map[string]any{
			"checks": jsonList(checks),
			"ok":     failed == 0,
		}
		if fixes != nil {
			payload["fixes"] = jsonList(fixes)
		}
		if err := printJSON(cmd.OutOrStdout(), payload); err != nil {
			return err
		}
	} else {
		for _, fix := range fixes {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s: %s\n", fix.Status, fix.Name, fix.Detail)
		}
		switch {
		case len(fixes) > 0:
			_, _ = fmt.Fprintln(cmd.OutOrStdout())
		case fixes != nil:
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "--fix: nothing to fix.")
		}
		for _, check := range checks {
			if strings.TrimSpace(check.Detail) != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s: %s\n", check.Status, check.Name, check.Detail)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s\n", check.Status, check.Name)
			}
		}
	}

	if failed > 0 {
		return exitcode.Errorf(exitcode.GenericError, "%d of %d checks failed", failed, len(checks))
	}
	if !a.jsonOutput() {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "All checks passed.")
	}
	return nil
}

// intFromRow reads a count column out of a SQL runner row, tolerating the
// numeric shapes JSON decoding can produce (float64, json.Number, string).
func intFromRow(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
