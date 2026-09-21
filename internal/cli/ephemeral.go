package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/capydatabase/capydbclient"
	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/envfile"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
	"github.com/capydatabase/capydb-cli/internal/gitignore"
	"github.com/capydatabase/capydb-cli/internal/project"
)

const (
	// Provisioning is normally seconds; the generous ceiling covers a busy job
	// queue without leaving a script hanging on a database that failed.
	defaultEphemeralWaitTimeout = 5 * time.Minute
	ephemeralPollInterval       = 2 * time.Second
)

var errEphemeralFailed = errors.New("the ephemeral database failed to come up; run `capydb ephemeral create` again")

// newEphemeralCommand is the account-less path: `create` makes no login and
// sends no credential. The claim token it gets back is kept in
// .capydb/ephemeral.json so `status` and `claim` need no arguments.
func (a *app) newEphemeralCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "ephemeral",
		Short: "Spin up a throwaway database with no account, and claim it if you want to keep it",
		Long: "An ephemeral database is a real Postgres database created without signing up.\n" +
			"It is destroyed, with its data, 72 hours after creation - unless you claim it\n" +
			"into your CapyDB organization first, which turns it into a normal project.",
	}

	command.AddCommand(a.newEphemeralCreateCommand())
	command.AddCommand(a.newEphemeralStatusCommand())
	command.AddCommand(a.newEphemeralClaimCommand())
	return command
}

func (a *app) newEphemeralCreateCommand() *cobra.Command {
	var envFileOverride string
	var name string
	var noEnv bool
	var overwriteEnv bool
	var postgresVersion string
	var region string
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "create",
		Short: "Create an ephemeral database and write its connection string to the env file",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Same contract as `capydb create`: in JSON mode stdout is one
			// document and the narration goes to stderr.
			progress := cmd.OutOrStdout()
			if a.jsonOutput() {
				progress = cmd.ErrOrStderr()
			}

			if existing, err := config.LoadEphemeralState(a.cwd); err == nil && existing.ExpiresAt.After(time.Now()) {
				return usageErrorf("this directory already has an ephemeral database (%s, expires %s); run `capydb ephemeral status`, or delete %s to start another",
					existing.Name, formatTime(existing.ExpiresAt), filepath.Join(".capydb", "ephemeral.json"))
			}

			var detection project.Detection
			if !noEnv {
				detected, err := a.detectProject(envFileOverride)
				if err != nil {
					return err
				}
				detection = detected
			}

			apiURL := a.resolveAPIURL("")
			client, err := a.newAPIClient(apiURL, "")
			if err != nil {
				return err
			}

			created, err := client.CreateEphemeralDatabase(ctx, api.EphemeralDatabaseCreateRequest{
				Name:            strings.TrimSpace(name),
				PostgresVersion: strings.TrimSpace(postgresVersion),
				Region:          strings.TrimSpace(region),
			})
			if err != nil {
				// The create answers 404 only when the deployment has the feature off.
				if capydbclient.IsNotFound(err) {
					return notFoundErrorf("ephemeral databases are not enabled on %s; run `capydb create` for a database that stays", apiURL)
				}
				return fmt.Errorf("create ephemeral database: %w", err)
			}
			database := created.EphemeralDatabase

			// Persist the claim token before anything else can fail: it is shown
			// once, and without it the database can be neither read nor claimed.
			state := config.EphemeralState{
				APIURL:     apiURL,
				ClaimToken: created.ClaimToken,
				ClaimURL:   created.ClaimURL,
				ExpiresAt:  database.ExpiresAt,
				Name:       database.Name,
				ProjectID:  database.ProjectID,
			}
			if err := config.SaveEphemeralState(a.cwd, state); err != nil {
				return err
			}
			if err := gitignore.EnsureLocalConfigIgnored(a.cwd); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(progress, "Created ephemeral database %s; waiting for it to come up\n", database.Name)
			details, err := waitForEphemeralDatabase(ctx, client, database.ProjectID, created.ClaimToken, waitTimeout)
			if err != nil {
				if errors.Is(err, errEphemeralFailed) {
					// Nothing to read or claim; do not let the record block the retry.
					if removeErr := config.RemoveEphemeralState(a.cwd); removeErr != nil {
						return errors.Join(err, removeErr)
					}
				}
				return err
			}

			// The claim link embeds the claim token, so it stays in
			// .capydb/ephemeral.json; `status --claim-url` prints it on request.
			summary := map[string]any{
				"ephemeral_database": details.EphemeralDatabase,
			}
			if noEnv {
				// Explicit opt-out of the env file is the one case where the
				// connection strings are printed: there is nowhere else for them to go.
				summary["connections"] = details.Connections
			} else {
				plan := project.BuildEnvPlan(detection, details.Connections.DirectURL, details.Connections.PooledURL)
				var resolver envfile.ConflictResolver
				if !overwriteEnv {
					resolver = a.envOverwriteResolver(cmd)
				}
				envAbsPath := envTargetPath(a.cwd, detection.AppPath, detection.EnvFile)
				if err := envfile.UpsertWithResolver(envAbsPath, plan.Vars, resolver); err != nil {
					return err
				}
				if err := gitignore.EnsureLocalConfigIgnored(a.cwd, envIgnoreEntry(detection.AppPath, detection.EnvFile)); err != nil {
					return err
				}
				state.EnvFile = detection.EnvFile
				if err := config.SaveEphemeralState(a.cwd, state); err != nil {
					return err
				}
				warnEnvShadowing(cmd, a.cwd, detection.EnvFile)

				envVars := make([]string, 0, len(plan.Vars))
				for key := range plan.Vars {
					envVars = append(envVars, key)
				}
				sort.Strings(envVars)
				summary["env_file"] = firstNonEmpty(filepath.Join(detection.AppPath, detection.EnvFile), detection.EnvFile)
				summary["env_vars"] = envVars
			}

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), summary)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Ephemeral database %s is ready (%s, Postgres %s)\n", details.EphemeralDatabase.Name, details.EphemeralDatabase.Region, details.EphemeralDatabase.PostgresVersion)
			if noEnv {
				_, _ = fmt.Fprintf(out, "Pooled URL: %s\n", details.Connections.PooledURL)
				_, _ = fmt.Fprintf(out, "Direct URL: %s\n", details.Connections.DirectURL)
			} else {
				_, _ = fmt.Fprintf(out, "Env file updated: %s\n", summary["env_file"])
			}
			writeEphemeralLifetime(out, details.EphemeralDatabase.ExpiresAt)
			_, _ = fmt.Fprintln(out, "Keep it: run `capydb ephemeral claim` (or `capydb ephemeral status --claim-url` for a link to open in a browser).")
			return nil
		},
	}

	command.Flags().StringVar(&envFileOverride, "env-file", "", "Env file to update")
	command.Flags().BoolVar(&overwriteEnv, "overwrite-env", false, "Overwrite an existing DATABASE_URL (and related vars) in the env file without prompting")
	command.Flags().BoolVar(&noEnv, "no-env", false, "Do not touch an env file; print the connection strings instead")
	command.Flags().StringVar(&name, "name", "", "Display name for the database")
	command.Flags().StringVar(&region, "region", "", "Region slug")
	command.Flags().StringVar(&postgresVersion, "postgres-version", "", "Postgres major version: 16, 17, or 18 (default: platform default)")
	command.Flags().DurationVar(&waitTimeout, "wait-timeout", defaultEphemeralWaitTimeout, "How long to wait for the database to come up")
	return command
}

