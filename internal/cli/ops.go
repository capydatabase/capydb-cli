package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

func (a *app) newPreviewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "preview",
		Short: "Manage preview databases",
	}

	var listProjectRef string
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List preview databases for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, listProjectRef)
			if err != nil {
				return err
			}

			previews, err := client.ListPreviewDatabases(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list previews: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"previews": jsonList(previews)})
			}
			if len(previews) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No preview databases for project %s\n", project.Name)
				return nil
			}

			writePreviewTable(cmd.OutOrStdout(), previews)
			return nil
		},
	}
	listCommand.Flags().StringVar(&listProjectRef, "project", "", "Project id, slug, or name")

	var createMode string
	var createName string
	var createProjectRef string
	var createTTLHours int
	var createWait bool
	var createWaitTimeout time.Duration
	createCommand := &cobra.Command{
		Use:   "create",
		Short: "Create a preview database for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Same range the server enforces (and `preview extend` validates)
			// - reject locally to save the round-trip. 0 means server default.
			if createTTLHours != 0 && (createTTLHours < 1 || createTTLHours > 168) {
				return usageErrorf("--ttl-hours must be between 1 and 168")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, createProjectRef)
			if err != nil {
				return err
			}

			created, job, err := client.CreatePreviewDatabase(ctx, project.ID, api.CreatePreviewRequest{
				Mode:     strings.TrimSpace(createMode),
				Name:     strings.TrimSpace(createName),
				TTLHours: createTTLHours,
			})
			if err != nil {
				return fmt.Errorf("create preview: %w", err)
			}

			// The preview itself was created; a failure to fetch its
			// connection details is surfaced as a warning, not an error.
			preview := api.PreviewDetails{Preview: created}
			connections, connErr := client.GetPreviewConnection(ctx, created.ID)
			if connErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: preview created, but fetching connection details failed: %v\n", connErr)
			} else {
				preview.Connections = connections
			}

			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created preview %s (%s) for project %s\n", preview.Preview.Name, preview.Preview.ID, project.Name)
				writePreviewTable(cmd.OutOrStdout(), []api.PreviewDetails{preview})
			}

			if createWait && job.ID != "" {
				if !a.jsonOutput() {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Waiting for job %s\n", job.ID)
				}
				job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, createWaitTimeout)
				if err != nil {
					return err
				}
				if err := ensureCompletedJob(job, "preview creation"); err != nil {
					return err
				}
				if latestPreview, latestErr := a.resolvePreview(ctx, client, project.ID, preview.Preview.ID); latestErr == nil {
					preview = latestPreview
					if !a.jsonOutput() {
						writePreviewTable(cmd.OutOrStdout(), []api.PreviewDetails{preview})
					}
				}
			}

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"preview": preview, "job": job})
			}
			return nil
		},
	}
	createCommand.Flags().StringVar(&createProjectRef, "project", "", "Project id, slug, or name")
	createCommand.Flags().StringVar(&createMode, "mode", "", "Preview mode: empty or clone")
	createCommand.Flags().StringVar(&createName, "name", "", "Preview name")
	createCommand.Flags().IntVar(&createTTLHours, "ttl-hours", 0, "Preview TTL in hours")
	addWaitFlags(createCommand, &createWait, &createWaitTimeout, "preview create")

	var deletePreviewRef string
	var deleteProjectRef string
	var deleteWait bool
	var deleteWaitTimeout time.Duration
	deleteCommand := &cobra.Command{
		Use:   "delete",
		Short: "Delete a preview database",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, deleteProjectRef)
			if err != nil {
				return err
			}
			preview, err := a.resolvePreview(ctx, client, project.ID, deletePreviewRef)
			if err != nil {
				return err
			}

			job, err := client.DeletePreviewDatabase(ctx, preview.Preview.ID)
			if err != nil {
				return fmt.Errorf("delete preview: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued delete for preview %s (%s)\n", preview.Preview.Name, preview.Preview.ID)
			}
			return a.maybeWaitForJob(cmd, client, job, deleteWait, deleteWaitTimeout, "preview delete")
		},
	}
	deleteCommand.Flags().StringVar(&deleteProjectRef, "project", "", "Project id, slug, or name")
	deleteCommand.Flags().StringVar(&deletePreviewRef, "preview", "", "Preview id or name")
	addWaitFlags(deleteCommand, &deleteWait, &deleteWaitTimeout, "preview delete")

	var resetPreviewRef string
	var resetProjectRef string
	var resetWait bool
	var resetWaitTimeout time.Duration
	resetCommand := &cobra.Command{
		Use:   "reset",
		Short: "Reset a preview database",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, resetProjectRef)
			if err != nil {
				return err
			}
			preview, err := a.resolvePreview(ctx, client, project.ID, resetPreviewRef)
			if err != nil {
				return err
			}

			job, err := client.ResetPreviewDatabase(ctx, preview.Preview.ID)
			if err != nil {
				return fmt.Errorf("reset preview: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued reset for preview %s (%s)\n", preview.Preview.Name, preview.Preview.ID)
			}
			return a.maybeWaitForJob(cmd, client, job, resetWait, resetWaitTimeout, "preview reset")
		},
	}
	resetCommand.Flags().StringVar(&resetProjectRef, "project", "", "Project id, slug, or name")
	resetCommand.Flags().StringVar(&resetPreviewRef, "preview", "", "Preview id or name")
	addWaitFlags(resetCommand, &resetWait, &resetWaitTimeout, "preview reset")

	var extendTTLHours int
	extendCommand := &cobra.Command{
		Use:   "extend <preview-id>",
		Short: "Extend the TTL of a preview database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			previewID := strings.TrimSpace(args[0])
			if previewID == "" {
				return usageErrorf("preview id is required")
			}
			if extendTTLHours < 1 || extendTTLHours > 168 {
				return usageErrorf("--ttl-hours must be between 1 and 168")
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			preview, err := client.ExtendPreviewDatabase(ctx, previewID, extendTTLHours)
			if err != nil {
				return fmt.Errorf("extend preview: %w", err)
			}

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"preview": preview})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Set preview %s (%s) TTL to %dh from now\n", preview.Name, preview.ID, extendTTLHours)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ttl_expires_at: %s\n", formatTime(preview.TTLExpiresAt))
			return nil
		},
	}
	extendCommand.Flags().IntVar(&extendTTLHours, "ttl-hours", 0, "New TTL in hours (1-168)")

	command.AddCommand(listCommand)
	command.AddCommand(createCommand)
	command.AddCommand(deleteCommand)
	command.AddCommand(resetCommand)
	command.AddCommand(extendCommand)
	return command
}

func (a *app) newBackupsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "backups",
		Aliases: []string{"backup"},
		Short:   "Manage project backups",
	}

	var listProjectRef string
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List backups for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, listProjectRef)
			if err != nil {
				return err
			}

			backups, err := client.ListBackups(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list backups: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"backups": jsonList(backups)})
			}
			if len(backups) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No backups for project %s\n", project.Name)
				return nil
			}

			writeBackupTable(cmd.OutOrStdout(), backups)
			return nil
		},
	}
	listCommand.Flags().StringVar(&listProjectRef, "project", "", "Project id, slug, or name")

	var createLabel string
	var createProjectRef string
	var createWait bool
	var createWaitTimeout time.Duration
	createCommand := &cobra.Command{
		Use:   "create",
		Short: "Queue a backup for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, createProjectRef)
			if err != nil {
				return err
			}

			job, err := client.CreateBackup(ctx, project.ID, createLabel)
			if err != nil {
				return fmt.Errorf("create backup: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued backup job %s for project %s\n", job.ID, project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, createWait, createWaitTimeout, "backup")
		},
	}
	createCommand.Flags().StringVar(&createProjectRef, "project", "", "Project id, slug, or name")
	createCommand.Flags().StringVar(&createLabel, "label", "", "Optional backup label")
	addWaitFlags(createCommand, &createWait, &createWaitTimeout, "backup")

	command.AddCommand(listCommand)
	command.AddCommand(createCommand)
	command.AddCommand(a.newBackupScheduleCommand())
	return command
}

