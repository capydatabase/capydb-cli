package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func (a *app) newNotificationsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "notifications",
		Short: "Show and set which notification emails the organization receives",
		Long: `Alert emails (usage, backup and reachability alerts) and billing notices go to the organization's
billing email when one is set; the recipient lists here add to it. Alert emails can be switched
off; billing notices (suspension, read-only, offline, restored) cannot - only their extra
recipients are configurable.`,
	}
	command.AddCommand(a.newNotificationsShowCommand(), a.newNotificationsSetCommand())
	return command
}

func (a *app) newNotificationsShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the organization's notification preferences",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, authConfig, err := a.resolveClient(true)
			if err != nil {
				return err
			}
			orgID, err := a.resolveOrgID(ctx, client, authConfig)
			if err != nil {
				return err
			}
			preferences, err := client.GetNotificationPreferences(ctx, orgID)
			if err != nil {
				return fmt.Errorf("get notification preferences: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"notification_preferences": preferences})
			}
			writeNotificationPreferences(cmd.OutOrStdout(), preferences)
			return nil
		},
	}
}

func (a *app) newNotificationsSetCommand() *cobra.Command {
	var alertEmails string
	var alertRecipients []string
	var billingRecipients []string

	command := &cobra.Command{
		Use:   "set",
		Short: "Change the organization's notification preferences",
		Long: `Changes only the settings passed; the others keep their current values. A recipient list replaces
the current one (at most 10 addresses each); pass an empty value to clear it:

  capydb notifications set --alert-emails off
  capydb notifications set --alert-recipients ops@example.com,oncall@example.com
  capydb notifications set --billing-recipients ""

Needs an organization admin, or an organization-wide API key an admin created.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			flags := cmd.Flags()
			if !flags.Changed("alert-emails") && !flags.Changed("alert-recipients") && !flags.Changed("billing-recipients") {
				return usageErrorf("pass at least one of --alert-emails, --alert-recipients, or --billing-recipients")
			}
			var alertsOn bool
			if flags.Changed("alert-emails") {
				switch strings.ToLower(strings.TrimSpace(alertEmails)) {
				case "on":
					alertsOn = true
				case "off":
					alertsOn = false
				default:
					return usageErrorf("--alert-emails must be on or off")
				}
			}

			client, authConfig, err := a.resolveClient(true)
			if err != nil {
				return err
			}
			orgID, err := a.resolveOrgID(ctx, client, authConfig)
			if err != nil {
				return err
			}
			// The endpoint is a full replace, so the current values fill in
			// every setting this invocation does not change.
			current, err := client.GetNotificationPreferences(ctx, orgID)
			if err != nil {
				return fmt.Errorf("get notification preferences: %w", err)
			}
			request := api.PutNotificationPreferencesRequest{
				AlertEmailsEnabled:     current.AlertEmailsEnabled,
				AlertEmailRecipients:   current.AlertEmailRecipients,
				BillingEmailRecipients: current.BillingEmailRecipients,
			}
			if flags.Changed("alert-emails") {
				request.AlertEmailsEnabled = alertsOn
			}
			if flags.Changed("alert-recipients") {
				request.AlertEmailRecipients = trimmedValues(alertRecipients)
			}
			if flags.Changed("billing-recipients") {
				request.BillingEmailRecipients = trimmedValues(billingRecipients)
			}

			saved, err := client.PutNotificationPreferences(ctx, orgID, request)
			if err != nil {
				return fmt.Errorf("save notification preferences: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"notification_preferences": saved})
			}
			writeNotificationPreferences(cmd.OutOrStdout(), saved)
			return nil
		},
	}
	command.Flags().StringVar(&alertEmails, "alert-emails", "", "Send usage, backup and reachability alert emails: on or off")
	command.Flags().StringSliceVar(&alertRecipients, "alert-recipients", nil, "Extra recipients of alert emails, comma-separated (replaces the list; \"\" clears it)")
	command.Flags().StringSliceVar(&billingRecipients, "billing-recipients", nil, "Extra recipients of billing notices, comma-separated (replaces the list; \"\" clears it)")
	return command
}

// trimmedValues drops blanks and surrounding space from a comma-separated
// flag's values.
func trimmedValues(values []string) []string {
	trimmed := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func writeNotificationPreferences(out io.Writer, preferences api.NotificationPreferences) {
	alerts := "off"
	if preferences.AlertEmailsEnabled {
		alerts = "on"
	}
	recipients := func(list []string) string {
		if len(list) == 0 {
			return "-"
		}
		return strings.Join(list, ", ")
	}
	_, _ = fmt.Fprintf(out, "alert_emails: %s\n", alerts)
	_, _ = fmt.Fprintf(out, "alert_recipients: %s\n", recipients(preferences.AlertEmailRecipients))
	_, _ = fmt.Fprintf(out, "billing_recipients: %s\n", recipients(preferences.BillingEmailRecipients))
	if preferences.UpdatedAt != nil {
		_, _ = fmt.Fprintf(out, "updated_at: %s\n", formatTime(*preferences.UpdatedAt))
	} else {
		_, _ = fmt.Fprintln(out, "updated_at: - (defaults)")
	}
}
