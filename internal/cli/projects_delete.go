package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

// newProjectsDeleteCommand deletes a project: its database, previews, and
// backups. Two independent gates guard it. The CLI's own typed-name
// confirmation (or --confirm) protects against a slip at the keyboard; the
// control plane's approval protects production projects against the caller
// itself - an API key cannot approve its own delete, so a person creates the
// approval in the dashboard and the CLI only presents it.
//
// The project is a required argument rather than falling back to the linked
// project: a delete should never be aimed by whichever directory the shell
// happens to be in.
func (a *app) newProjectsDeleteCommand() *cobra.Command {
	var approvalToken string
	var confirmFlag bool
	var yes bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "delete <project>",
		Short: "Delete a project and its database, previews, and backups",
		Long: `Deletes a project: its database, preview databases, and backups. This cannot be undone.

The project is named explicitly by id, slug, or name - the linked project is never used implicitly.
Confirm by retyping the project name, or pass --confirm in scripts.

A production project also needs an approval from an organization admin: open the project's
settings page in the dashboard, create a delete approval, and pass it with --approval-token
(or CAPYDB_APPROVAL_TOKEN). The approval works once, for 10 minutes. Non-production projects
need only the confirmation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ref := strings.TrimSpace(args[0])
			if ref == "" {
				return usageErrorf("a project id, slug, or name is required")
			}

			client, authConfig, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, ref)
			if err != nil {
				return err
			}

			// Checked before the confirmation prompt, so nobody retypes a
			// project name only to learn the delete cannot proceed.
			// Anything but an explicit non_production label is treated as
			// production: the control plane's default environment is
			// production, so an unlabelled project needs the approval too.
			token := resolveApprovalToken(approvalToken)
			if project.Environment != "non_production" && token == "" {
				settingsURL, urlErr := buildDashboardURL(a.resolveAppURL(authConfig.APIURL), lookupWorkspaceSlug(ctx, client), project.Slug, project.ID, "settings")
				if urlErr != nil {
					settingsURL = "the project's settings page in the dashboard"
				}
				return fmt.Errorf("deleting production project %s needs an approval from an organization admin: open %s, create a delete approval, and re-run with --approval-token <token> (or set CAPYDB_APPROVAL_TOKEN); the token works once, for 10 minutes. Nothing was deleted", project.Name, settingsURL)
			}

			confirmed, err := confirmProjectDestructiveAction(cmd, project, confirmFlag || yes,
				"This will DELETE project %q (%s): its database, preview databases, and backups. This cannot be undone.\n")
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("deletion not confirmed; pass --confirm or confirm interactively")
			}

			job, err := client.DeleteProject(ctx, project.ID, token)
			if err != nil {
				return fmt.Errorf("delete project: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued deletion job %s for project %s (%s)\n", job.ID, project.Name, project.ID)
			}
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "project deletion"); err != nil {
				return err
			}
			if wait && job.ID != "" && !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s is deleted.\n", project.Name)
			}
			if linked, loadErr := config.LoadProjectConfig(a.cwd); loadErr == nil && linked.ProjectID == project.ID {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "This directory is still linked to the deleted project; run `capydb unlink` and remove its env vars.")
			}
			return nil
		},
	}

	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm the deletion without prompting")
	command.Flags().StringVar(&approvalToken, "approval-token", "", "Delete approval an organization admin created in the dashboard (production projects); defaults to $CAPYDB_APPROVAL_TOKEN")
	// --yes is accepted as a spelling of --confirm for scripts written against
	// other CLIs; --confirm is the standard destructive-confirm flag here.
	command.Flags().BoolVar(&yes, "yes", false, "Confirm the deletion without prompting")
	_ = command.Flags().MarkHidden("yes")
	addWaitFlags(command, &wait, &waitTimeout, "project deletion")
	return command
}

// resolveApprovalToken is the approval token a destructive command presents:
// the flag, else $CAPYDB_APPROVAL_TOKEN.
func resolveApprovalToken(flag string) string {
	return firstNonEmpty(strings.TrimSpace(flag), strings.TrimSpace(os.Getenv("CAPYDB_APPROVAL_TOKEN")))
}

// approvalRequiredError explains how to get the approval a step needs: only a
// person signed in to the dashboard can create one, so it points at the
// project's settings page. doing describes the refused step ("upgrading x");
// approval names what to create ("a major upgrade").
func (a *app) approvalRequiredError(ctx context.Context, client *api.Client, apiURL string, project api.Project, doing, approval string) error {
	settingsURL, err := buildDashboardURL(a.resolveAppURL(apiURL), lookupWorkspaceSlug(ctx, client), project.Slug, project.ID, "settings")
	if err != nil {
		settingsURL = "the project's settings page in the dashboard"
	}
	return fmt.Errorf("%s needs an approval from an organization admin: open %s, create an approval for %s, and re-run with --approval-token <token> (or set CAPYDB_APPROVAL_TOKEN); the token works once, for 10 minutes. Nothing was changed", doing, settingsURL, approval)
}