func (a *app) newExportCommand() *cobra.Command {
	var projectRef string
	var output string
	var noDownload bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "export",
		Short: "Export the project database to a downloadable dump",
		Long: "Queues a logical export (a pg_dump custom-format archive), waits for it to complete, " +
			"and downloads the artifact. Exports stay downloadable for 7 days; re-download one within " +
			"that window with `capydb export download`.",
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

			exportID, job, err := client.CreateExport(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("create export: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued export job %s for project %s\n", job.ID, project.Name)
			}

			job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, waitTimeout)
			if err != nil {
				return err
			}
			if err := ensureCompletedJob(job, "export"); err != nil {
				return err
			}

			if noDownload {
				if a.jsonOutput() {
					return printJSON(cmd.OutOrStdout(), map[string]any{"export_id": exportID, "job": job})
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Export %s is ready; download it with: capydb export download --export %s\n", exportID, exportID)
				return nil
			}

			dest := output
			if dest == "" {
				dest = exportFileName(project.Name)
			}
			if err := downloadExport(ctx, cmd.ErrOrStderr(), client, project.ID, exportID, dest); err != nil {
				return err
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"export_id": exportID, "file": dest})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Downloaded export %s to %s\n", exportID, dest)
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&output, "output", "", "Destination file (default <project>-<timestamp>.dump)")
	command.Flags().BoolVar(&noDownload, "no-download", false, "Queue the export and wait, but skip the download")
	command.Flags().DurationVar(&waitTimeout, "wait-timeout", defaultWaitTimeout, "Maximum time to wait for the export job")

	var listProjectRef string
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List exports for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, listProjectRef)
			if err != nil {
				return err
			}

			exports, err := client.ListExports(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list exports: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"exports": jsonList(exports)})
			}
			if len(exports) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No exports for project %s\n", project.Name)
				return nil
			}

			writeExportTable(cmd.OutOrStdout(), exports)
			return nil
		},
	}
	listCommand.Flags().StringVar(&listProjectRef, "project", "", "Project id, slug, or name")

	var downloadProjectRef string
	var downloadExportID string
	var downloadOutput string
	downloadCommand := &cobra.Command{
		Use:   "download",
		Short: "Download a completed export",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(downloadExportID) == "" {
				return errors.New("specify --export <id> (see capydb export list)")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, downloadProjectRef)
			if err != nil {
				return err
			}

			dest := downloadOutput
			if dest == "" {
				dest = exportFileName(project.Name)
			}
			if err := downloadExport(ctx, cmd.ErrOrStderr(), client, project.ID, downloadExportID, dest); err != nil {
				return err
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"export_id": downloadExportID, "file": dest})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Downloaded export %s to %s\n", downloadExportID, dest)
			return nil
		},
	}
	downloadCommand.Flags().StringVar(&downloadProjectRef, "project", "", "Project id, slug, or name")
	downloadCommand.Flags().StringVar(&downloadExportID, "export", "", "Export id to download")
	downloadCommand.Flags().StringVar(&downloadOutput, "output", "", "Destination file (default <project>-<timestamp>.dump)")

	command.AddCommand(listCommand)
	command.AddCommand(downloadCommand)
	return command
}

// downloadExport presigns a download URL for the export and streams it to
// destPath with the same TTY-aware progress rendering as dump uploads.
func downloadExport(ctx context.Context, stderr io.Writer, client *api.Client, projectID, exportID, destPath string) error {
	download, err := client.GetExportDownload(ctx, projectID, exportID)
	if err != nil {
		return fmt.Errorf("request download URL: %w", err)
	}

	isTTY := writerIsTerminal(stderr)
	renderInterval := 200 * time.Millisecond
	if !isTTY {
		renderInterval = 5 * time.Second
	}
	lastRender := time.Time{}
	progress := func(received, total int64) {
		if time.Since(lastRender) < renderInterval && (total == 0 || received < total) {
			return
		}
		if !isTTY && total > 0 && received >= total {
			return
		}
		lastRender = time.Now()
		line := fmt.Sprintf("Downloading %s... %s", destPath, formatBytes(received))
		if total > 0 {
			line += fmt.Sprintf(" / %s (%d%%)", formatBytes(total), received*100/total)
		}
		if isTTY {
			_, _ = fmt.Fprintf(stderr, "\r%s", line)
		} else {
			_, _ = fmt.Fprintln(stderr, line)
		}
	}

	if err := client.DownloadExport(ctx, download.DownloadURL, destPath, progress); err != nil {
		if isTTY {
			_, _ = fmt.Fprintln(stderr)
		}
		return err
	}
	if isTTY {
		_, _ = fmt.Fprintln(stderr)
	} else {
		_, _ = fmt.Fprintf(stderr, "Download complete: %s\n", destPath)
	}
	return nil
}

// exportFileName derives a safe default filename for a downloaded export.
func exportFileName(projectName string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r == ' ', r == '_':
			return '-'
		default:
			return -1
		}
	}, projectName)
	if sanitized == "" {
		sanitized = "capydb"
	}
	return fmt.Sprintf("%s-%s.dump", sanitized, time.Now().UTC().Format("20060102T150405Z"))
}

func writeExportTable(out io.Writer, exports []api.ProjectExport) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tSTATE\tSIZE\tCREATED_AT\tEXPIRES_AT")
	for _, export := range exports {
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\n",
			export.ID,
			export.State,
			formatBytes(export.SizeBytes),
			formatTime(export.CreatedAt),
			formatTime(export.ExpiresAt),
		)
	}
	_ = writer.Flush()
}

func (a *app) newBackupScheduleCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "schedule",
		Short: "Manage the project's daily backup schedule",
	}

	var getProjectRef string
	getCommand := &cobra.Command{
		Use:   "get",
		Short: "Show the backup schedule for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, getProjectRef)
			if err != nil {
				return err
			}

			schedules, err := client.ListScheduledBackups(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list scheduled backups: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"scheduled_backups": jsonList(schedules)})
			}
			if len(schedules) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No backup schedule for project %s\n", project.Name)
				return nil
			}

			writeScheduledBackupTable(cmd.OutOrStdout(), schedules)
			return nil
		},
	}
	getCommand.Flags().StringVar(&getProjectRef, "project", "", "Project id, slug, or name")

	var setHour int
	var setLabel string
	var setMinute int
	var setProjectRef string
	var setRetentionDays int
	setCommand := &cobra.Command{
		Use:   "set",
		Short: "Create or update the project's daily backup schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if setHour < 0 || setHour > 23 {
				return usageErrorf("--hour must be between 0 and 23")
			}
			if setMinute < 0 || setMinute > 59 {
				return usageErrorf("--minute must be between 0 and 59")
			}
			if setRetentionDays != 0 && (setRetentionDays < 1 || setRetentionDays > 365) {
				return usageErrorf("--retention-days must be between 1 and 365")
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, setProjectRef)
			if err != nil {
				return err
			}

			schedule, err := client.UpsertScheduledBackup(ctx, project.ID, api.UpsertScheduledBackupRequest{
				CronHour:      setHour,
				CronMinute:    setMinute,
				IsActive:      true,
				Label:         strings.TrimSpace(setLabel),
				RetentionDays: setRetentionDays,
			})
			if err != nil {
				return fmt.Errorf("set backup schedule: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"scheduled_backup": schedule})
			}
			_, _ = fmt.Fprintf(
				cmd.OutOrStdout(),
				"Backup schedule for project %s: daily at %02d:%02d UTC, retention %dd\n",
				project.Name,
				schedule.CronHour,
				schedule.CronMinute,
				schedule.RetentionDays,
			)
			return nil
		},
	}
	setCommand.Flags().StringVar(&setProjectRef, "project", "", "Project id, slug, or name")
	setCommand.Flags().IntVar(&setHour, "hour", 0, "UTC hour of day to run the backup (0-23)")
	setCommand.Flags().IntVar(&setMinute, "minute", 0, "Minute of the hour to run the backup (0-59)")
	setCommand.Flags().IntVar(&setRetentionDays, "retention-days", 0, "Days to keep scheduled backups (1-365; server default when omitted)")
	setCommand.Flags().StringVar(&setLabel, "label", "", "Optional label for scheduled backups")

	var disableProjectRef string
	disableCommand := &cobra.Command{
		Use:   "disable",
		Short: "Disable the project's backup schedule",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			project, err := a.resolveProject(ctx, client, disableProjectRef)
			if err != nil {
				return err
			}

			schedules, err := client.ListScheduledBackups(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list scheduled backups: %w", err)
			}
			if len(schedules) == 0 {
				return notFoundErrorf("project %s has no backup schedule to disable", project.Name)
			}

			// The upsert replaces the single default schedule wholesale, so the
			// existing cadence/retention/label are carried over unchanged.
			current := schedules[0]
			schedule, err := client.UpsertScheduledBackup(ctx, project.ID, api.UpsertScheduledBackupRequest{
				CronHour:      current.CronHour,
				CronMinute:    current.CronMinute,
				IsActive:      false,
				Label:         current.Label,
				RetentionDays: current.RetentionDays,
			})
			if err != nil {
				return fmt.Errorf("disable backup schedule: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"scheduled_backup": schedule})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Disabled backup schedule for project %s\n", project.Name)
			return nil
		},
	}
	disableCommand.Flags().StringVar(&disableProjectRef, "project", "", "Project id, slug, or name")

	command.AddCommand(getCommand)
	command.AddCommand(setCommand)
	command.AddCommand(disableCommand)
	return command
}

