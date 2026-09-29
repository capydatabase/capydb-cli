package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// newCloudflareCommand is CapyDB's half of Cloudflare's Cloudflare-billed
// database flow: Cloudflare mints a short-lived signed authorization
// (createDatabaseSignature -> account_id, timestamp, signature) and hands it
// to the partner's CLI, which creates the database. Cloudflare invoices the
// usage, so there is no CapyDB login involved - the authorization is the
// credential, and the control plane verifies it.
//
// What is settled and what is not: the flags carry exactly the three values
// Cloudflare's API returns plus the create options every other CapyDB create
// path takes, named the way `capydb create` names them. How Cloudflare's
// dashboard invokes a partner CLI (argument names, stdout shape) is decided at
// onboarding - see docs/cloudflare-hyperdrive-partner-submission.md - and
// until the partner secret is configured the control plane refuses every
// request, so the command stays hidden from `capydb --help`.
func (a *app) newCloudflareCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "cloudflare",
		Short:  "Cloudflare-billed database provisioning (Cloudflare partner flow; not yet enabled)",
		Hidden: true,
	}

	var accountID string
	var accountName string
	var name string
	var plan string
	var postgresVersion string
	var region string
	var signature string
	var timestamp int64

	createCommand := &cobra.Command{
		Use:   "create-database",
		Short: "Create a database from a Cloudflare-issued authorization",
		Long: `Creates a CapyDB database from the authorization Cloudflare's
createDatabaseSignature API returns (account id, timestamp, signature). No CapyDB
login or API key is used: the control plane verifies the signature, creates or
reuses the organization tied to that Cloudflare account (billed through
Cloudflare), and queues the database's provision job.

Not yet enabled: the Cloudflare partner integration is waiting on Cloudflare
onboarding, and until then the control plane rejects every request with
"cloudflare partner provisioning is not enabled on this deployment". Flag names
and the output shape may change when Cloudflare settles how its dashboard calls
partner CLIs.

Prints the database, organization, and job; -o json prints them as one JSON
document. The database is ready when the job completes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ordered, so the first missing flag reported is always the same one.
			required := []struct {
				flag  string
				value string
			}{
				{flag: "--account-id", value: accountID},
				{flag: "--signature", value: signature},
				{flag: "--name", value: name},
			}
			for _, field := range required {
				if strings.TrimSpace(field.value) == "" {
					return usageErrorf("%s is required", field.flag)
				}
			}
			if timestamp <= 0 {
				return usageErrorf("--timestamp is required")
			}

			// No API key: this path is authenticated by Cloudflare's signature,
			// and the customer has no CapyDB credentials yet by definition.
			client, err := a.newAPIClient(a.resolveAPIURL(""), "")
			if err != nil {
				return err
			}

			result, err := client.ProvisionCloudflareDatabase(cmd.Context(), api.ProvisionCloudflareDatabaseRequest{
				AccountID:       strings.TrimSpace(accountID),
				AccountName:     strings.TrimSpace(accountName),
				BillingPlan:     strings.TrimSpace(plan),
				Name:            strings.TrimSpace(name),
				PostgresVersion: strings.TrimSpace(postgresVersion),
				Region:          strings.TrimSpace(region),
				Signature:       strings.TrimSpace(signature),
				Timestamp:       timestamp,
			})
			if err != nil {
				return fmt.Errorf("create Cloudflare-billed database: %w", err)
			}

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), result)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Database %s (%s) is provisioning.\n", result.Project.Name, result.Project.ID)
			_, _ = fmt.Fprintf(out, "Organization: %s (%s), billed through Cloudflare\n", result.Organization.Name, result.Organization.ID)
			_, _ = fmt.Fprintf(out, "Provision job: %s\n", result.Job.ID)
			return nil
		},
	}

	createCommand.Flags().StringVar(&accountID, "account-id", "", "Cloudflare account id the authorization was issued for (account_id)")
	createCommand.Flags().StringVar(&signature, "signature", "", "Hex signature from Cloudflare's createDatabaseSignature response (signature)")
	createCommand.Flags().Int64Var(&timestamp, "timestamp", 0, "Unix timestamp from Cloudflare's createDatabaseSignature response (timestamp)")
	createCommand.Flags().StringVar(&name, "name", "", "Database name")
	createCommand.Flags().StringVar(&accountName, "account-name", "", "Name for the CapyDB organization created on this account's first database")
	createCommand.Flags().StringVar(&plan, "plan", "", "Plan Cloudflare invoices: vibe (default), ship, or business")
	createCommand.Flags().StringVar(&region, "region", "", "Region for the database (server picks one when omitted)")
	createCommand.Flags().StringVar(&postgresVersion, "postgres-version", "", "Postgres major version: 16, 17, or 18 (server default when omitted)")

	command.AddCommand(createCommand)
	return command
}
