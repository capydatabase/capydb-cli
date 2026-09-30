package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// The env vars `capydb env pull` writes for a project's runtime login.
// DATABASE_APP_URL takes the same pooled-or-direct choice DATABASE_URL makes
// for the detected stack; DATABASE_APP_POOL_URL is always the pooled one.
const (
	appURLVar     = "DATABASE_APP_URL"
	appPoolURLVar = "DATABASE_APP_POOL_URL"
)

func (a *app) newRolesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "roles",
		Short: "Manage a project's database login roles",
	}
	appCommand := &cobra.Command{
		Use:   "app",
		Short: "Manage the project's runtime login (app_user) for the split role model",
		Long: `The split role model gives a project a second login, app_user, for application traffic. It owns
nothing, cannot bypass row-level security and is not a member of the owner, so RLS policies apply
to everything it runs; the owner stays the login for migrations and admin jobs.

Enable it, grant it access to your tables (for example with the bundle from
` + "`capydb migrate rls --role-model split`" + `), then point the app at DATABASE_APP_POOL_URL, which
` + "`capydb env pull`" + ` writes once the role exists. Needs PostgreSQL 16 or newer.`,
	}
	appCommand.AddCommand(a.newAppRoleShowCommand())
	appCommand.AddCommand(a.newAppRoleEnableCommand())
	appCommand.AddCommand(a.newAppRoleRotateCommand())
	command.AddCommand(appCommand)
	return command
}

func (a *app) newAppRoleShowCommand() *cobra.Command {
	var projectRef string
	command := &cobra.Command{
		Use:   "show",
		Short: "Show whether the project has its runtime login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			status, err := client.GetAppRole(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("get app role: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"app_role": status})
			}
			writeAppRoleStatus(cmd.OutOrStdout(), status)
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func writeAppRoleStatus(out io.Writer, status api.AppRoleStatus) {
	_, _ = fmt.Fprintf(out, "enabled: %s\n", yesNo(status.Enabled))
	if status.Enabled {
		_, _ = fmt.Fprintf(out, "username: %s\n", firstNonEmpty(status.Username, "app_user"))
		if status.CreatedAt != nil {
			_, _ = fmt.Fprintf(out, "created_at: %s\n", formatTime(*status.CreatedAt))
		}
		if status.RotatedAt != nil {
			_, _ = fmt.Fprintf(out, "rotated_at: %s\n", formatTime(*status.RotatedAt))
		}
		_, _ = fmt.Fprintf(out, "Run `capydb env pull` to write %s and %s.\n", appURLVar, appPoolURLVar)
		return
	}
	if status.Available {
		_, _ = fmt.Fprintln(out, "Enable it with `capydb roles app enable`.")
		return
	}
	_, _ = fmt.Fprintln(out, "The split role model is not offered for new opt-ins right now.")
}

func (a *app) newAppRoleEnableCommand() *cobra.Command {
	var projectRef string
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "enable",
		Short: "Create the project's runtime login (app_user)",
		Long: "Queues the job that creates app_user on the project's database. Its connection strings " +
			"appear once the job completes; run `capydb env pull` then to write " + appURLVar + " and " + appPoolURLVar + ".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			// The status says up front what enabling would otherwise answer
			// with a bare 404 (not offered) or 409 (already there).
			status, err := client.GetAppRole(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("get app role: %w", err)
			}
			if status.Enabled {
				if a.jsonOutput() {
					return printJSON(cmd.OutOrStdout(), map[string]any{"app_role": status})
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s already has its runtime login (%s).\n", project.Name, firstNonEmpty(status.Username, "app_user"))
				return nil
			}
			if !status.Available {
				return notFoundErrorf("the split role model is not offered for new opt-ins right now; project %s was not changed", project.Name)
			}

			job, err := client.EnableAppRole(ctx, project.ID)
			if err != nil {
				return appRoleError("enable app role", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued app role creation for project %s\n", project.Name)
			}
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "app role creation"); err != nil {
				return err
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Once it completes, run `capydb env pull` to write %s and %s.\n", appURLVar, appPoolURLVar)
			}
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(command, &wait, &waitTimeout, "app role creation")
	return command
}

func (a *app) newAppRoleRotateCommand() *cobra.Command {
	var projectRef string
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "rotate",
		Short: "Replace the runtime login's password",
		Long: "Queues the job that replaces app_user's password in place. The old password stops working when " +
			"the job completes, so update the app's " + appURLVar + "/" + appPoolURLVar + " right after (`capydb env pull`, " +
			"or your deployment platform's env settings).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			job, err := client.RotateAppRole(ctx, project.ID)
			if err != nil {
				return appRoleError("rotate app role", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued app role password rotation for project %s\n", project.Name)
			}
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "app role rotation"); err != nil {
				return err
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "The old password stops working when the job completes; run `capydb env pull` to refresh %s and %s.\n", appURLVar, appPoolURLVar)
			}
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(command, &wait, &waitTimeout, "app role rotation")
	return command
}

// appRoleError names what a refused app-role call means: 404 is the platform
// not offering the role (or, on rotate, a project without one), 409 a
// conflicting job or state.
func appRoleError(action string, err error) error {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		switch apiErr.StatusCode {
		case http.StatusNotFound:
			return notFoundErrorf("%s: %v (the split role model is not offered here, or the project has no runtime login - see `capydb roles app show`)", action, err)
		case http.StatusConflict:
			return fmt.Errorf("%s: %w (a role job may already be running; check `capydb jobs list`)", action, err)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// appRoleEnvVars are the runtime login's env vars for a project whose
// connections carry one; nil otherwise. databaseURL is the value the env plan
// chose for DATABASE_URL, so DATABASE_APP_URL makes the same pooled-or-direct
// choice for the stack.
func appRoleEnvVars(connections api.ProjectConnectionInfo, databaseURL string) map[string]string {
	app := connections.App
	if app == nil {
		return nil
	}
	vars := map[string]string{}
	appURL := firstNonEmpty(app.DirectURL, app.PooledURL)
	if databaseURL != "" && databaseURL == connections.PooledURL {
		appURL = firstNonEmpty(app.PooledURL, app.DirectURL)
	}
	if appURL != "" {
		vars[appURLVar] = appURL
	}
	if app.PooledURL != "" {
		vars[appPoolURLVar] = app.PooledURL
	}
	return vars
}