func writeScheduledBackupTable(out io.Writer, schedules []api.ScheduledBackup) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tLABEL\tACTIVE\tRUNS_AT_UTC\tRETENTION\tLAST_RUN_AT")
	for _, schedule := range schedules {
		active := "no"
		if schedule.IsActive {
			active = "yes"
		}
		lastRun := "-"
		if schedule.LastRunAt != nil {
			lastRun = schedule.LastRunAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%02d:%02d\t%dd\t%s\n",
			schedule.ID,
			firstNonEmpty(schedule.Label, "-"),
			active,
			schedule.CronHour,
			schedule.CronMinute,
			schedule.RetentionDays,
			lastRun,
		)
	}
	_ = writer.Flush()
}

func (a *app) newImportCommand() *cobra.Command {
	var dumpFile string
	var projectRef string
	var recreate bool
	var sourceURL string
	var follow bool
	var confirm bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "import",
		Short: "Import data from another Postgres database or a dump file into a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			sourceURL = strings.TrimSpace(sourceURL)
			dumpFile = strings.TrimSpace(dumpFile)

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}

			// --follow: near-zero-downtime streaming import. Base-copies then
			// streams the source's changes until `capydb import cutover`.
			if follow {
				switch {
				case sourceURL == "":
					return usageErrorf("--follow requires --source-url")
				case dumpFile != "":
					return usageErrorf("--follow cannot be used with --file (dump files are point-in-time)")
				case recreate:
					return usageErrorf("--follow cannot be used with --recreate")
				}
				// The follower writes into the live database from the start
				// (base copy, then continuous streaming) - the cutover confirm
				// arrives only after that data has already landed, so the start
				// gets the same typed-name gate as a plain import.
				confirmed, confirmErr := confirmProjectDestructiveAction(cmd, project, confirm,
					"A follow import begins writing into the LIVE database for project %q (%s) immediately (base copy, then continuous streaming until cutover).\n")
				if confirmErr != nil {
					return confirmErr
				}
				if !confirmed {
					return fmt.Errorf("follow import not confirmed; pass --confirm or confirm interactively")
				}
				job, startErr := client.StartImportFollow(ctx, project.ID, sourceURL)
				if startErr != nil {
					return fmt.Errorf("start follow import: %w", startErr)
				}
				if !a.jsonOutput() {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Started follow import (job %s) for project %s.\nWatch progress with `capydb import follow-status`, then finish with `capydb import cutover`.\n", job.ID, project.Name)
				}
				return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "import follow start")
			}

			switch {
			case sourceURL == "" && dumpFile == "":
				return usageErrorf("one of --source-url or --file is required")
			case sourceURL != "" && dumpFile != "":
				return usageErrorf("--source-url and --file are mutually exclusive")
			}

			// Import loads into the project's live database - same blast radius
			// as an overwrite restore, so it gets the same typed-name gate.
			confirmed, confirmErr := confirmProjectDestructiveAction(cmd, project, confirm,
				"This will load the imported data into the LIVE database for project %q (%s).\n")
			if confirmErr != nil {
				return confirmErr
			}
			if !confirmed {
				return fmt.Errorf("import not confirmed; pass --confirm or confirm interactively")
			}

			request := api.CreateImportRequest{Confirm: true, Recreate: recreate, SourceURL: sourceURL}
			if dumpFile != "" {
				uploadKey, uploadErr := uploadImportDump(ctx, cmd.ErrOrStderr(), client, project.ID, dumpFile)
				if uploadErr != nil {
					return uploadErr
				}
				request.UploadKey = uploadKey
			}

			job, err := client.CreateImport(ctx, project.ID, request)
			if err != nil {
				return fmt.Errorf("create import: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued import job %s for project %s\n", job.ID, project.Name)
			}
			// A nil error with --wait set means ensureCompletedJob passed
			// (freshly queued jobs are always pending, so the wait ran).
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "import"); err != nil {
				return err
			}
			if wait && job.ID != "" && !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Import complete: the live database for project %s now contains the imported data. Verify with `capydb sql \"select 1\"`.\n", project.Name)
			}
			return nil
		},
	}

	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&sourceURL, "source-url", "", "Source Postgres connection string (use the provider's direct endpoint - Neon '-pooler' hosts and Supabase's transaction pooler on port 6543 are rejected)")
	command.Flags().StringVar(&dumpFile, "file", "", "Path to a pg_dump file to upload and import (custom format recommended: pg_dump -Fc)")
	command.Flags().BoolVar(&recreate, "recreate", false, "Recreate the target database before import")
	command.Flags().BoolVar(&follow, "follow", false, "Near-zero-downtime import: base-copy then stream the source's changes until `capydb import cutover` (source needs logical replication enabled)")
	command.Flags().BoolVar(&confirm, "confirm", false, "Confirm loading the imported data into the project's live database")
	addWaitFlags(command, &wait, &waitTimeout, "import")

	command.AddCommand(a.newImportPreflightCommand())
	command.AddCommand(a.newImportFollowStatusCommand())
	command.AddCommand(a.newImportCutoverCommand())
	command.AddCommand(a.newImportFollowAbortCommand())
	return command
}

func (a *app) newImportFollowStatusCommand() *cobra.Command {
	var projectRef string
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "follow-status",
		Short: "Show progress of an in-progress follow import (replication lag, phase)",
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
			job, err := client.ImportFollowStatus(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("follow status: %w", err)
			}
			if !wait {
				return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "import follow status")
			}
			job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, waitTimeout)
			if err != nil {
				return err
			}
			// The status payload (unit state, replication lag, sentinel LSNs)
			// is the whole point of this command; render it, not just the job.
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
				return ensureCompletedJob(job, "import follow status")
			}
			writeJob(cmd.OutOrStdout(), job)
			if resultErr == nil && len(result) > 0 {
				writeImportFollowStatus(cmd.OutOrStdout(), result)
			}
			return ensureCompletedJob(job, "import follow status")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(command, &wait, &waitTimeout, "import follow status")
	return command
}

