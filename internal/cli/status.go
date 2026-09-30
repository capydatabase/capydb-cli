package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

type statusOptions struct {
	projectRef string
	remote     bool
	usage      bool
}

type statusOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

type statusLinkedProject struct {
	ProjectID     string `json:"project_id"`
	ProjectName   string `json:"project_name"`
	ProjectSlug   string `json:"project_slug,omitempty"`
	AppPath       string `json:"app_path"`
	Profile       string `json:"profile,omitempty"`
	Framework     string `json:"framework,omitempty"`
	DatabaseLayer string `json:"database_layer,omitempty"`
	EnvFile       string `json:"env_file"`
}

type statusAPIComponent struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type statusAPIHealth struct {
	Status     string               `json:"status"`
	CheckedAt  *time.Time           `json:"checked_at,omitempty"`
	Error      string               `json:"error,omitempty"`
	Components []statusAPIComponent `json:"components"`
}

type statusRemoteProject struct {
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	State              string                    `json:"state"`
	Environment        string                    `json:"environment,omitempty"`
	PostgresVersion    string                    `json:"postgres_version,omitempty"`
	PostgresChannel    string                    `json:"postgres_channel,omitempty"`
	PostgresWarning    string                    `json:"postgres_warning,omitempty"`
	LastError          string                    `json:"last_error,omitempty"`
	LatestJob          *api.Job                  `json:"latest_job,omitempty"`
	LatestJobError     string                    `json:"latest_job_error,omitempty"`
	Observability      *api.ProjectObservability `json:"observability,omitempty"`
	ObservabilityError string                    `json:"observability_error,omitempty"`
	BackupCount        *int                      `json:"backup_count,omitempty"`
	LatestBackup       *api.Backup               `json:"latest_backup,omitempty"`
	BackupsError       string                    `json:"backups_error,omitempty"`
}

type statusReport struct {
	LoggedIn      bool                 `json:"logged_in"`
	APIURL        string               `json:"api_url,omitempty"`
	AppURL        string               `json:"app_url,omitempty"`
	Organization  *statusOrganization  `json:"organization"`
	LinkedProject *statusLinkedProject `json:"linked_project"`
	API           *statusAPIHealth     `json:"api,omitempty"`
	RemoteProject *statusRemoteProject `json:"remote_project,omitempty"`
	Usage         *statusUsage         `json:"usage,omitempty"`
}

func (a *app) runStatus(cmd *cobra.Command, options statusOptions) error {
	report, err := a.buildStatusReport(cmd, options)
	if err != nil {
		return err
	}

	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), report)
	}
	writeStatusReport(cmd, report, options)
	if report.Usage != nil {
		writeStatusUsage(cmd.OutOrStdout(), *report.Usage)
	}
	return nil
}

