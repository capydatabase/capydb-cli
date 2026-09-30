package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// newUpgradeCommand groups the PostgreSQL version operations.
//
// Minor and major are separate commands because they are separate risks, and
// collapsing them behind one verb would hide that. A minor is a restart onto a
// binary-compatible release; a major rewrites the on-disk format and is a
// migration that must be checked first.
func (a *app) newUpgradeCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "upgrade",
		Short: "Manage PostgreSQL version upgrades",
		Long: "PostgreSQL versions move in two very different ways.\n\n" +
			"Minor releases (17.4 -> 17.5) are binary-compatible: your data is untouched and the " +
			"upgrade is a restart. Security fixes ship as minors. A paused database picks the new " +
			"version up on its own when it next resumes, so most databases need no action at all.\n\n" +
			"Major releases (17 -> 18) change the on-disk format and are a real migration: " +
			"`capydb upgrade major` checks the database with a preflight, copies it onto the new major, and " +
			"keeps the previous database for 72 hours so the upgrade can be rolled back. Each step needs an " +
			"approval from an organization admin.",
	}

	var minorProjectRef string
	var minorWait bool
	var minorWaitTimeout time.Duration
	minorCommand := &cobra.Command{
		Use:   "minor",
		Short: "Restart the database onto the latest available PostgreSQL minor",
		Long: "Applies a pending minor upgrade by restarting the database onto the version the " +
			"platform already has installed.\n\n" +
			"Minors are binary-compatible, so nothing is migrated - only the server binary changes. " +
			"The cost is a brief interruption to open connections. A database that is currently " +
			"paused does not need this: it starts on the new version when it next resumes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, minorProjectRef)
			if err != nil {
				return err
			}

			job, err := client.UpgradeProjectMinor(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("upgrade minor: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued minor upgrade for project %s\n", project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, minorWait, minorWaitTimeout, "minor upgrade")
		},
	}
	minorCommand.Flags().StringVar(&minorProjectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(minorCommand, &minorWait, &minorWaitTimeout, "minor upgrade")

	var preflightProjectRef string
	var preflightTarget int
	preflightWait := true
	var preflightWaitTimeout time.Duration
	preflightCommand := &cobra.Command{
		Use:   "preflight",
		Short: "Check whether the database can move to a PostgreSQL major",
		Long: "Runs a read-only check and reports whether a major upgrade would succeed.\n\n" +
			"Nothing is changed, so it is safe to run repeatedly. The blocker that matters most in " +
			"practice is extensions: if one you use has no build for the target major, migrating " +
			"would leave your schema referencing types and functions that no longer exist.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if preflightTarget <= 0 {
				return usageErrorf("--target-major is required (e.g. --target-major 18)")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, preflightProjectRef)
			if err != nil {
				return err
			}

			job, err := client.MajorUpgradePreflight(ctx, project.ID, preflightTarget)
			if err != nil {
				return fmt.Errorf("major upgrade preflight: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"Queued major-upgrade preflight (target %d) for project %s\n", preflightTarget, project.Name)
			}
			if !preflightWait {
				return a.maybeWaitForJob(cmd, client, job, false, preflightWaitTimeout, "major upgrade preflight")
			}
			job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, preflightWaitTimeout)
			if err != nil {
				return err
			}
			result, resultErr := client.GetJobResult(ctx, job.ID)
			if a.jsonOutput() {
				if resultErr == nil && len(result) > 0 {
					if err := printJSON(cmd.OutOrStdout(), struct {
						Job    api.Job         `json:"job"`
						Result json.RawMessage `json:"result"`
					}{Job: job, Result: result}); err != nil {
						return err
					}
				} else if err := a.printJob(cmd, job); err != nil {
					return err
				}
			} else {
				writeJob(cmd.OutOrStdout(), job)
				if resultErr == nil && len(result) > 0 {
					writeMajorUpgradePreflight(cmd.OutOrStdout(), result)
				}
			}
			return ensureCompletedJob(job, "major upgrade preflight")
		},
	}
	preflightCommand.Flags().StringVar(&preflightProjectRef, "project", "", "Project id, slug, or name")
	preflightCommand.Flags().IntVar(&preflightTarget, "target-major", 0, "PostgreSQL major to evaluate (e.g. 18)")
	preflightCommand.Flags().BoolVar(&preflightWait, "wait", true, "Wait for the major upgrade preflight job to finish")
	preflightCommand.Flags().DurationVar(&preflightWaitTimeout, "wait-timeout", defaultWaitTimeout, "Maximum time to wait for the major upgrade preflight job")

	command.AddCommand(minorCommand)
	command.AddCommand(preflightCommand)
	command.AddCommand(a.newUpgradeMajorCommand())
	command.AddCommand(a.newUpgradeConfirmCommand())
	command.AddCommand(a.newUpgradeRollbackCommand())
	command.AddCommand(a.newUpgradeStatusCommand())
	return command
}