// writeImportFollowStatus renders the follow-status job's structured result in
// human terms: whether the follower is streaming, how far behind the source it
// is, and what to do next.
func writeImportFollowStatus(out io.Writer, result json.RawMessage) {
	var status struct {
		UnitActive string          `json:"unit_active"`
		Result     string          `json:"result"`
		LagBytes   *int64          `json:"lag_bytes"`
		Sentinel   json.RawMessage `json:"sentinel"`
	}
	if err := json.Unmarshal(result, &status); err != nil {
		return
	}
	switch {
	case status.UnitActive == "active":
		_, _ = fmt.Fprintln(out, "Stream: active (copying/streaming changes from the source)")
	case status.Result == "success":
		_, _ = fmt.Fprintln(out, "Stream: stopped at the end position - ready for `capydb import cutover`")
	case status.UnitActive == "":
		_, _ = fmt.Fprintln(out, "Stream: no follow in progress for this project")
	default:
		_, _ = fmt.Fprintf(out, "Stream: not running (state %s) - restart with `capydb import --follow` or cancel with `capydb import follow-abort`\n", firstNonEmpty(status.Result, status.UnitActive))
	}
	if status.LagBytes != nil {
		_, _ = fmt.Fprintf(out, "Replication lag: %s behind the source\n", formatBytes(*status.LagBytes))
	} else {
		_, _ = fmt.Fprintln(out, "Replication lag: unknown (the base copy may still be running, or the stream has finished)")
	}
}

func (a *app) newImportCutoverCommand() *cobra.Command {
	var projectRef string
	var confirm bool
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "cutover",
		Short: "Finish a follow import: drain the stream, flip the project live (stop source writers first)",
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
			confirmed, err := confirmProjectDestructiveAction(cmd, project, confirm,
				"Cutover will REPLACE the live database for project %q (%s) with the imported data.\n")
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("cutover not confirmed; pass --confirm or confirm interactively")
			}
			job, err := client.ImportFollowCutover(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("cutover: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued cutover job %s for project %s\n", job.ID, project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "import cutover")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&confirm, "confirm", false, "Confirm replacing the project's live database with the imported data")
	addWaitFlags(command, &wait, &waitTimeout, "import cutover")
	return command
}

func (a *app) newImportFollowAbortCommand() *cobra.Command {
	var projectRef string
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "follow-abort",
		Short: "Cancel an in-progress follow import and clean up the source replication slot",
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
			job, err := client.ImportFollowAbort(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("abort follow: %w", err)
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "import follow abort")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(command, &wait, &waitTimeout, "import follow abort")
	return command
}

// uploadImportDump requests a presigned upload slot for the project, streams
// the local dump file to it with a progress line on stderr, and returns the
// object key to start the import with.
func uploadImportDump(ctx context.Context, stderr io.Writer, client *api.Client, projectID, dumpFile string) (string, error) {
	upload, err := client.CreateImportUpload(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("request upload URL: %w", err)
	}

	// On a TTY the progress line rewrites itself in place; piped/CI output
	// gets plain lines at most every 5 seconds so logs stay readable.
	isTTY := writerIsTerminal(stderr)
	renderInterval := 200 * time.Millisecond
	if !isTTY {
		renderInterval = 5 * time.Second
	}
	lastRender := time.Time{}
	progress := func(sent, total int64) {
		// Throttle redraws; always render the final 100% line on a TTY. The
		// non-TTY path ends with the "Upload complete" line instead.
		if time.Since(lastRender) < renderInterval && sent < total {
			return
		}
		if !isTTY && sent >= total {
			return
		}
		lastRender = time.Now()
		percent := int64(100)
		if total > 0 {
			percent = sent * 100 / total
		}
		line := fmt.Sprintf("Uploading %s... %s / %s (%d%%)", dumpFile, formatBytes(sent), formatBytes(total), percent)
		if isTTY {
			_, _ = fmt.Fprintf(stderr, "\r%s", line)
		} else {
			_, _ = fmt.Fprintln(stderr, line)
		}
	}

	if err := client.UploadDump(ctx, upload.UploadURL, dumpFile, progress); err != nil {
		if isTTY {
			_, _ = fmt.Fprintln(stderr)
		}
		return "", fmt.Errorf("upload dump: %w", err)
	}
	if isTTY {
		_, _ = fmt.Fprintln(stderr)
	} else {
		_, _ = fmt.Fprintf(stderr, "Upload complete: %s\n", dumpFile)
	}
	return upload.ObjectKey, nil
}

func (a *app) newImportPreflightCommand() *cobra.Command {
	var projectRef string
	var sourceURL string

	command := &cobra.Command{
		Use:   "preflight",
		Short: "Check a source database against the project before importing",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(sourceURL) == "" {
				return usageErrorf("--source-url is required")
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}

			preflight, err := client.ImportPreflight(ctx, project.ID, sourceURL)
			if err != nil {
				return fmt.Errorf("import preflight: %w", err)
			}

			if a.jsonOutput() {
				if err := printJSON(cmd.OutOrStdout(), map[string]any{"preflight": preflight}); err != nil {
					return err
				}
			} else {
				writeImportPreflight(cmd.OutOrStdout(), preflight)
			}
			if !preflight.OK {
				return fmt.Errorf("import preflight failed; fix the failing checks before running `capydb import`")
			}
			return nil
		},
	}

	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&sourceURL, "source-url", "", "Source Postgres connection string (use the provider's direct endpoint - Neon '-pooler' hosts and Supabase's transaction pooler on port 6543 are rejected)")
	return command
}

func writeImportPreflight(out io.Writer, preflight api.ImportPreflight) {
	for _, check := range preflight.Checks {
		marker := "[" + strings.ToLower(firstNonEmpty(check.Status, "unknown")) + "]"
		switch strings.ToLower(strings.TrimSpace(check.Status)) {
		case "pass", "ok", "passed":
			marker = "[pass]"
		case "warn", "warning":
			marker = "[warn]"
		case "fail", "failed", "error":
			marker = "[fail]"
		}
		if strings.TrimSpace(check.Detail) != "" {
			_, _ = fmt.Fprintf(out, "%s %s: %s\n", marker, check.Name, check.Detail)
		} else {
			_, _ = fmt.Fprintf(out, "%s %s\n", marker, check.Name)
		}
	}

	_, _ = fmt.Fprintf(out, "source_version: %s\n", firstNonEmpty(preflight.Source.ServerVersion, "-"))
	_, _ = fmt.Fprintf(out, "source_size: %s\n", formatBytes(preflight.Source.DatabaseSizeBytes))
	if len(preflight.Source.Extensions) > 0 {
		names := make([]string, 0, len(preflight.Source.Extensions))
		for _, extension := range preflight.Source.Extensions {
			names = append(names, extension.Name+" "+extension.Version)
		}
		_, _ = fmt.Fprintf(out, "source_extensions: %s\n", strings.Join(names, ", "))
	}
	if len(preflight.Source.AppSchemas) > 0 {
		_, _ = fmt.Fprintf(out, "app_schemas_beyond_public: %s\n", strings.Join(preflight.Source.AppSchemas, ", "))
	}
	if len(preflight.Source.ProviderSchemas) > 0 {
		_, _ = fmt.Fprintf(out, "provider_schemas_not_migrated: %s\n", strings.Join(preflight.Source.ProviderSchemas, ", "))
	}
	if len(preflight.Source.ProviderAuthFKs) > 0 {
		refs := make([]string, 0, len(preflight.Source.ProviderAuthFKs))
		for _, fk := range preflight.Source.ProviderAuthFKs {
			refs = append(refs, fk.SourceTable+" -> "+fk.TargetTable)
		}
		_, _ = fmt.Fprintf(out, "provider_auth_fks: %s\n", strings.Join(refs, ", "))
	}
	if len(preflight.Source.ProviderAuthPolicyTables) > 0 {
		_, _ = fmt.Fprintf(out, "auth_bound_rls_tables: %s\n", strings.Join(preflight.Source.ProviderAuthPolicyTables, ", "))
	}
	_, _ = fmt.Fprintf(out, "target_version: %s\n", firstNonEmpty(preflight.TargetVersion, "-"))
	_, _ = fmt.Fprintf(out, "storage_limit: %s\n", formatBytes(preflight.StorageLimitBytes))
	if preflight.OK {
		_, _ = fmt.Fprintln(out, "preflight: ok")
	} else {
		_, _ = fmt.Fprintln(out, "preflight: failed")
	}
}

