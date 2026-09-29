package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// usageWarnPercent is where a meter reads as "getting close". It matches the
// connection alert the control plane raises.
const usageWarnPercent = 80.0

// statusUsageMeter is one used-of-limit figure. Limit 0 means the plan sets
// none (or it is not known).
type statusUsageMeter struct {
	Used    int64    `json:"used"`
	Limit   int64    `json:"limit"`
	Percent *float64 `json:"percent,omitempty"`
}

type statusUsage struct {
	Plan          string            `json:"plan"`
	BillingPlan   string            `json:"billing_plan,omitempty"`
	BillingStatus string            `json:"billing_status,omitempty"`
	RuntimeStatus string            `json:"runtime_status,omitempty"`
	AlwaysOn      bool              `json:"always_on"`
	Storage       *statusUsageMeter `json:"storage,omitempty"`
	Connections   *statusUsageMeter `json:"connections,omitempty"`
	// LiveSkipped is set when the database is paused: storage and
	// connections are read from the running database, and reading them would
	// wake it.
	LiveSkipped      bool     `json:"live_skipped"`
	LiveError        string   `json:"live_error,omitempty"`
	Previews         *int     `json:"previews,omitempty"`
	PreviewsError    string   `json:"previews_error,omitempty"`
	Backups          *int     `json:"backups,omitempty"`
	BackupBytes      *int64   `json:"backup_bytes,omitempty"`
	BackupsError     string   `json:"backups_error,omitempty"`
	Alerts           []string `json:"alerts"`
	Notes            []string `json:"notes"`
	OrganizationName string   `json:"organization,omitempty"`
}

func newUsageMeter(used, limit int64) *statusUsageMeter {
	meter := &statusUsageMeter{Used: used, Limit: limit}
	if limit > 0 {
		percent := float64(used) / float64(limit) * 100
		meter.Percent = &percent
	}
	return meter
}

// buildStatusUsage gathers what the project consumes against its plan from
// endpoints that already exist. Each source fails independently: a missing
// piece is reported, never turned into a failed command.
func buildStatusUsage(ctx context.Context, client *api.Client, project api.Project) statusUsage {
	usage := statusUsage{
		Plan:          firstNonEmpty(project.Plan, "-"),
		RuntimeStatus: project.RuntimeStatus,
		AlwaysOn:      project.AlwaysOn,
		Alerts:        []string{},
		Notes:         []string{},
	}

	if viewer, err := client.GetViewer(ctx); err == nil && viewer.Organization != nil {
		usage.BillingPlan = viewer.Organization.BillingPlan
		usage.BillingStatus = viewer.Organization.BillingStatus
		usage.OrganizationName = firstNonEmpty(viewer.Organization.Name, viewer.Organization.Slug)
	}

	if project.RuntimeStatus == "paused" {
		usage.LiveSkipped = true
		usage.Storage = &statusUsageMeter{Limit: project.StorageLimitBytes}
		usage.Notes = append(usage.Notes, "the database is paused; storage and connection counts are read from the running database, so they were not read (that would wake it)")
	} else if observability, err := client.GetProjectObservability(ctx, project.ID); err != nil {
		usage.LiveError = err.Error()
	} else {
		usage.Storage = newUsageMeter(observability.DatabaseSizeBytes, observability.StorageLimitBytes)
		usage.Connections = newUsageMeter(int64(observability.ConnectionCount), int64(observability.ConnectionLimit))
		usage.Alerts = jsonList(observability.Alerts)
	}

	if previews, err := client.ListPreviewDatabases(ctx, project.ID); err != nil {
		usage.PreviewsError = err.Error()
	} else {
		count := len(previews)
		usage.Previews = &count
	}

	if backups, err := client.ListBackups(ctx, project.ID); err != nil {
		usage.BackupsError = err.Error()
	} else {
		count := len(backups)
		var total int64
		for _, backup := range backups {
			total += backup.SizeBytes
		}
		usage.Backups = &count
		usage.BackupBytes = &total
	}

	if meter := usage.Storage; meter != nil && meter.Percent != nil && *meter.Percent >= usageWarnPercent {
		usage.Notes = append(usage.Notes, fmt.Sprintf("storage is at %.0f%% of the storage included in the plan; clean up data or move to a larger plan", *meter.Percent))
	}
	if meter := usage.Connections; meter != nil && meter.Percent != nil && *meter.Percent >= usageWarnPercent {
		usage.Notes = append(usage.Notes, fmt.Sprintf("connections are at %.0f%% of the limit; route application traffic through the pooled URL", *meter.Percent))
	}
	return usage
}

func writeStatusUsage(out io.Writer, usage statusUsage) {
	_, _ = fmt.Fprintln(out, "\nUsage")
	plan := usage.Plan
	if usage.BillingPlan != "" && !strings.EqualFold(usage.BillingPlan, usage.Plan) {
		plan += fmt.Sprintf(" (organization plan %s)", usage.BillingPlan)
	}
	if usage.BillingStatus != "" {
		plan += ", billing " + usage.BillingStatus
	}
	_, _ = fmt.Fprintf(out, "  plan:        %s\n", plan)

	state := firstNonEmpty(usage.RuntimeStatus, "-")
	if usage.AlwaysOn {
		state += ", always on"
	}
	_, _ = fmt.Fprintf(out, "  database:    %s\n", state)

	switch {
	case usage.LiveError != "":
		_, _ = fmt.Fprintf(out, "  storage:     unavailable (%s)\n", usage.LiveError)
	case usage.LiveSkipped:
		_, _ = fmt.Fprintf(out, "  storage:     not read while paused (limit %s)\n", formatLimit(usage.Storage.Limit))
	default:
		_, _ = fmt.Fprintf(out, "  storage:     %s\n", describeMeter(usage.Storage, formatBytes))
		_, _ = fmt.Fprintf(out, "  connections: %s\n", describeMeter(usage.Connections, func(v int64) string { return fmt.Sprintf("%d", v) }))
	}

	if usage.PreviewsError != "" {
		_, _ = fmt.Fprintf(out, "  previews:    unavailable (%s)\n", usage.PreviewsError)
	} else if usage.Previews != nil {
		_, _ = fmt.Fprintf(out, "  previews:    %d active\n", *usage.Previews)
	}
	if usage.BackupsError != "" {
		_, _ = fmt.Fprintf(out, "  backups:     unavailable (%s)\n", usage.BackupsError)
	} else if usage.Backups != nil {
		_, _ = fmt.Fprintf(out, "  backups:     %d retained, %s\n", *usage.Backups, formatBytes(*usage.BackupBytes))
	}
	for _, alert := range usage.Alerts {
		_, _ = fmt.Fprintf(out, "  alert:       %s\n", alert)
	}
	for _, note := range usage.Notes {
		_, _ = fmt.Fprintf(out, "  - %s\n", note)
	}
}

func describeMeter(meter *statusUsageMeter, format func(int64) string) string {
	if meter == nil {
		return "-"
	}
	if meter.Limit <= 0 || meter.Percent == nil {
		return format(meter.Used) + " (no plan limit)"
	}
	return fmt.Sprintf("%s of %s (%.1f%%)", format(meter.Used), format(meter.Limit), *meter.Percent)
}

func formatLimit(limit int64) string {
	if limit <= 0 {
		return "none"
	}
	return formatBytes(limit)
}