func (a *app) newEphemeralStatusCommand() *cobra.Command {
	var showClaimURL bool

	command := &cobra.Command{
		Use:   "status",
		Short: "Show this directory's ephemeral database and how long it has left",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := a.loadEphemeralState()
			if err != nil {
				return err
			}
			client, err := a.newAPIClient(state.APIURL, "")
			if err != nil {
				return err
			}

			details, err := client.GetEphemeralDatabase(cmd.Context(), state.ProjectID, state.ClaimToken)
			if err != nil {
				return ephemeralGoneError(err, state)
			}

			if a.jsonOutput() {
				summary := map[string]any{"ephemeral_database": details.EphemeralDatabase}
				if showClaimURL {
					summary["claim_url"] = state.ClaimURL
				}
				return printJSON(cmd.OutOrStdout(), summary)
			}

			out := cmd.OutOrStdout()
			database := details.EphemeralDatabase
			_, _ = fmt.Fprintf(out, "Ephemeral database %s: %s (%s, Postgres %s)\n", database.Name, database.State, database.Region, firstNonEmpty(database.PostgresVersion, "-"))
			writeEphemeralLifetime(out, database.ExpiresAt)
			if showClaimURL {
				_, _ = fmt.Fprintf(out, "Claim link: %s\n", state.ClaimURL)
			} else {
				_, _ = fmt.Fprintln(out, "Keep it: run `capydb ephemeral claim` (or `capydb ephemeral status --claim-url` for a link to open in a browser).")
			}
			return nil
		},
	}

	// The claim link embeds the claim token, so it is opt-in rather than part of
	// every status line that might end up in a transcript or a CI log.
	command.Flags().BoolVar(&showClaimURL, "claim-url", false, "Print the claim link (it embeds the secret claim token)")
	return command
}