func (a *app) newRestoreCommand() *cobra.Command {
	var allowUnverified bool
	var approvalToken string
	var backupKey string
	var confirmFlag bool
	var confirmProjectOverwrite bool
	var previewName string
	var previewRef string
	var projectRef string
	var recreate bool
	var restorePointID string
	var restoreTime string
	var targetKind string
	var ttlHours int
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "restore",
		Short: "Restore a backup into a project or preview database",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			trimmedBackupKey := strings.TrimSpace(backupKey)
			trimmedRestoreTime := strings.TrimSpace(restoreTime)
			trimmedRestorePoint := strings.TrimSpace(restorePointID)
			provided := 0
			if trimmedBackupKey != "" {
				provided++
			}
			if trimmedRestoreTime != "" {
				provided++
			}
			if trimmedRestorePoint != "" {
				provided++
			}
			if provided != 1 {
				return usageErrorf("exactly one of --backup-key, --restore-time, or --restore-point is required")
			}

			client, authConfig, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}

			resolvedKind, err := resolveRestoreTargetKind(targetKind, previewRef, previewName)
			if err != nil {
				return err
			}

			request := api.CreateRestoreRequest{
				AllowUnverifiedBackup: allowUnverified,
				BackupKey:             trimmedBackupKey,
				PreviewName:           strings.TrimSpace(previewName),
				Recreate:              recreate,
				RestorePointID:        trimmedRestorePoint,
				RestoreTime:           trimmedRestoreTime,
				TargetKind:            resolvedKind,
				TTLHours:              ttlHours,
			}
			if resolvedKind == "preview" {
				preview, err := a.resolvePreview(ctx, client, project.ID, previewRef)
				if err != nil {
					return err
				}
				request.PreviewID = preview.Preview.ID
			}

			if resolvedKind == "new_preview" && ttlHours != 0 && (ttlHours < 1 || ttlHours > 168) {
				return usageErrorf("--ttl-hours must be between 1 and 168")
			}

			if resolvedKind == "project" {
				confirmed, err := confirmProjectRestoreOverwrite(cmd, project, confirmProjectOverwrite || confirmFlag)
				if err != nil {
					return err
				}
				if !confirmed {
					return fmt.Errorf("project overwrite not confirmed; pass --confirm or confirm interactively")
				}
				// The control plane's gate is a single-use approval that only a
				// person signed in to the dashboard can create; an API key cannot
				// approve its own overwrite. The CLI presents the token it is given.
				token := firstNonEmpty(strings.TrimSpace(approvalToken), strings.TrimSpace(os.Getenv("CAPYDB_APPROVAL_TOKEN")))
				if token == "" {
					backupsURL, urlErr := buildDashboardURL(a.resolveAppURL(authConfig.APIURL), lookupWorkspaceSlug(ctx, client), project.Slug, project.ID, "backups")
					if urlErr != nil {
						backupsURL = "the project's Backups page in the dashboard"
					}
					return fmt.Errorf("overwriting %s needs an approval from an organization admin: open %s, create an approval for an overwrite restore, and re-run with --approval-token <token> (or set CAPYDB_APPROVAL_TOKEN); the token works for 10 minutes", project.Name, backupsURL)
				}
				request.ApprovalToken = token
			}

			job, err := client.CreateRestore(ctx, project.ID, request)
			if err != nil {
				return fmt.Errorf("create restore: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued restore job %s for project %s\n", job.ID, project.Name)
			}
			// A nil error with --wait set means ensureCompletedJob passed
			// (freshly queued jobs are always pending, so the wait ran).
			if err := a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "restore"); err != nil {
				return err
			}
			if wait && job.ID != "" && !a.jsonOutput() {
				source := restoreSourceDescription(trimmedBackupKey, trimmedRestoreTime, trimmedRestorePoint)
				_, _ = fmt.Fprint(cmd.OutOrStdout(), restoreOutcome(project.Name, resolvedKind, source, firstNonEmpty(job.PreviewDatabaseID, request.PreviewID)))
			}
			return nil
		},
	}

	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&backupKey, "backup-key", "", "Backup key to restore")
	command.Flags().StringVar(&restoreTime, "restore-time", "", "RFC3339 timestamp for point-in-time restore")
	command.Flags().StringVar(&restorePointID, "restore-point", "", "Labelled restore point id (alternative to --backup-key/--restore-time)")
	command.Flags().StringVar(&targetKind, "target-kind", "", "Restore target: project, preview, or new_preview (inferred from --preview/--preview-name; defaults to new_preview)")
	command.Flags().StringVar(&previewRef, "preview", "", "Existing preview id or name when target-kind is preview")
	command.Flags().StringVar(&previewName, "preview-name", "", "Preview name when target-kind is new_preview")
	command.Flags().IntVar(&ttlHours, "ttl-hours", 0, "Preview TTL in hours when target-kind is new_preview")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm overwriting the project database (target-kind project)")
	command.Flags().StringVar(&approvalToken, "approval-token", "", "Approval an organization admin created in the dashboard for an overwrite restore (target-kind project); defaults to $CAPYDB_APPROVAL_TOKEN")
	// Deprecated spelling kept as a hidden alias: --confirm is the standard
	// destructive-confirm flag across capydb commands.
	command.Flags().BoolVar(&confirmProjectOverwrite, "confirm-project-overwrite", false, "Confirm overwriting the project database")
	_ = command.Flags().MarkHidden("confirm-project-overwrite")
	command.Flags().BoolVar(&recreate, "recreate", false, "Recreate the target before restore")
	command.Flags().BoolVar(&allowUnverified, "allow-unverified", false, "Allow restoring from a backup that has not passed verification")
	addWaitFlags(command, &wait, &waitTimeout, "restore")
	return command
}

// resolveRestoreTargetKind validates an explicit --target-kind, or infers a safe
// default when one is not given. The backend requires target_kind to be one of
// project|preview|new_preview, so a bare `restore --backup-key X` would otherwise
// fail with a raw 400. Inference: --preview implies preview, --preview-name implies
// new_preview, and the safe non-destructive new_preview is used as the fallback.
func resolveRestoreTargetKind(targetKind, previewRef, previewName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(targetKind)) {
	case "project", "preview", "new_preview":
		return strings.ToLower(strings.TrimSpace(targetKind)), nil
	case "":
		switch {
		case strings.TrimSpace(previewRef) != "":
			return "preview", nil
		case strings.TrimSpace(previewName) != "":
			return "new_preview", nil
		default:
			return "new_preview", nil
		}
	default:
		return "", usageErrorf("--target-kind must be one of project, preview, or new_preview")
	}
}

// restoreSourceDescription names what was restored from, mirroring whichever
// of the mutually exclusive source flags the operator passed.
func restoreSourceDescription(backupKey, restoreTime, restorePointID string) string {
	switch {
	case backupKey != "":
		return "backup " + backupKey
	case restoreTime != "":
		return "the state at " + restoreTime
	case restorePointID != "":
		return "restore point " + restorePointID
	default:
		return "the requested restore source"
	}
}

