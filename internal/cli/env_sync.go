package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

// `capydb env sync vercel|netlify` pushes the project's connection env vars
// into a deployment platform. It uses the control plane's token-connect
// integration rather than calling Vercel/Netlify from the CLI: the token is
// stored encrypted, so the variables are pushed again on every credential
// rotation (and per branch with --preview-branches) instead of going stale
// after a one-off push. Re-running it re-pushes and replaces the stored token.

type envSyncOptions struct {
	previewBranches bool
	projectRef      string
	token           string
	wait            bool
	waitTimeout     time.Duration
	// vercel
	vercelProject string
	team          string
	// netlify
	site string
}

// withEnvSync adds `env sync` to the env command group.
func (a *app) withEnvSync(envCommand *cobra.Command) *cobra.Command {
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Push the project's connection env vars to Vercel or Netlify and keep them in sync",
		Long: "Connects the project to a Vercel project (`capydb env sync vercel`) or Netlify site (`capydb env sync netlify`) " +
			"and pushes DATABASE_URL and the other connection variables to it. The token is stored encrypted by CapyDB, so the " +
			"variables are pushed again whenever the credentials rotate; --preview-branches also gives each preview deployment " +
			"its own preview database. Run it again to re-push or to replace the token.",
	}
	sync.AddCommand(a.newEnvSyncVercelCommand(), a.newEnvSyncNetlifyCommand())
	envCommand.AddCommand(sync)
	return envCommand
}

func addEnvSyncFlags(command *cobra.Command, options *envSyncOptions, tokenEnv string) {
	command.Flags().StringVar(&options.projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&options.token, "token", "", "API token (defaults to $"+tokenEnv+"); stored encrypted by CapyDB")
	command.Flags().BoolVar(&options.previewBranches, "preview-branches", false, "Also create a preview database per branch deployment")
	command.Flags().BoolVar(&options.wait, "wait", true, "Wait for the first push to finish")
	command.Flags().DurationVar(&options.waitTimeout, "wait-timeout", 5*time.Minute, "Maximum time to wait for the push")
}

func (a *app) newEnvSyncVercelCommand() *cobra.Command {
	var options envSyncOptions
	command := &cobra.Command{
		Use:   "vercel",
		Short: "Push the connection env vars to a Vercel project",
		Long: "Pushes the connection env vars to a Vercel project. The project comes from --vercel-project or the directory's " +
			".vercel/project.json (written by `vercel link`); a team project uses that file's team id or --team.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			token := firstNonEmpty(options.token, os.Getenv("VERCEL_TOKEN"))
			if token == "" {
				return usageErrorf("a Vercel token is required: pass --token or set VERCEL_TOKEN (create one at vercel.com/account/tokens)")
			}
			linked := readVercelLink(a.linkedAppDirs())
			vercelProject := firstNonEmpty(options.vercelProject, linked.ProjectID)
			if vercelProject == "" {
				return usageErrorf("no Vercel project: pass --vercel-project <id|name> or run `vercel link` in this directory")
			}
			team := strings.TrimSpace(options.team)
			if team == "" && strings.HasPrefix(linked.OrgID, "team_") {
				team = linked.OrgID
			}
			return a.runEnvSync(cmd, options, "Vercel project "+vercelProject, func(client *api.Client, projectID string) (api.ProjectIntegration, api.Job, error) {
				return client.ConnectVercel(cmd.Context(), projectID, api.ConnectVercelRequest{
					PreviewBranches: options.previewBranches,
					TeamID:          team,
					Token:           token,
					VercelProjectID: vercelProject,
				})
			})
		},
	}
	addEnvSyncFlags(command, &options, "VERCEL_TOKEN")
	command.Flags().StringVar(&options.vercelProject, "vercel-project", "", "Vercel project id (prj_...) or name (defaults to .vercel/project.json)")
	command.Flags().StringVar(&options.team, "team", "", "Vercel team id for a team project (defaults to .vercel/project.json)")
	return command
}