func (a *app) buildStatusReport(cmd *cobra.Command, options statusOptions) (statusReport, error) {
	ctx := cmd.Context()
	userConfig, userErr := config.LoadUserConfig()
	projectConfig, projectErr := config.LoadProjectConfig(a.cwd)

	if userErr != nil && !errors.Is(userErr, os.ErrNotExist) {
		return statusReport{}, userErr
	}
	if projectErr != nil && !errors.Is(projectErr, os.ErrNotExist) {
		return statusReport{}, projectErr
	}

	report := statusReport{}
	activeOrgID, activeEntry, hasActive := userConfig.Active()
	if hasActive && strings.TrimSpace(activeEntry.APIKey) != "" {
		report.LoggedIn = true
		report.APIURL = userConfig.APIURL()
		report.AppURL = userConfig.AppURL()
		if activeOrgID != config.DefaultOrgKey {
			report.Organization = &statusOrganization{
				ID:   activeOrgID,
				Name: activeEntry.Name,
				Slug: activeEntry.Slug,
			}
		}
	}

	linked := !errors.Is(projectErr, os.ErrNotExist)
	if linked {
		report.LinkedProject = &statusLinkedProject{
			ProjectID:     projectConfig.ProjectID,
			ProjectName:   projectConfig.ProjectName,
			ProjectSlug:   projectConfig.ProjectSlug,
			AppPath:       firstNonEmpty(projectConfig.AppPath, "."),
			Profile:       firstNonEmpty(projectConfig.Profile, projectConfig.Framework),
			Framework:     projectConfig.Framework,
			DatabaseLayer: projectConfig.DatabaseLayer,
			EnvFile:       projectConfig.EnvFile,
		}
	}

	if options.usage && !linked && strings.TrimSpace(options.projectRef) == "" {
		return statusReport{}, usageErrorf("--usage needs a linked project or --project")
	}
	if !options.remote && !options.usage && strings.TrimSpace(options.projectRef) == "" {
		return report, nil
	}

	apiURL := a.resolveAPIURL(firstNonEmpty(projectConfig.APIURL, report.APIURL))
	anonymousClient, err := a.newAPIClient(apiURL, "")
	if err != nil {
		return statusReport{}, err
	}
	report.API = &statusAPIHealth{Components: []statusAPIComponent{}}
	publicStatus, err := anonymousClient.GetPublicStatus(ctx)
	if err != nil {
		report.API.Status = "unavailable"
		report.API.Error = err.Error()
	} else {
		report.API.Status = firstNonEmpty(publicStatus.Status, "-")
		if !publicStatus.UpdatedAt.IsZero() {
			checkedAt := publicStatus.UpdatedAt
			report.API.CheckedAt = &checkedAt
		}
		for _, component := range publicStatus.Components {
			report.API.Components = append(report.API.Components, statusAPIComponent{
				Name:    component.Name,
				Status:  component.Status,
				Message: component.Message,
			})
		}
	}

	if strings.TrimSpace(options.projectRef) == "" && !linked {
		return report, nil
	}

	client, _, err := a.resolveClient(false, apiURL)
	if err != nil {
		return statusReport{}, err
	}

	project, err := a.resolveProject(ctx, client, options.projectRef)
	if err != nil {
		return statusReport{}, err
	}

	remote := &statusRemoteProject{
		ID:          project.ID,
		Name:        project.Name,
		State:       firstNonEmpty(project.State, "-"),
		Environment: project.Environment,
		LastError:   project.LastError,

		PostgresVersion: project.PostgresVersion,
		PostgresChannel: project.PostgresChannel,
		PostgresWarning: project.PostgresWarning,
	}
	report.RemoteProject = remote

	if strings.TrimSpace(project.LatestJobID) != "" {
		job, err := client.GetJob(ctx, project.LatestJobID)
		if err != nil {
			remote.LatestJobError = err.Error()
		} else {
			remote.LatestJob = &job
		}
	}

	if options.usage {
		usage := buildStatusUsage(ctx, client, project)
		report.Usage = &usage
		// The usage section reads the live figures itself (and skips them
		// while the database is paused); reading observability again below
		// would wake a paused database.
		return report, nil
	}

	observability, err := client.GetProjectObservability(ctx, project.ID)
	if err != nil {
		remote.ObservabilityError = err.Error()
	} else {
		remote.Observability = &observability
	}

	backups, err := client.ListBackups(ctx, project.ID)
	if err != nil {
		remote.BackupsError = err.Error()
		return report, nil
	}
	count := len(backups)
	remote.BackupCount = &count
	if latest, ok := latestBackup(backups); ok {
		remote.LatestBackup = &latest
	}
	return report, nil
}