// restoreOutcome states what is now true after a successful restore, per
// target kind, ending with the command that reaches the restored data.
func restoreOutcome(projectName, targetKind, source, previewID string) string {
	if targetKind == "project" {
		return fmt.Sprintf("Restore complete: the live database for project %s now matches %s.\n", projectName, source) +
			"Connections resume automatically. Verify with `capydb sql \"select 1\"`.\n"
	}
	// preview / new_preview targets leave the live database untouched.
	if previewID != "" {
		return fmt.Sprintf("Restore complete: preview %s now holds the restored data.\n", previewID) +
			fmt.Sprintf("Get its connection string with `capydb connection-string --preview %s`.\n", previewID)
	}
	return "Restore complete: the preview database now holds the restored data.\n" +
		"Find it with `capydb preview list`.\n"
}

// confirmProjectRestoreOverwrite guards the destructive project restore path. The
// --confirm-project-overwrite flag is honoured for non-interactive (CI) usage. On
// an interactive terminal without the flag, the operator is prompted to retype the
// project name (or "yes") to confirm before the destructive restore is sent.
func confirmProjectRestoreOverwrite(cmd *cobra.Command, project api.Project, flagConfirmed bool) (bool, error) {
	return confirmProjectDestructiveAction(cmd, project, flagConfirmed,
		"This will OVERWRITE the live database for project %q (%s).\n")
}

// confirmProjectDestructiveAction is the shared typed-name confirmation for
// actions that overwrite a project's live database. A confirm flag is honoured
// for non-interactive (CI) usage; on an interactive terminal without the flag,
// the operator retypes the project name (or "yes"). The warning format takes
// the project name and id, in that order.
func confirmProjectDestructiveAction(cmd *cobra.Command, project api.Project, flagConfirmed bool, warningFormat string) (bool, error) {
	if flagConfirmed {
		return true, nil
	}
	if !stdinIsInteractive() {
		return false, nil
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), warningFormat, project.Name, project.ID)
	answer, err := promptLine(fmt.Sprintf("Type the project name %q (or \"yes\") to confirm", project.Name))
	if err != nil {
		return false, err
	}
	trimmed := strings.TrimSpace(answer)
	if strings.EqualFold(trimmed, "yes") || trimmed == strings.TrimSpace(project.Name) {
		return true, nil
	}
	return false, nil
}

func (a *app) newRestorePointsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "restore-points",
		Aliases: []string{"restore-point"},
		Short:   "Manage labelled restore points",
	}

	var listProjectRef string
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List restore points for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, listProjectRef)
			if err != nil {
				return err
			}
			points, err := client.ListRestorePoints(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list restore points: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"restore_points": jsonList(points)})
			}
			if len(points) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No restore points for project %s\n", project.Name)
				return nil
			}
			writeRestorePointTable(cmd.OutOrStdout(), points)
			return nil
		},
	}
	listCommand.Flags().StringVar(&listProjectRef, "project", "", "Project id, slug, or name")

	var createProjectRef string
	var createKind string
	var createLabel string
	var createBackupKey string
	var createPITRTime string
	var createNote string
	createCommand := &cobra.Command{
		Use:   "create",
		Short: "Create a labelled restore point",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(createLabel) == "" {
				return usageErrorf("--label is required")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, createProjectRef)
			if err != nil {
				return err
			}
			point, err := client.CreateRestorePoint(ctx, project.ID, api.CreateRestorePointRequest{
				BackupKey: strings.TrimSpace(createBackupKey),
				Kind:      strings.TrimSpace(createKind),
				Label:     strings.TrimSpace(createLabel),
				Note:      strings.TrimSpace(createNote),
				PITRTime:  strings.TrimSpace(createPITRTime),
			})
			if err != nil {
				return fmt.Errorf("create restore point: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"restore_point": point})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created restore point %s (%s)\n", point.ID, point.Label)
			return nil
		},
	}
	createCommand.Flags().StringVar(&createProjectRef, "project", "", "Project id, slug, or name")
	createCommand.Flags().StringVar(&createLabel, "label", "", "Restore point label (required)")
	createCommand.Flags().StringVar(&createKind, "kind", "", "Restore point kind: backup or pitr (inferred if omitted)")
	createCommand.Flags().StringVar(&createBackupKey, "backup-key", "", "Backup key to anchor against (kind=backup)")
	createCommand.Flags().StringVar(&createPITRTime, "pitr-time", "", "RFC3339 timestamp (kind=pitr; defaults to now)")
	createCommand.Flags().StringVar(&createNote, "note", "", "Optional note")

	var deleteProjectRef string
	var deleteRestorePointID string
	deleteCommand := &cobra.Command{
		Use:   "delete",
		Short: "Delete a restore point",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(deleteRestorePointID) == "" {
				return usageErrorf("--id is required")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, deleteProjectRef)
			if err != nil {
				return err
			}
			if err := client.DeleteRestorePoint(ctx, project.ID, strings.TrimSpace(deleteRestorePointID)); err != nil {
				return fmt.Errorf("delete restore point: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted restore point %s\n", deleteRestorePointID)
			return nil
		},
	}
	deleteCommand.Flags().StringVar(&deleteProjectRef, "project", "", "Project id, slug, or name")
	deleteCommand.Flags().StringVar(&deleteRestorePointID, "id", "", "Restore point id")

	command.AddCommand(listCommand)
	command.AddCommand(createCommand)
	command.AddCommand(deleteCommand)
	return command
}

func writeRestorePointTable(out io.Writer, points []api.RestorePoint) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tLABEL\tKIND\tANCHOR\tSTATE\tCREATED")
	for _, point := range points {
		anchor := ""
		switch point.Kind {
		case "backup":
			anchor = point.BackupKey
			if anchor == "" {
				anchor = point.BackupID
			}
		case "pitr":
			if point.PITRTime != nil {
				anchor = point.PITRTime.UTC().Format(time.RFC3339)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			point.ID,
			point.Label,
			point.Kind,
			anchor,
			point.State,
			point.CreatedAt.UTC().Format(time.RFC3339),
		)
	}
	_ = tw.Flush()
}

func (a *app) newJobsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect async jobs",
	}

	var jobID string
	var wait bool
	var waitTimeout time.Duration
	getCommand := &cobra.Command{
		Use:     "get",
		Aliases: []string{"status"},
		Short:   "Get the current state of a job",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if strings.TrimSpace(jobID) == "" {
				return usageErrorf("--job-id is required")
			}

			client, _, err := a.resolveClient(true)
			if err != nil {
				return err
			}

			job, err := client.GetJob(ctx, strings.TrimSpace(jobID))
			if err != nil {
				return fmt.Errorf("get job: %w", err)
			}
			if wait && !jobDone(job) {
				if !a.jsonOutput() {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Waiting for job %s\n", job.ID)
				}
				job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, waitTimeout)
				if err != nil {
					return err
				}
			}

			return a.printJob(cmd, job)
		},
	}
	getCommand.Flags().StringVar(&jobID, "job-id", "", "Job id")
	getCommand.Flags().BoolVar(&wait, "wait", false, "Wait for the job to finish before printing")
	getCommand.Flags().DurationVar(&waitTimeout, "wait-timeout", defaultWaitTimeout, "Maximum time to wait for the job when --wait is set")

	var listProjectRef string
	var listLimit int
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List a project's recent jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if listLimit < 0 {
				return usageErrorf("--limit must be zero or positive")
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, listProjectRef)
			if err != nil {
				return err
			}

			jobs, err := client.ListProjectJobs(ctx, project.ID, listLimit)
			if err != nil {
				return fmt.Errorf("list jobs: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"jobs": jsonList(jobs)})
			}
			if len(jobs) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No jobs for project %s\n", project.Name)
				return nil
			}

			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "CREATED_AT\tTYPE\tSTATE\tATTEMPTS\tDURATION\tJOB_ID")
			for _, job := range jobs {
				_, _ = fmt.Fprintf(
					writer,
					"%s\t%s\t%s\t%d/%d\t%s\t%s\n",
					formatTime(job.CreatedAt),
					jobTypeLabel(job.Type),
					job.State,
					max(job.Attempts, 1),
					job.MaxAttempts,
					formatJobDuration(job),
					job.ID,
				)
			}
			return writer.Flush()
		},
	}
	listCommand.Flags().StringVar(&listProjectRef, "project", "", "Project id, slug, or name")
	listCommand.Flags().IntVar(&listLimit, "limit", 0, "Maximum number of jobs to return (server default when omitted)")

	command.AddCommand(getCommand)
	command.AddCommand(listCommand)
	return command
}