func (a *app) newEphemeralClaimCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "claim",
		Short: "Keep this directory's ephemeral database: attach it to your organization as a project",
		Long: "Claiming moves the ephemeral database into your CapyDB organization. It stops\n" +
			"expiring, gains nightly backups, and counts as a project on your plan. The data\n" +
			"and the connection strings do not change, so the app keeps working as it is.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			state, err := a.loadEphemeralState()
			if err != nil {
				return err
			}

			authConfig, err := a.resolveAuthOrLogin(cmd, state.APIURL)
			if err != nil {
				return err
			}
			client, err := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
			if err != nil {
				return err
			}

			claimed, err := client.ClaimEphemeralDatabase(ctx, state.ProjectID, state.ClaimToken)
			if err != nil {
				if capydbclient.IsNotFound(err) {
					return ephemeralGoneError(err, state)
				}
				return fmt.Errorf("claim ephemeral database: %w", err)
			}

			// From here it is a project like any other: link the directory so the
			// rest of the CLI works on it. With an env file on record the link goes
			// through the usual writer (the values it rewrites are unchanged).
			linkConfig := config.ProjectConfig{
				APIURL:         authConfig.APIURL,
				EnvFile:        state.EnvFile,
				OrganizationID: claimed.OrganizationID,
				ProjectID:      claimed.ID,
				ProjectName:    claimed.Name,
				ProjectSlug:    claimed.Slug,
				Region:         claimed.Region,
			}
			if state.EnvFile != "" {
				if detection, err := a.detectProject(state.EnvFile); err == nil {
					linkConfig.AppPath = detection.AppPath
					linkConfig.DatabaseLayer = detection.DatabaseLayer
					linkConfig.Framework = detection.Framework
					linkConfig.Profile = detection.Profile
				}
				if err := a.writeProjectEnv(cmd, client, claimed.ID, linkConfig, state.EnvFile, false, true); err != nil {
					return err
				}
			} else if err := config.SaveProjectConfig(a.cwd, linkConfig); err != nil {
				return err
			}
			if err := config.RemoveEphemeralState(a.cwd); err != nil {
				return err
			}

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"project": claimed})
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Claimed %s into %s. It is now project %s and no longer expires.\n",
				claimed.Name, firstNonEmpty(authConfig.OrganizationName, authConfig.OrganizationSlug, claimed.OrganizationID), claimed.ID)
			_, _ = fmt.Fprintln(out, "This directory is linked to it. The connection strings did not change.")
			return nil
		},
	}
	return command
}

func (a *app) loadEphemeralState() (config.EphemeralState, error) {
	state, err := config.LoadEphemeralState(a.cwd)
	if errors.Is(err, os.ErrNotExist) {
		return config.EphemeralState{}, notFoundErrorf("no ephemeral database is recorded for this directory; run `capydb ephemeral create`")
	}
	return state, err
}

// ephemeralGoneError explains the one not-found that matters here: the API
// answers 404 for a database that expired or was already claimed, and the claim
// token stops working at that moment by design.
func ephemeralGoneError(err error, state config.EphemeralState) error {
	if !capydbclient.IsNotFound(err) {
		return fmt.Errorf("fetch ephemeral database: %w", err)
	}
	if !state.ExpiresAt.After(time.Now()) {
		return notFoundErrorf("ephemeral database %s expired on %s and has been destroyed; delete %s and run `capydb ephemeral create` for a new one",
			state.Name, formatTime(state.ExpiresAt), filepath.Join(".capydb", "ephemeral.json"))
	}
	return notFoundErrorf("ephemeral database %s is no longer ephemeral: it was claimed (or removed). If you claimed it in the browser, run `capydb link` to link this directory to the project", state.Name)
}

func writeEphemeralLifetime(out io.Writer, expiresAt time.Time) {
	remaining := time.Until(expiresAt).Round(time.Minute)
	if remaining <= 0 {
		_, _ = fmt.Fprintf(out, "Expired: %s\n", formatTime(expiresAt))
		return
	}
	_, _ = fmt.Fprintf(out, "Expires: %s (in %s) - it and its data are destroyed then unless claimed\n", formatTime(expiresAt), remaining)
}

// waitForEphemeralDatabase polls the anonymous read until the database is
// ready. /v1/jobs needs an account, so the claim-token read is the only way to
// follow provisioning here.
func waitForEphemeralDatabase(ctx context.Context, client *api.Client, projectID, claimToken string, timeout time.Duration) (api.EphemeralDatabaseDetails, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(ephemeralPollInterval)
	defer ticker.Stop()

	for {
		details, err := client.GetEphemeralDatabase(ctx, projectID, claimToken)
		if err != nil {
			return api.EphemeralDatabaseDetails{}, fmt.Errorf("fetch ephemeral database: %w", err)
		}
		switch details.EphemeralDatabase.State {
		case "ready":
			return details, nil
		case "failed":
			return api.EphemeralDatabaseDetails{}, errEphemeralFailed
		}

		if time.Now().After(deadline) {
			return api.EphemeralDatabaseDetails{}, exitcode.Errorf(exitcode.Timeout,
				"timed out after %s waiting for the ephemeral database; it may still come up - check `capydb ephemeral status`", timeout)
		}
		select {
		case <-ctx.Done():
			return api.EphemeralDatabaseDetails{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