func writeStatusReport(cmd *cobra.Command, report statusReport, options statusOptions) {
	out := cmd.OutOrStdout()

	if report.LoggedIn {
		_, _ = fmt.Fprintf(out, "logged_in: yes\n")
		_, _ = fmt.Fprintf(out, "api_url: %s\n", report.APIURL)
		_, _ = fmt.Fprintf(out, "app_url: %s\n", report.AppURL)
		if report.Organization != nil {
			_, _ = fmt.Fprintf(out, "organization_id: %s\n", report.Organization.ID)
			_, _ = fmt.Fprintf(out, "organization_name: %s\n", firstNonEmpty(report.Organization.Name, "-"))
			_, _ = fmt.Fprintf(out, "organization_slug: %s\n", firstNonEmpty(report.Organization.Slug, "-"))
		}
	} else {
		_, _ = fmt.Fprintf(out, "logged_in: no\n")
	}

	if report.LinkedProject == nil {
		_, _ = fmt.Fprintf(out, "linked_project: no\n")
	} else {
		linkedProject := report.LinkedProject
		_, _ = fmt.Fprintf(out, "linked_project: yes\n")
		_, _ = fmt.Fprintf(out, "project_id: %s\n", linkedProject.ProjectID)
		_, _ = fmt.Fprintf(out, "project_name: %s\n", linkedProject.ProjectName)
		if strings.TrimSpace(linkedProject.ProjectSlug) != "" {
			_, _ = fmt.Fprintf(out, "project_slug: %s\n", linkedProject.ProjectSlug)
		}
		_, _ = fmt.Fprintf(out, "app_path: %s\n", linkedProject.AppPath)
		_, _ = fmt.Fprintf(out, "profile: %s\n", linkedProject.Profile)
		_, _ = fmt.Fprintf(out, "framework: %s\n", firstNonEmpty(linkedProject.Framework, "-"))
		_, _ = fmt.Fprintf(out, "database_layer: %s\n", firstNonEmpty(linkedProject.DatabaseLayer, "-"))
		_, _ = fmt.Fprintf(out, "env_file: %s\n", linkedProject.EnvFile)
	}

	if report.API == nil {
		return
	}

	_, _ = fmt.Fprintf(out, "api_status: %s\n", report.API.Status)
	if strings.TrimSpace(report.API.Error) != "" {
		_, _ = fmt.Fprintf(out, "api_status_error: %s\n", report.API.Error)
	}
	if report.API.CheckedAt != nil {
		_, _ = fmt.Fprintf(out, "api_checked_at: %s\n", formatTime(*report.API.CheckedAt))
	}
	for _, component := range report.API.Components {
		key := strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(component.Name))
		_, _ = fmt.Fprintf(out, "api_component_%s: %s\n", key, firstNonEmpty(component.Status, "-"))
		if strings.TrimSpace(component.Message) != "" {
			_, _ = fmt.Fprintf(out, "api_component_%s_message: %s\n", key, component.Message)
		}
	}

	if report.RemoteProject == nil {
		if strings.TrimSpace(options.projectRef) == "" && report.LinkedProject == nil {
			_, _ = fmt.Fprintf(out, "remote_project: not_checked\n")
		}
		return
	}

	remote := report.RemoteProject
	_, _ = fmt.Fprintf(out, "remote_project: yes\n")
	_, _ = fmt.Fprintf(out, "remote_project_id: %s\n", remote.ID)
	_, _ = fmt.Fprintf(out, "remote_project_name: %s\n", remote.Name)
	_, _ = fmt.Fprintf(out, "remote_project_state: %s\n", remote.State)
	_, _ = fmt.Fprintf(out, "remote_project_environment: %s\n", firstNonEmpty(remote.Environment, "-"))
	if remote.PostgresVersion != "" {
		_, _ = fmt.Fprintf(out, "remote_project_postgres: %s\n", postgresLabel(remote.PostgresVersion, remote.PostgresChannel))
	}
	if strings.TrimSpace(remote.PostgresWarning) != "" {
		_, _ = fmt.Fprintf(out, "remote_project_postgres_warning: %s\n", remote.PostgresWarning)
	}
	if strings.TrimSpace(remote.LastError) != "" {
		_, _ = fmt.Fprintf(out, "remote_project_error: %s\n", remote.LastError)
	}

	if strings.TrimSpace(remote.LatestJobError) != "" {
		_, _ = fmt.Fprintf(out, "latest_job_error: %s\n", remote.LatestJobError)
	} else if remote.LatestJob != nil {
		_, _ = fmt.Fprintf(out, "latest_job_id: %s\n", remote.LatestJob.ID)
		_, _ = fmt.Fprintf(out, "latest_job_type: %s\n", firstNonEmpty(remote.LatestJob.Type, "-"))
		_, _ = fmt.Fprintf(out, "latest_job_state: %s\n", firstNonEmpty(remote.LatestJob.State, "-"))
	}

	if strings.TrimSpace(remote.ObservabilityError) != "" {
		_, _ = fmt.Fprintf(out, "observability_error: %s\n", remote.ObservabilityError)
	} else if remote.Observability != nil {
		writeProjectObservability(cmd, *remote.Observability)
	}

	if strings.TrimSpace(remote.BackupsError) != "" {
		_, _ = fmt.Fprintf(out, "backups_error: %s\n", remote.BackupsError)
		return
	}
	if remote.BackupCount != nil {
		_, _ = fmt.Fprintf(out, "backup_count: %d\n", *remote.BackupCount)
	}
	if remote.LatestBackup != nil {
		_, _ = fmt.Fprintf(out, "latest_backup_id: %s\n", remote.LatestBackup.ID)
		_, _ = fmt.Fprintf(out, "latest_backup_state: %s\n", firstNonEmpty(remote.LatestBackup.State, "-"))
		_, _ = fmt.Fprintf(out, "latest_backup_verify: %s\n", firstNonEmpty(remote.LatestBackup.VerificationState, "-"))
		_, _ = fmt.Fprintf(out, "latest_backup_created_at: %s\n", formatTime(remote.LatestBackup.CreatedAt))
	}
}

func writeProjectObservability(cmd *cobra.Command, observability api.ProjectObservability) {
	_, _ = fmt.Fprintf(
		cmd.OutOrStdout(),
		"connections: %d/%d\n",
		observability.ConnectionCount,
		observability.ConnectionLimit,
	)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "connection_usage: %s\n", formatPercent(observability.ConnectionUsagePercent))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "database_size: %s\n", formatBytes(observability.DatabaseSizeBytes))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "storage_limit: %s\n", formatBytes(observability.StorageLimitBytes))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "storage_usage: %s\n", formatPercent(observability.StorageUsagePercent))
	for _, alert := range observability.Alerts {
		if strings.TrimSpace(alert) != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "alert: %s\n", alert)
		}
	}
}

func latestBackup(backups []api.Backup) (api.Backup, bool) {
	if len(backups) == 0 {
		return api.Backup{}, false
	}

	latest := backups[0]
	for _, backup := range backups[1:] {
		if backup.CreatedAt.After(latest.CreatedAt) {
			latest = backup
		}
	}
	return latest, true
}

func formatPercent(value float64) string {
	return fmt.Sprintf("%.1f%%", value)
}