// formatJobDuration renders a terminal job's wall-clock duration, or "-" for
// jobs that never started/finished.
func formatJobDuration(job api.Job) string {
	start := job.CreatedAt
	if job.StartedAt != nil {
		start = *job.StartedAt
	}
	if job.CompletedAt == nil {
		if job.State == "running" || job.State == "pending" {
			return time.Since(start).Round(time.Second).String()
		}
		return "-"
	}
	return job.CompletedAt.Sub(start).Round(time.Second).String()
}

func (a *app) newStudioCommand() *cobra.Command {
	var page string
	var printOnly bool
	var projectRef string

	command := &cobra.Command{
		Use:   "studio",
		Short: "Open the dashboard page for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			var authConfig resolvedAuth
			var projectID string
			var projectSlug string
			var workspaceSlug string
			if strings.TrimSpace(projectRef) == "" {
				linkConfig, linkErr := config.LoadProjectConfig(a.cwd)
				if linkErr != nil {
					if errors.Is(linkErr, os.ErrNotExist) {
						return fmt.Errorf("no linked project found; pass --project or run `capydb link` first")
					}
					return linkErr
				}
				projectID = linkConfig.ProjectID
				projectSlug = linkConfig.ProjectSlug
				authConfig.APIURL = a.resolveAPIURL(linkConfig.APIURL)
				// Best effort: the workspace slug completes the readable URL, but `capydb studio`
				// must keep working from the link file alone when there are no credentials.
				if client, _, clientErr := a.resolveClient(true); clientErr == nil {
					workspaceSlug = lookupWorkspaceSlug(ctx, client)
				}
			} else {
				client, resolved, err := a.resolveClient(true)
				if err != nil {
					return err
				}
				authConfig = resolved
				project, err := a.resolveProject(ctx, client, projectRef)
				if err != nil {
					return err
				}
				projectID = project.ID
				projectSlug = project.Slug
				workspaceSlug = lookupWorkspaceSlug(ctx, client)
			}

			rawURL, err := buildDashboardURL(a.resolveAppURL(authConfig.APIURL), workspaceSlug, projectSlug, projectID, page)
			if err != nil {
				return err
			}

			if a.jsonOutput() {
				if err := printJSON(cmd.OutOrStdout(), map[string]string{"url": rawURL}); err != nil {
					return err
				}
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), rawURL)
			}
			if printOnly {
				return nil
			}
			if err := openURL(rawURL); err != nil {
				return fmt.Errorf("open browser: %w", err)
			}
			return nil
		},
	}

	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&page, "page", "connections", "Dashboard page: overview, connections, studio, previews, backups, or settings")
	command.Flags().BoolVar(&printOnly, "print-only", false, "Print the URL without opening it")
	return command
}

func (a *app) resolveClient(prompt bool, fallbackAPIURL ...string) (*api.Client, resolvedAuth, error) {
	authConfig, err := a.resolveAuth(prompt, fallbackAPIURL...)
	if err != nil {
		return nil, resolvedAuth{}, err
	}

	client, err := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
	if err != nil {
		return nil, resolvedAuth{}, err
	}
	if err := a.persistResolvedAuth(authConfig); err != nil {
		return nil, resolvedAuth{}, err
	}
	return client, authConfig, nil
}

func (a *app) linkedProjectAPIURL() string {
	linkConfig, err := config.LoadProjectConfig(a.cwd)
	if err != nil {
		return ""
	}
	return linkConfig.APIURL
}

func (a *app) resolveProject(ctx context.Context, client *api.Client, ref string) (api.Project, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		linkConfig, err := config.LoadProjectConfig(a.cwd)
		if err == nil && strings.TrimSpace(linkConfig.ProjectID) != "" {
			project, _, err := client.GetProject(ctx, linkConfig.ProjectID)
			if err != nil {
				return api.Project{}, fmt.Errorf("fetch linked project: %w", err)
			}
			return project, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return api.Project{}, err
		}
	}

	projects, err := client.ListProjects(ctx, "")
	if err != nil {
		return api.Project{}, fmt.Errorf("list projects: %w", err)
	}
	if len(projects) == 0 {
		return api.Project{}, fmt.Errorf("no projects available for this api key")
	}

	if trimmed == "" {
		if len(projects) == 1 {
			return projects[0], nil
		}
		if stdinIsInteractive() {
			return selectProject(projects)
		}
		return api.Project{}, fmt.Errorf("multiple projects are available; pass --project or run the command inside a linked project")
	}

	var matches []api.Project
	for _, project := range projects {
		switch {
		case project.ID == trimmed:
			return project, nil
		case strings.EqualFold(project.Slug, trimmed):
			matches = append(matches, project)
		case strings.EqualFold(project.Name, trimmed):
			matches = append(matches, project)
		}
	}

	switch len(matches) {
	case 0:
		return api.Project{}, notFoundErrorf("project %q not found", trimmed)
	case 1:
		return matches[0], nil
	default:
		if stdinIsInteractive() {
			return selectProject(matches)
		}
		return api.Project{}, fmt.Errorf("project %q is ambiguous; use the project id instead", trimmed)
	}
}

func (a *app) resolvePreview(ctx context.Context, client *api.Client, projectID, ref string) (api.PreviewDetails, error) {
	previews, err := client.ListPreviewDatabases(ctx, projectID)
	if err != nil {
		return api.PreviewDetails{}, fmt.Errorf("list previews: %w", err)
	}
	if len(previews) == 0 {
		return api.PreviewDetails{}, fmt.Errorf("no preview databases found")
	}

	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		if len(previews) == 1 {
			return previews[0], nil
		}
		return api.PreviewDetails{}, fmt.Errorf("multiple preview databases are available; pass --preview")
	}

	var matches []api.PreviewDetails
	for _, preview := range previews {
		switch {
		case preview.Preview.ID == trimmed:
			return preview, nil
		case strings.EqualFold(preview.Preview.Name, trimmed):
			matches = append(matches, preview)
		}
	}

	switch len(matches) {
	case 0:
		return api.PreviewDetails{}, notFoundErrorf("preview %q not found", trimmed)
	case 1:
		return matches[0], nil
	default:
		return api.PreviewDetails{}, fmt.Errorf("preview %q is ambiguous; use the preview id instead", trimmed)
	}
}

