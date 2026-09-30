package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
	projectdetect "github.com/capydatabase/capydb-cli/internal/project"
)

type integrationEnvPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type vercelEnvPayload struct {
	Comment   string   `json:"comment,omitempty"`
	GitBranch string   `json:"gitBranch,omitempty"`
	Key       string   `json:"key"`
	Target    []string `json:"target"`
	Type      string   `json:"type"`
	Value     string   `json:"value"`
}

type netlifyEnvValue struct {
	Context          string `json:"context"`
	ContextParameter string `json:"context_parameter,omitempty"`
	Value            string `json:"value"`
}

type netlifyEnvPayload struct {
	IsSecret bool              `json:"is_secret"`
	Key      string            `json:"key"`
	Scopes   []string          `json:"scopes"`
	Values   []netlifyEnvValue `json:"values"`
}

// wranglerHyperdriveBinding is one entry of wrangler.jsonc's `hyperdrive` array.
type wranglerHyperdriveBinding struct {
	Binding string `json:"binding"`
	ID      string `json:"id"`
}

// wranglerPayload is a paste-ready wrangler.jsonc fragment. Only the Hyperdrive
// binding travels here: connection strings are secrets and belong in `.dev.vars`
// or `wrangler secret`, never in a file that gets committed.
type wranglerPayload struct {
	CompatibilityFlags []string                    `json:"compatibility_flags"`
	Hyperdrive         []wranglerHyperdriveBinding `json:"hyperdrive"`
}

func (a *app) newIntegrationsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "integrations",
		Short: "Print integration payloads and re-push env vars to connected platforms",
	}

	var branch string
	var netlifyContext string
	var projectRef string
	var target string
	envCommand := &cobra.Command{
		Use:   "env",
		Short: "Print environment variable payloads for deployment platforms",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			resolvedProject, plan, err := a.resolveIntegrationEnvPlan(ctx, client, projectRef)
			if err != nil {
				return err
			}

			switch strings.ToLower(strings.TrimSpace(target)) {
			case "", "dotenv":
				writeDotenvPayload(cmd, plan.Vars)
			case "json":
				writeJSONPayload(cmd, buildIntegrationEnvPayload(plan.Vars))
			case "vercel":
				writeJSONPayload(cmd, buildVercelEnvPayload(plan.Vars, branch))
			case "netlify":
				writeJSONPayload(cmd, buildNetlifyEnvPayload(plan.Vars, netlifyContext))
			case "wrangler", "cloudflare":
				payload, hint, err := a.buildWranglerPayload(ctx, client, resolvedProject.ID, plan.Vars)
				if err != nil {
					return err
				}
				writeJSONPayload(cmd, payload)
				_, _ = fmt.Fprint(cmd.ErrOrStderr(), hint)
			default:
				return fmt.Errorf("--target must be dotenv, json, vercel, netlify, or wrangler")
			}

			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "project: %s (%s)\n", resolvedProject.Name, resolvedProject.ID)
			return nil
		},
	}

	envCommand.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	envCommand.Flags().StringVar(&target, "target", "dotenv", "Output target: dotenv, json, vercel, netlify, or wrangler")
	envCommand.Flags().StringVar(&branch, "branch", "", "Vercel preview branch for branch-scoped env vars")
	envCommand.Flags().StringVar(&netlifyContext, "netlify-context", "all", "Netlify context: all, dev, branch-deploy, deploy-preview, production, or branch")

	command.AddCommand(envCommand)
	command.AddCommand(a.newIntegrationsSyncCommand())
	return command
}

// integrationSyncProviders are the platforms the control plane pushes env
// vars to.
var integrationSyncProviders = []string{"vercel", "netlify", "cloudflare"}