// majorUpgradeApprovalHelp is the approval paragraph shared by the three
// major-upgrade steps; each names its own approval action.
const majorUpgradeApprovalHelp = `Each step needs its own single-use approval from an organization admin: open the project's
settings page in the dashboard, create an approval for the step, and pass it with --approval-token
(or CAPYDB_APPROVAL_TOKEN). An API key cannot approve its own upgrade. The approval works once, for
10 minutes. Self-serve major upgrades are available only where CapyDB has enabled them.`

func (a *app) newUpgradeMajorCommand() *cobra.Command {
	var projectRef string
	var targetMajor int
	var approvalToken string
	var confirmFlag bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "major",
		Short: "Upgrade the database to a newer PostgreSQL major",
		Long: `Moves the database to a newer PostgreSQL major. The upgrade stages a new database on the target
major, stops writes to the current one, copies the data across, verifies the copy, and swaps the
project onto it. The connection string does not change. Writes are refused while the copy runs,
so plan for a maintenance window sized to the database.

The previous database is kept for 72 hours: until then ` + "`capydb upgrade rollback`" + ` returns to it
and ` + "`capydb upgrade confirm`" + ` finalizes the upgrade early; afterwards CapyDB confirms it on its own.

The command runs the preflight check first (the control plane requires a passing one for the
same target from the last hour) and starts the upgrade only when it passes. The approval's 10
minutes include the preflight, so create it just before running this.

` + majorUpgradeApprovalHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if targetMajor <= 0 {
				return usageErrorf("--target-major is required (e.g. --target-major 18)")
			}
			client, authConfig, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			// Checked before the preflight so nobody waits on a check only to
			// learn the upgrade cannot start.
			token := resolveApprovalToken(approvalToken)
			if token == "" {
				return a.approvalRequiredError(ctx, client, authConfig.APIURL, project, "upgrading "+project.Name+" to Postgres "+strconv.Itoa(targetMajor), "a major upgrade")
			}
			confirmed, err := confirmProjectDestructiveAction(cmd, project, confirmFlag,
				"This will UPGRADE project %q (%s) to Postgres "+strconv.Itoa(targetMajor)+". Writes are refused while the data is copied.\n")
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("major upgrade not confirmed; pass --confirm or confirm interactively")
			}

			progress := cmd.OutOrStdout()
			if a.jsonOutput() {
				progress = cmd.ErrOrStderr()
			}
			_, _ = fmt.Fprintf(progress, "Running the major-upgrade preflight (target %d) for project %s\n", targetMajor, project.Name)
			preflight, err := client.MajorUpgradePreflight(ctx, project.ID, targetMajor)
			if err != nil {
				return majorUpgradeError("major upgrade preflight", err)
			}
			preflight, err = waitForJob(ctx, cmd.ErrOrStderr(), client, preflight.ID, waitTimeout)
			if err != nil {
				return err
			}
			if err := ensureCompletedJob(preflight, "major upgrade preflight"); err != nil {
				return err
			}
			result, err := client.GetJobResult(ctx, preflight.ID)
			if err != nil {
				return fmt.Errorf("read major upgrade preflight result: %w", err)
			}
			writeMajorUpgradePreflight(progress, result)
			if verdict := majorUpgradePreflightStatus(result); verdict != "upgradable" {
				return fmt.Errorf("the preflight did not pass (%s); nothing was upgraded", firstNonEmpty(verdict, "no verdict"))
			}

			job, err := client.UpgradeProjectMajor(ctx, project.ID, targetMajor, token)
			if err != nil {
				return majorUpgradeError("upgrade major", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued major upgrade to Postgres %d for project %s\n", targetMajor, project.Name)
			}
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "major upgrade"); err != nil {
				return err
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Follow it with `capydb upgrade status`; once it completes, `capydb upgrade confirm` or `capydb upgrade rollback`.")
			}
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().IntVar(&targetMajor, "target-major", 0, "PostgreSQL major to upgrade to (e.g. 18)")
	command.Flags().StringVar(&approvalToken, "approval-token", "", "Major-upgrade approval an organization admin created in the dashboard; defaults to $CAPYDB_APPROVAL_TOKEN")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm the upgrade without prompting")
	addWaitFlags(command, &wait, &waitTimeout, "major upgrade")
	return command
}