func (a *app) resolveAppURL(apiURL string) string {
	if value := strings.TrimSpace(a.appURL); value != "" {
		return strings.TrimRight(value, "/")
	}
	if value := strings.TrimSpace(os.Getenv("CAPYDB_APP_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	userConfig, err := config.LoadUserConfig()
	if err == nil {
		if _, entry, ok := userConfig.Active(); ok && strings.TrimSpace(entry.AppURL) != "" {
			return strings.TrimRight(strings.TrimSpace(entry.AppURL), "/")
		}
	}
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if trimmed, ok := strings.CutSuffix(apiURL, "/api/capydb"); ok {
		return trimmed
	}
	return apiURL
}

// lookupWorkspaceSlug returns the dashboard's workspace segment for the caller's
// organization, or "" when it cannot be determined. The dashboard addresses a
// workspace by its Clerk organization slug.
func lookupWorkspaceSlug(ctx context.Context, client *api.Client) string {
	viewer, err := client.GetViewer(ctx)
	if err != nil || viewer.Organization == nil {
		return ""
	}
	return strings.TrimSpace(viewer.Organization.ClerkOrganizationSlug)
}

// buildDashboardURL builds the readable dashboard URL
// (/dashboard/<workspace>/<project>/<page>) whenever both slugs are known. The
// id form is kept as the fallback: the dashboard still resolves it server-side
// and forwards, so an unauthenticated or partially-linked CLI still opens the
// right page.
func buildDashboardURL(appURL, workspaceSlug, projectSlug, projectID, page string) (string, error) {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	if appURL == "" {
		return "", fmt.Errorf("could not determine the dashboard URL; set CAPYDB_APP_URL")
	}

	workspaceSlug = strings.TrimSpace(workspaceSlug)
	projectSlug = strings.TrimSpace(projectSlug)
	basePath := "/dashboard/projects/" + strings.TrimSpace(projectID)
	if workspaceSlug != "" && projectSlug != "" {
		basePath = "/dashboard/" + url.PathEscape(workspaceSlug) + "/" + url.PathEscape(projectSlug)
	}
	switch strings.ToLower(strings.TrimSpace(page)) {
	case "", "overview":
		return appURL + basePath, nil
	case "connections", "studio", "previews", "backups", "settings":
		return appURL + basePath + "/" + strings.ToLower(strings.TrimSpace(page)), nil
	default:
		return "", usageErrorf("page must be one of overview, connections, studio, previews, backups, or settings")
	}
}

// addWaitFlags registers the paired --wait / --wait-timeout flags used by every
// command that queues an async job.
func addWaitFlags(command *cobra.Command, wait *bool, waitTimeout *time.Duration, action string) {
	command.Flags().BoolVar(wait, "wait", false, "Wait for the "+action+" job to finish")
	command.Flags().DurationVar(waitTimeout, "wait-timeout", defaultWaitTimeout, "Maximum time to wait for the job when --wait is set")
}

func (a *app) maybeWaitForJob(cmd *cobra.Command, client *api.Client, job api.Job, wait bool, waitTimeout time.Duration, action string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if !wait || job.ID == "" || jobDone(job) {
		return a.printJob(cmd, job)
	}

	if !a.jsonOutput() {
		_ = a.printJob(cmd, job)
		_, _ = fmt.Fprintf(out, "Waiting for job %s\n", job.ID)
	}
	job, err := waitForJob(ctx, errOut, client, job.ID, waitTimeout)
	if err != nil {
		return err
	}
	if err := a.printJob(cmd, job); err != nil {
		return err
	}
	return ensureCompletedJob(job, action)
}

// printJob renders a job to stdout, as JSON in JSON output mode.
func (a *app) printJob(cmd *cobra.Command, job api.Job) error {
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), job)
	}
	writeJob(cmd.OutOrStdout(), job)
	return nil
}

func ensureCompletedJob(job api.Job, action string) error {
	if job.State != "completed" {
		return fmt.Errorf("%s ended in %s: %s", action, job.State, job.Error)
	}
	return nil
}

func selectProject(projects []api.Project) (api.Project, error) {
	fmt.Println("Select a project:")
	for index, project := range projects {
		fmt.Printf("  %d. %s (%s, %s)\n", index+1, project.Name, firstNonEmpty(project.Slug, "-"), project.ID)
	}

	value, err := promptLine("Project number")
	if err != nil {
		return api.Project{}, err
	}
	selection, err := strconv.Atoi(value)
	if err != nil || selection < 1 || selection > len(projects) {
		return api.Project{}, fmt.Errorf("invalid project selection")
	}
	return projects[selection-1], nil
}

func jobDone(job api.Job) bool {
	switch job.State {
	case "completed", "failed":
		return true
	default:
		return false
	}
}

func writeBackupTable(out io.Writer, backups []api.Backup) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tLABEL\tSTATE\tVERIFY\tSIZE\tCREATED_AT\tBACKUP_KEY")
	for _, backup := range backups {
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			backup.ID,
			firstNonEmpty(backup.Label, "-"),
			backup.State,
			firstNonEmpty(backup.VerificationState, "-"),
			formatBytes(backup.SizeBytes),
			formatTime(backup.CreatedAt),
			backup.BackupKey,
		)
	}
	_ = writer.Flush()
}

// jobTypeLabels maps internal job-kind tokens to the customer-facing action
// labels used across every surface (mirrors frontend display.ts). Text output
// must never print raw kinds like "instance.create" or "branch.create"; JSON
// output keeps the raw API values for machines.
var jobTypeLabels = map[string]string{
	"project.apply_plan":            "Applying configuration",
	"project.backup":                "Creating backup",
	"project.backup_delete":         "Deleting backup",
	"project.import":                "Importing data",
	"project.import_follow_start":   "Starting live import",
	"project.import_follow_status":  "Checking import progress",
	"project.import_follow_cutover": "Finalizing import",
	"project.import_follow_abort":   "Canceling import",
	"project.restore":               "Restoring database",
	"project.rotate_credentials":    "Rotating credentials",
	"project.promote_preview":       "Promoting preview",
	"project.extension_enable":      "Enabling extension",
	"project.extension_disable":     "Disabling extension",
	"integration.sync_env":          "Syncing environment variables",
	"integration.clerk_backfill":    "Syncing account",
	"instance.create":               "Provisioning database",
	"instance.destroy":              "Deleting database",
	"instance.sleep":                "Pausing database",
	"instance.wake":                 "Resuming database",
	"instance.seed_standby":         "Setting up replication",
	"instance.pitr_restore":         "Restoring to point in time",
	"instance.set_readonly":         "Updating database access mode",
	"branch.create":                 "Creating preview",
	"host.health_probe":             "Health check",
}

// jobTypeLabel converts an internal job kind into its customer-facing label,
// falling back to a readable sentence-cased form for unknown kinds.
func jobTypeLabel(jobType string) string {
	trimmed := strings.TrimSpace(jobType)
	if trimmed == "" {
		return "-"
	}
	if label, ok := jobTypeLabels[trimmed]; ok {
		return label
	}
	words := strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(trimmed))
	if len(words) == 0 {
		return trimmed
	}
	sentence := strings.ToLower(strings.Join(words, " "))
	return strings.ToUpper(sentence[:1]) + sentence[1:]
}

func writeJob(out io.Writer, job api.Job) {
	_, _ = fmt.Fprintf(out, "job_id: %s\n", job.ID)
	_, _ = fmt.Fprintf(out, "type: %s\n", jobTypeLabel(job.Type))
	_, _ = fmt.Fprintf(out, "state: %s\n", firstNonEmpty(job.State, "-"))
	if strings.TrimSpace(job.ProjectID) != "" {
		_, _ = fmt.Fprintf(out, "project_id: %s\n", job.ProjectID)
	}
	if strings.TrimSpace(job.Error) != "" {
		_, _ = fmt.Fprintf(out, "error: %s\n", job.Error)
	}
}

func writePreviewTable(out io.Writer, previews []api.PreviewDetails) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tNAME\tMODE\tSTATE\tTTL_EXPIRES_AT\tDIRECT_URL\tPOOLED_URL")
	for _, preview := range previews {
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			preview.Preview.ID,
			preview.Preview.Name,
			preview.Preview.Mode,
			preview.Preview.State,
			formatTime(preview.Preview.TTLExpiresAt),
			firstNonEmpty(preview.Connections.DirectURL, "-"),
			firstNonEmpty(preview.Connections.PooledURL, "-"),
		)
	}
	_ = writer.Flush()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func formatBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	const unit = 1024
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	index := 0
	for size >= unit && index < len(units)-1 {
		size /= unit
		index++
	}
	if size >= 10 || index == 0 {
		return fmt.Sprintf("%.0f %s", size, units[index])
	}
	return fmt.Sprintf("%.1f %s", size, units[index])
}

func openURL(rawURL string) error {
	var command *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", rawURL)
	case "linux":
		command = exec.Command("xdg-open", rawURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