func (a *app) newEnvSyncNetlifyCommand() *cobra.Command {
	var options envSyncOptions
	command := &cobra.Command{
		Use:   "netlify",
		Short: "Push the connection env vars to a Netlify site",
		Long:  "Pushes the connection env vars to a Netlify site. The site comes from --site or the directory's .netlify/state.json (written by `netlify link`).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			token := firstNonEmpty(options.token, os.Getenv("NETLIFY_AUTH_TOKEN"))
			if token == "" {
				return usageErrorf("a Netlify token is required: pass --token or set NETLIFY_AUTH_TOKEN (a personal access token from app.netlify.com/user/applications)")
			}
			site := firstNonEmpty(options.site, readNetlifySiteID(a.linkedAppDirs()))
			if site == "" {
				return usageErrorf("no Netlify site: pass --site <id> or run `netlify link` in this directory")
			}
			return a.runEnvSync(cmd, options, "Netlify site "+site, func(client *api.Client, projectID string) (api.ProjectIntegration, api.Job, error) {
				return client.ConnectNetlify(cmd.Context(), projectID, api.ConnectNetlifyRequest{
					PreviewBranches: options.previewBranches,
					SiteID:          site,
					Token:           token,
				})
			})
		},
	}
	addEnvSyncFlags(command, &options, "NETLIFY_AUTH_TOKEN")
	command.Flags().StringVar(&options.site, "site", "", "Netlify site id (defaults to .netlify/state.json)")
	return command
}

func (a *app) runEnvSync(cmd *cobra.Command, options envSyncOptions, destination string, connect func(*api.Client, string) (api.ProjectIntegration, api.Job, error)) error {
	ctx := cmd.Context()
	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return err
	}
	project, err := a.resolveProject(ctx, client, options.projectRef)
	if err != nil {
		return err
	}
	integration, job, err := connect(client, project.ID)
	if err != nil {
		return fmt.Errorf("connect %s: %w", destination, err)
	}

	progress := cmd.OutOrStdout()
	if a.jsonOutput() {
		progress = cmd.ErrOrStderr()
	}
	_, _ = fmt.Fprintf(progress, "Connected project %s to %s; pushing env vars (job %s)\n", project.Name, destination, job.ID)
	var config map[string]any
	if len(integration.Config) > 0 && json.Unmarshal(integration.Config, &config) == nil {
		// The connect succeeds without the deploy hook when the token lacks
		// that permission; say so instead of implying preview branches work.
		for _, key := range []string{"webhook_error", "hook_error"} {
			if message, ok := config[key].(string); ok && message != "" {
				_, _ = fmt.Fprintf(progress, "note: %s\n", message)
			}
		}
	}

	if options.wait && job.ID != "" {
		job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, options.waitTimeout)
		if err != nil {
			return err
		}
		if err := ensureCompletedJob(job, "env sync"); err != nil {
			return err
		}
	}
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{"integration": integration, "job": job})
	}
	if options.wait {
		_, _ = fmt.Fprintf(progress, "Env vars pushed to %s. Redeploy for running deployments to pick them up.\n", destination)
	}
	return nil
}

// linkedAppDirs are where platform link files may live: the working
// directory and, when the local link names one, the app directory.
func (a *app) linkedAppDirs() []string {
	dirs := []string{a.cwd}
	if linkConfig, err := config.LoadProjectConfig(a.cwd); err == nil && strings.TrimSpace(linkConfig.AppPath) != "" && linkConfig.AppPath != "." {
		dirs = append([]string{filepath.Join(a.cwd, linkConfig.AppPath)}, dirs...)
	}
	return dirs
}

type vercelLink struct {
	ProjectID string `json:"projectId"`
	OrgID     string `json:"orgId"`
}

// readVercelLink reads the first .vercel/project.json found; a missing or
// unreadable file yields an empty link (the flags then decide).
func readVercelLink(dirs []string) vercelLink {
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, ".vercel", "project.json"))
		if err != nil {
			continue
		}
		var link vercelLink
		if json.Unmarshal(raw, &link) == nil && link.ProjectID != "" {
			return link
		}
	}
	return vercelLink{}
}

func readNetlifySiteID(dirs []string) string {
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, ".netlify", "state.json"))
		if err != nil {
			continue
		}
		var state struct {
			SiteID string `json:"siteId"`
		}
		if json.Unmarshal(raw, &state) == nil && state.SiteID != "" {
			return state.SiteID
		}
	}
	return ""
}