func (a *app) newUpgradeConfirmCommand() *cobra.Command {
	return a.newMajorUpgradeFinishCommand(majorUpgradeFinish{
		use:   "confirm",
		short: "Finalize a major upgrade and delete the previous database",
		long: `Finalizes a completed major upgrade: the database kept from before the upgrade is deleted, after
which the upgrade can no longer be rolled back. Without it, CapyDB confirms on its own when the
rollback window (72 hours after the cutover) closes; confirming early only gives up the rollback.

` + majorUpgradeApprovalHelp,
		warning:  "This will FINALIZE the major upgrade of project %q (%s) and delete the previous database. Rollback will no longer be possible.\n",
		action:   "an upgrade confirm",
		verb:     "confirming the upgrade of ",
		queued:   "Queued upgrade confirm for project %s\n",
		jobLabel: "major upgrade confirm",
		run: func(ctx context.Context, client *api.Client, projectID, token string) (api.Job, error) {
			return client.ConfirmMajorUpgrade(ctx, projectID, token)
		},
	})
}

func (a *app) newUpgradeRollbackCommand() *cobra.Command {
	return a.newMajorUpgradeFinishCommand(majorUpgradeFinish{
		use:   "rollback",
		short: "Roll a major upgrade back to the previous version",
		long: `Swaps the project back to the database kept from before the upgrade and deletes the upgraded one.
Every write committed after the upgrade's cutover is lost: the kept database is the state at the
cutover, not a replica. Available until the rollback window closes (see ` + "`capydb upgrade status`" + `).
Delete the project's preview databases first.

` + majorUpgradeApprovalHelp,
		warning:  "This will ROLL BACK the major upgrade of project %q (%s). Every write since the upgrade's cutover is lost.\n",
		action:   "an upgrade rollback",
		verb:     "rolling back the upgrade of ",
		queued:   "Queued upgrade rollback for project %s\n",
		jobLabel: "major upgrade rollback",
		run: func(ctx context.Context, client *api.Client, projectID, token string) (api.Job, error) {
			return client.RollbackMajorUpgrade(ctx, projectID, token)
		},
	})
}

// majorUpgradeFinish describes one of the two steps that end a major upgrade.
type majorUpgradeFinish struct {
	use, short, long string
	// warning is the confirmation prompt; it takes the project name and id.
	warning string
	// action names the approval to create; verb prefixes the project name in
	// the missing-approval error.
	action, verb string
	queued       string
	jobLabel     string
	run          func(ctx context.Context, client *api.Client, projectID, token string) (api.Job, error)
}