func (a *app) newIntegrationsSyncCommand() *cobra.Command {
	var projectRef string
	var wait bool
	var waitTimeout time.Duration
	command := &cobra.Command{
		Use:   "sync <vercel|netlify|cloudflare>",
		Short: "Re-push the connection env vars to a connected platform now",
		Long: `Pushes the project's current connection env vars (the same set connecting pushed) to a connected
Vercel project, Netlify site or Cloudflare integration again, with the token CapyDB stored when
it was connected - nothing needs re-entering. Use it after a variable was edited or deleted on the
platform, or after a failed push. Pushes also happen on their own after credential rotations and
restores.

This needs the integration to be connected already: connect with ` + "`capydb env sync vercel|netlify`" + `
or ` + "`capydb cloudflare create-database`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			provider := strings.ToLower(strings.TrimSpace(args[0]))
			if !slices.Contains(integrationSyncProviders, provider) {
				return usageErrorf("provider must be one of %s", strings.Join(integrationSyncProviders, ", "))
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			job, err := client.SyncProjectIntegrationEnv(ctx, project.ID, provider)
			if err != nil {
				if apiErr, ok := errors.AsType[*api.APIError](err); ok {
					switch apiErr.StatusCode {
					case http.StatusNotFound:
						return notFoundErrorf("project %s has no %s integration to sync (%v); connect it first", project.Name, provider, err)
					case http.StatusConflict:
						return fmt.Errorf("a %s env push for project %s is already queued or running; wait for it (see `capydb jobs list`): %w", provider, project.Name, err)
					}
				}
				return fmt.Errorf("sync %s env: %w", provider, err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued %s env push for project %s\n", provider, project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, provider+" env push")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addWaitFlags(command, &wait, &waitTimeout, "env push")
	return command
}

func (a *app) resolveIntegrationEnvPlan(ctx context.Context, client *api.Client, projectRef string) (api.Project, projectdetect.EnvPlan, error) {
	detection, err := a.integrationDetection()
	if err != nil {
		return api.Project{}, projectdetect.EnvPlan{}, err
	}

	resolvedProject, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return api.Project{}, projectdetect.EnvPlan{}, err
	}

	connections, err := client.GetProjectConnection(ctx, resolvedProject.ID)
	if err != nil {
		return api.Project{}, projectdetect.EnvPlan{}, fmt.Errorf("fetch project connections: %w", err)
	}

	return resolvedProject, projectdetect.BuildEnvPlan(detection, connections.DirectURL, connections.PooledURL), nil
}

func (a *app) integrationDetection() (projectdetect.Detection, error) {
	linkConfig, err := config.LoadProjectConfig(a.cwd)
	if err == nil {
		return projectDetectionFromConfig(linkConfig), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return projectdetect.Detection{}, err
	}

	detection, err := a.detectProject("")
	if err != nil {
		return projectdetect.Detection{}, err
	}
	return detection, nil
}

func buildIntegrationEnvPayload(vars map[string]string) []integrationEnvPayload {
	keys := sortedEnvKeys(vars)
	payload := make([]integrationEnvPayload, 0, len(keys))
	for _, key := range keys {
		payload = append(payload, integrationEnvPayload{Key: key, Value: vars[key]})
	}
	return payload
}

func buildVercelEnvPayload(vars map[string]string, branch string) []vercelEnvPayload {
	keys := sortedEnvKeys(vars)
	targets := []string{"preview", "production"}
	if strings.TrimSpace(branch) != "" {
		targets = []string{"preview"}
	}

	payload := make([]vercelEnvPayload, 0, len(keys))
	for _, key := range keys {
		item := vercelEnvPayload{
			Comment: "CapyDB Postgres connection string",
			Key:     key,
			Target:  targets,
			Type:    "sensitive",
			Value:   vars[key],
		}
		if strings.TrimSpace(branch) != "" {
			item.GitBranch = strings.TrimSpace(branch)
		}
		payload = append(payload, item)
	}
	return payload
}

func buildNetlifyEnvPayload(vars map[string]string, contextName string) []netlifyEnvPayload {
	contextName = strings.TrimSpace(contextName)
	if contextName == "" {
		contextName = "all"
	}

	keys := sortedEnvKeys(vars)
	payload := make([]netlifyEnvPayload, 0, len(keys))
	for _, key := range keys {
		payload = append(payload, netlifyEnvPayload{
			IsSecret: true,
			Key:      key,
			Scopes:   []string{"builds", "functions"},
			Values: []netlifyEnvValue{{
				Context: contextName,
				Value:   vars[key],
			}},
		})
	}
	return payload
}

// cloudflareIntegrationConfig is the slice of the Cloudflare integration's
// config the CLI needs. KEEP IN LOCKSTEP with the keys the backend writes in
// service.ConnectCloudflareIntegration / worker.syncCloudflareEnv.
type cloudflareIntegrationConfig struct {
	Hyperdrive        bool   `json:"hyperdrive"`
	HyperdriveBinding string `json:"hyperdrive_binding"`
	HyperdriveID      string `json:"hyperdrive_id"`
	Target            string `json:"target"`
}

// buildWranglerPayload fetches the project's integrations and renders the
// Cloudflare one as a wrangler.jsonc fragment.
func (a *app) buildWranglerPayload(
	ctx context.Context,
	client *api.Client,
	projectID string,
	vars map[string]string,
) (wranglerPayload, string, error) {
	integrations, err := client.ListProjectIntegrations(ctx, projectID)
	if err != nil {
		return wranglerPayload{}, "", fmt.Errorf("fetch project integrations: %w", err)
	}
	return wranglerPayloadFromIntegrations(integrations, vars)
}

// wranglerPayloadFromIntegrations turns the project's Cloudflare integration
// into a wrangler.jsonc fragment. The Hyperdrive id only exists once the
// connect sync has run, so a project without one gets an actionable error
// rather than a snippet carrying a placeholder id that fails at deploy time.
func wranglerPayloadFromIntegrations(
	integrations []api.ProjectIntegration,
	vars map[string]string,
) (wranglerPayload, string, error) {
	var config cloudflareIntegrationConfig
	found := false
	for _, integration := range integrations {
		if integration.Provider != "cloudflare" || integration.State != "active" {
			continue
		}
		found = true
		if len(integration.Config) > 0 {
			if err := json.Unmarshal(integration.Config, &config); err != nil {
				return wranglerPayload{}, "", fmt.Errorf("decode cloudflare integration config: %w", err)
			}
		}
		break
	}

	switch {
	case !found:
		return wranglerPayload{}, "", fmt.Errorf(
			"no active Cloudflare integration on this project; connect one in the dashboard (Settings -> Integrations -> Cloudflare) first",
		)
	case !config.Hyperdrive:
		return wranglerPayload{}, "", fmt.Errorf(
			"the Cloudflare integration on this project was connected without Hyperdrive; reconnect it with Hyperdrive enabled",
		)
	case config.HyperdriveID == "":
		return wranglerPayload{}, "", fmt.Errorf(
			"the Hyperdrive config is still being created; re-run once the connect job finishes",
		)
	}

	binding := config.HyperdriveBinding
	if binding == "" {
		binding = defaultHyperdriveBinding
	}
	payload := wranglerPayload{
		// Postgres drivers need Node.js APIs in both Workers and Pages Functions.
		CompatibilityFlags: []string{"nodejs_compat"},
		Hyperdrive:         []wranglerHyperdriveBinding{{Binding: binding, ID: config.HyperdriveID}},
	}
	return payload, wranglerLocalDevHint(binding, vars), nil
}

// defaultHyperdriveBinding mirrors integrations.CloudflareHyperdriveBindingName
// in the backend: the variable name CapyDB binds Hyperdrive to.
const defaultHyperdriveBinding = "HYPERDRIVE"

// wranglerLocalDevHint tells the reader how to make `wrangler dev` talk to the
// same database, which needs a direct connection under a binding-specific name.
func wranglerLocalDevHint(binding string, vars map[string]string) string {
	directURL := firstNonEmptyVar(vars, "DATABASE_DIRECT_URL", "DATABASE_URL")
	if directURL == "" {
		return ""
	}
	return fmt.Sprintf(
		"\nMerge the fragment above into wrangler.jsonc. For `wrangler dev`, export:\n"+
			"  CLOUDFLARE_HYPERDRIVE_LOCAL_CONNECTION_STRING_%s=%s\n",
		binding,
		quoteEnvValue(directURL),
	)
}

func firstNonEmptyVar(vars map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(vars[key]); value != "" {
			return value
		}
	}
	return ""
}

func sortedEnvKeys(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for key, value := range vars {
		if strings.TrimSpace(value) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeDotenvPayload(cmd *cobra.Command, vars map[string]string) {
	for _, key := range sortedEnvKeys(vars) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", key, quoteEnvValue(vars[key]))
	}
}

func writeJSONPayload(cmd *cobra.Command, payload any) {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
}

func quoteEnvValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}