func (a *app) newMajorUpgradeFinishCommand(step majorUpgradeFinish) *cobra.Command {
	var projectRef string
	var approvalToken string
	var confirmFlag bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   step.use,
		Short: step.short,
		Long:  step.long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, authConfig, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			token := resolveApprovalToken(approvalToken)
			if token == "" {
				return a.approvalRequiredError(ctx, client, authConfig.APIURL, project, step.verb+project.Name, step.action)
			}
			confirmed, err := confirmProjectDestructiveAction(cmd, project, confirmFlag, step.warning)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("%s not confirmed; pass --confirm or confirm interactively", step.jobLabel)
			}

			job, err := step.run(ctx, client, project.ID, token)
			if err != nil {
				return majorUpgradeError(step.jobLabel, err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), step.queued, project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, step.jobLabel)
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&approvalToken, "approval-token", "", "Approval an organization admin created in the dashboard for this step; defaults to $CAPYDB_APPROVAL_TOKEN")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm without prompting")
	addWaitFlags(command, &wait, &waitTimeout, step.jobLabel)
	return command
}

func (a *app) newUpgradeStatusCommand() *cobra.Command {
	var projectRef string
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the major upgrade in flight, if any",
		Long: "Shows the major upgrade in flight: the versions it moves between, its state, and until when " +
			"it can be rolled back or confirmed. States: staging (copying and verifying the data), " +
			"rollback_available (running on the new major, previous database kept), confirming, rolling_back.",
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
			upgrade, err := client.GetMajorUpgradeStatus(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("get major upgrade status: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"upgrade": upgrade})
			}
			writeMajorUpgradeStatus(cmd.OutOrStdout(), project, upgrade)
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func writeMajorUpgradeStatus(out io.Writer, project api.Project, upgrade *api.MajorUpgradeStatus) {
	if upgrade == nil {
		_, _ = fmt.Fprintf(out, "No major upgrade in flight for project %s (Postgres %s).\n", project.Name, postgresLabel(project.PostgresVersion, project.PostgresChannel))
		return
	}
	_, _ = fmt.Fprintf(out, "postgresql: %s -> %s\n", upgrade.FromMajor, upgrade.ToMajor)
	_, _ = fmt.Fprintf(out, "state: %s\n", upgrade.State)
	if upgrade.RollbackAvailableUntil != nil {
		_, _ = fmt.Fprintf(out, "rollback_available_until: %s\n", formatTime(*upgrade.RollbackAvailableUntil))
	}
	_, _ = fmt.Fprintf(out, "started_at: %s\n", formatTime(upgrade.CreatedAt))
	_, _ = fmt.Fprintf(out, "updated_at: %s\n", formatTime(upgrade.UpdatedAt))
	if upgrade.State == "rollback_available" {
		_, _ = fmt.Fprintln(out, "Next: `capydb upgrade confirm` to keep it, or `capydb upgrade rollback` to return to the previous major.")
	}
}

// majorUpgradeError adds the likely reason to a refused major-upgrade step: a
// 403 means self-serve upgrades are not enabled or the caller may not run
// them, which the bare status does not say.
func majorUpgradeError(action string, err error) error {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.StatusCode == http.StatusForbidden {
		return authErrorf("%s: %v (self-serve major upgrades need CapyDB to have enabled them and an organization admin or manager key)", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// majorUpgradePreflightStatus is the preflight job result's verdict:
// "upgradable" or "blocked".
func majorUpgradePreflightStatus(raw json.RawMessage) string {
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return ""
	}
	return result.Status
}

func writeMajorUpgradePreflight(out io.Writer, raw json.RawMessage) {
	var result struct {
		Blockers     []string `json:"blockers"`
		CurrentMajor string   `json:"current_major"`
		Status       string   `json:"status"`
		TargetMajor  string   `json:"target_major"`
		Warnings     []string `json:"warnings"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return
	}
	_, _ = fmt.Fprintf(out, "verdict: %s\n", result.Status)
	_, _ = fmt.Fprintf(out, "postgresql: %s -> %s\n", result.CurrentMajor, result.TargetMajor)
	for _, blocker := range result.Blockers {
		_, _ = fmt.Fprintf(out, "blocker: %s\n", blocker)
	}
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
	}
}
