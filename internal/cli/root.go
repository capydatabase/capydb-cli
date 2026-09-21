package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/envfile"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
	"github.com/capydatabase/capydb-cli/internal/gitignore"
	"github.com/capydatabase/capydb-cli/internal/project"
	"github.com/capydatabase/capydb-cli/internal/scan"
	"github.com/capydatabase/capydbclient"
)

type app struct {
	apiKey  string
	apiURL  string
	appURL  string
	cwd     string
	output  string
	version string
}

// newAPIClient builds an API client carrying the CLI build version so the
// User-Agent header identifies the binary.
func (a *app) newAPIClient(baseURL, apiKey string) (*api.Client, error) {
	return api.NewClient(baseURL, apiKey, a.version)
}

type resolvedAuth struct {
	APIKey           string
	APIURL           string
	AppURL           string
	OrganizationID   string
	OrganizationName string
	OrganizationSlug string
	Persist          bool
}

func Execute(version string) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return err
	}

	// Cancel the command context on Ctrl-C / SIGTERM so long-running waits
	// (job polling, browser login) can unwind gracefully instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := &app{cwd: workingDirectory}
	return newRootCommand(application, version).ExecuteContext(ctx)
}

func newRootCommand(application *app, version string) *cobra.Command {
	if strings.TrimSpace(version) == "" {
		version = "dev (built from source)"
	}
	application.version = version

	root := &cobra.Command{
		Use:   "capydb",
		Short: "CapyDB Postgres project CLI",
		Long: `CapyDB links local projects to hosted Postgres databases, writes the right env vars, and handles repeatable project workflows. Every project runs in its own isolated database cell - a dedicated Postgres runtime you reach with normal connection strings.

Exit codes:
  0  success
  1  generic error
  2  usage or validation error
  3  authentication or authorization error
  4  resource not found
  5  conflict or failed precondition
  6  timeout`,
		Version: version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return validateOutputMode(application.output)
		},
	}

	root.PersistentFlags().StringVar(&application.apiURL, "api-url", "", "CapyDB API base URL")
	root.PersistentFlags().StringVar(&application.apiKey, "api-key", "", "CapyDB organization API key")
	root.PersistentFlags().StringVar(&application.appURL, "app-url", "", "CapyDB app URL for browser-based login and dashboard links")
	root.PersistentFlags().StringVarP(&application.output, "output", "o", outputText, "Output format: text or json")

	// Flag parse failures are usage errors (exit code 2).
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return exitcode.New(exitcode.UsageError, err)
	})

	root.AddCommand(application.newLoginCommand())
	root.AddCommand(application.newLogoutCommand())
	root.AddCommand(application.newWhoamiCommand())
	root.AddCommand(application.newStatusCommand())
	root.AddCommand(application.newAuthCommand())
	root.AddCommand(application.newCreateCommand())
	root.AddCommand(application.newLinkCommand())
	root.AddCommand(application.newUnlinkCommand())
	root.AddCommand(application.newEnvCommand())
	root.AddCommand(application.newInitCommand())
	root.AddCommand(application.newGenerateCommand())
	root.AddCommand(application.newSchemaCommand())
	root.AddCommand(application.newPreviewCommand())
	root.AddCommand(application.newEphemeralCommand())
	root.AddCommand(application.newBackupsCommand())
	root.AddCommand(application.newExportCommand())
	root.AddCommand(application.newImportCommand())
	root.AddCommand(application.newMigrateCommand())
	root.AddCommand(application.newRestoreCommand())
	root.AddCommand(application.newRestorePointsCommand())
	root.AddCommand(application.newJobsCommand())
	root.AddCommand(application.newStudioCommand())
	root.AddCommand(application.newIntegrationsCommand())
	root.AddCommand(application.newCloudflareCommand())
	root.AddCommand(application.newConnectionStringCommand())
	root.AddCommand(application.newCredentialsCommand())
	root.AddCommand(application.newKVCommand())
	root.AddCommand(application.newPsqlCommand())
	root.AddCommand(application.newSQLCommand())
	root.AddCommand(application.newMetricsCommand())
	root.AddCommand(application.newLogsCommand())
	root.AddCommand(application.newProjectsCommand())
	root.AddCommand(application.newRegionsCommand())
	root.AddCommand(application.newOrgsCommand())
	root.AddCommand(application.newWebhooksCommand())
	root.AddCommand(application.newAPIKeysCommand())
	root.AddCommand(application.newAuditCommand())
	root.AddCommand(application.newExtensionsCommand())
	root.AddCommand(application.newAdvisorCommand())
	root.AddCommand(application.newUpgradeCommand())
	root.AddCommand(application.newAlertsCommand())
	root.AddCommand(application.newDoctorCommand())
	root.AddCommand(application.newConfigCommand())
	root.AddCommand(application.newVersionCommand())
	return root
}

func (a *app) newAuthCommand() *cobra.Command {
	authCommand := &cobra.Command{
		Use:   "auth",
		Short: "Manage CLI authentication",
	}

	authCommand.AddCommand(a.newLoginCommandForAuthGroup())
	authCommand.AddCommand(a.newLogoutCommandForAuthGroup())
	authCommand.AddCommand(a.newWhoamiCommandForAuthGroup())
	return authCommand
}

type loginOptions struct {
	deviceName string
	expiresIn  string
	name       string
	noOpen     bool
}

func (a *app) newLoginCommand() *cobra.Command {
	var options loginOptions

	command := &cobra.Command{
		Use:   "login",
		Short: "Log in to CapyDB from the browser",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runLogin(cmd, options)
		},
	}

	command.Flags().BoolVar(&options.noOpen, "no-open", false, "Print the authorization URL without opening a browser")
	command.Flags().StringVar(&options.name, "name", "", "Human-readable name for the CLI key")
	command.Flags().StringVar(&options.deviceName, "device-name", "", "Device label stored with the CLI key")
	command.Flags().StringVar(&options.expiresIn, "expires-in", "", "Optional key lifetime such as 24h, 7d, 30d, or an RFC3339 timestamp")
	return command
}

func (a *app) newLoginCommandForAuthGroup() *cobra.Command {
	var options loginOptions

	command := &cobra.Command{
		Use:   "login",
		Short: "Log in to CapyDB from the browser",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runLogin(cmd, options)
		},
	}

	command.Flags().BoolVar(&options.noOpen, "no-open", false, "Print the authorization URL without opening a browser")
	command.Flags().StringVar(&options.name, "name", "", "Human-readable name for the CLI key")
	command.Flags().StringVar(&options.deviceName, "device-name", "", "Device label stored with the CLI key")
	command.Flags().StringVar(&options.expiresIn, "expires-in", "", "Optional key lifetime such as 24h, 7d, 30d, or an RFC3339 timestamp")
	return command
}

func (a *app) runLogin(cmd *cobra.Command, options loginOptions) error {
	ctx := cmd.Context()
	apiURL := a.resolveAPIURL("")
	appURL := a.resolveAppURL(apiURL)

	if manualAPIKey := firstNonEmpty(a.apiKey, os.Getenv("CAPYDB_API_KEY")); strings.TrimSpace(manualAPIKey) != "" {
		client, err := a.newAPIClient(apiURL, manualAPIKey)
		if err != nil {
			return err
		}
		viewer, err := client.GetViewer(ctx)
		if err != nil {
			return fmt.Errorf("validate api key: %w", err)
		}

		authConfig := resolvedAuth{
			APIKey:  strings.TrimSpace(manualAPIKey),
			APIURL:  apiURL,
			AppURL:  appURL,
			Persist: true,
		}
		if viewer.Organization != nil {
			authConfig.OrganizationID = viewer.Organization.ID
			authConfig.OrganizationName = viewer.Organization.Name
			authConfig.OrganizationSlug = viewer.Organization.Slug
		}
		return a.saveAuthAndPrint(cmd, authConfig)
	}

	authConfig, err := a.deviceLogin(ctx, cmd.ErrOrStderr(), options)
	if err != nil {
		return err
	}
	return a.saveAuthAndPrint(cmd, authConfig)
}

// deviceLogin runs the browser device-login flow and returns the minted
// credentials. Progress (the approval URL, waiting notices) goes to the given
// writer - callers in --output json mode pass stderr so stdout stays
// machine-readable. The printed "Open this URL to approve:" line is a contract
// with capydb.dev/agents.md; agents grep for it.
func (a *app) deviceLogin(ctx context.Context, progress io.Writer, options loginOptions) (resolvedAuth, error) {
	apiURL := a.resolveAPIURL("")
	appURL := a.resolveAppURL(apiURL)

	anonymousClient, err := a.newAPIClient(apiURL, "")
	if err != nil {
		return resolvedAuth{}, err
	}
	deviceName := resolveCLILoginDeviceName(options.deviceName)
	expiresAt, err := resolveCLILoginExpiry(options.expiresIn)
	if err != nil {
		return resolvedAuth{}, err
	}
	source := cliLoginSource()
	session, err := anonymousClient.StartCLILoginSession(ctx, api.CLILoginSessionStartRequest{
		DeviceName: deviceName,
		ExpiresAt:  expiresAt,
		Name:       resolveCLILoginName(options.name, deviceName, source),
		Source:     source,
	})
	if err != nil {
		return resolvedAuth{}, fmt.Errorf("start login session: %w", err)
	}

	loginURL := strings.TrimRight(appURL, "/") + "/dashboard/cli/login?session=" + url.QueryEscape(session.SessionID)
	_, _ = fmt.Fprintf(progress, "Open this URL to approve:\n%s\n", loginURL)

	if !options.noOpen {
		if err := openURL(loginURL); err != nil {
			_, _ = fmt.Fprintf(progress, "Could not open browser automatically: %v\n", err)
		}
	}

	_, _ = fmt.Fprintln(progress, "Waiting for browser approval (sign-up, plan, and consent happen in that tab)...")
	status, err := waitForCLILoginSession(ctx, anonymousClient, session.SessionID, session.PollToken, session.ExpiresAt)
	if err != nil {
		return resolvedAuth{}, err
	}

	return resolvedAuth{
		APIKey:           status.PlaintextAPIKey,
		APIURL:           apiURL,
		AppURL:           appURL,
		OrganizationID:   status.OrganizationID,
		OrganizationName: status.OrganizationName,
		OrganizationSlug: status.OrganizationSlug,
		Persist:          true,
	}, nil
}

// cliLoginSource detects who is driving the login so the minted key gets the
// right provenance label: AI assistants run the CLI without an interactive
// stdin (or set CAPYDB_AGENT=1 explicitly).
func cliLoginSource() string {
	if os.Getenv("CAPYDB_AGENT") == "1" || !stdinIsInteractive() {
		return "agent"
	}
	return "cli"
}

// resolveAuthOrLogin returns explicit/saved credentials when present and
// otherwise starts the browser device login instead of failing, so first-run
// `capydb create`/`link` need no separate login step. Login progress goes to
// stderr to keep stdout machine-readable.
func (a *app) resolveAuthOrLogin(cmd *cobra.Command, fallbackAPIURL ...string) (resolvedAuth, error) {
	authConfig, err := a.resolveAuth(false, fallbackAPIURL...)
	if err == nil {
		return authConfig, nil
	}

	authConfig, err = a.deviceLogin(cmd.Context(), cmd.ErrOrStderr(), loginOptions{})
	if err != nil {
		return resolvedAuth{}, err
	}
	if err := a.persistResolvedAuth(authConfig); err != nil {
		return resolvedAuth{}, err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged in to organization %s\n", firstNonEmpty(authConfig.OrganizationName, authConfig.OrganizationSlug, authConfig.OrganizationID))
	return authConfig, nil
}

func (a *app) newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Clear saved CLI authentication",
		RunE:  a.runLogout,
	}
}

func (a *app) newLogoutCommandForAuthGroup() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Clear saved CLI authentication",
		RunE:  a.runLogout,
	}
}

func (a *app) runLogout(cmd *cobra.Command, args []string) error {
	path, err := config.UserConfigPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No saved CLI session found.")
			return nil
		}
		return fmt.Errorf("remove saved auth: %w", err)
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Cleared saved CLI session.")
	return nil
}

func (a *app) newWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the current CapyDB CLI identity",
		RunE:  a.runWhoami,
	}
}

func (a *app) newWhoamiCommandForAuthGroup() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the current CapyDB CLI identity",
		RunE:  a.runWhoami,
	}
}

func (a *app) runWhoami(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	client, authConfig, err := a.resolveClient(false)
	if err != nil {
		return err
	}

	viewer, err := client.GetViewer(ctx)
	if err != nil {
		return fmt.Errorf("load viewer: %w", err)
	}

	if a.jsonOutput() {
		report := struct {
			APIURL       string            `json:"api_url"`
			AppURL       string            `json:"app_url"`
			AuthSource   string            `json:"auth_source"`
			Organization *api.Organization `json:"organization"`
		}{
			APIURL:       authConfig.APIURL,
			AppURL:       authConfig.AppURL,
			AuthSource:   firstNonEmpty(viewer.Principal.AuthSource, "api_key"),
			Organization: viewer.Organization,
		}
		return printJSON(cmd.OutOrStdout(), report)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "api_url: %s\n", authConfig.APIURL)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "app_url: %s\n", authConfig.AppURL)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "auth_source: %s\n", firstNonEmpty(viewer.Principal.AuthSource, "api_key"))
	if viewer.Organization != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "organization_id: %s\n", viewer.Organization.ID)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "organization_name: %s\n", viewer.Organization.Name)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "organization_slug: %s\n", viewer.Organization.Slug)
	} else {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "organization_id: -\n")
	}
	return nil
}

func (a *app) newStatusCommand() *cobra.Command {
	var options statusOptions

	command := &cobra.Command{
		Use:   "status",
		Short: "Show saved auth, local project link, and optional remote project status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runStatus(cmd, options)
		},
	}

	command.Flags().StringVar(&options.projectRef, "project", "", "Project id, slug, or name for remote status")
	command.Flags().BoolVar(&options.remote, "remote", false, "Check the CapyDB API and linked project")
	return command
}

func (a *app) newCreateCommand() *cobra.Command {
	var envFileOverride string
	var nonInteractive bool
	var overwriteEnv bool
	var projectName string
	var region string
	var slug string
	var environment string
	var postgresVersion string
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:     "create",
		Aliases: []string{"init"},
		Short:   "Create a CapyDB project and link the current directory to it",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// In JSON mode stdout carries exactly one JSON document (the
			// summary or a structured error); progress narration moves to
			// stderr so agents can pipe stdout straight into a parser.
			progress := cmd.OutOrStdout()
			if a.jsonOutput() {
				progress = cmd.ErrOrStderr()
			}

			err := func() error {
				detection, err := a.detectProject(envFileOverride)
				if err != nil {
					return err
				}

				authConfig, err := a.resolveAuthOrLogin(cmd)
				if err != nil {
					return err
				}

				client, err := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
				if err != nil {
					return err
				}
				viewer, err := client.GetViewer(ctx)
				if err != nil {
					return fmt.Errorf("load organization billing: %w", err)
				}
				if err := ensureViewerCanProvision(viewer, authConfig.AppURL); err != nil {
					return err
				}
				regions, err := client.ListRegions(ctx)
				if err != nil {
					return fmt.Errorf("list regions: %w", err)
				}
				if err := a.persistResolvedAuth(authConfig); err != nil {
					return err
				}

				selectedRegion, err := selectRegion(regions, region, nonInteractive)
				if err != nil {
					return err
				}

				request := api.CreateProjectRequest{
					Environment:     strings.TrimSpace(environment),
					Name:            firstNonEmpty(projectName, detection.ProjectName),
					PostgresVersion: strings.TrimSpace(postgresVersion),
					Region:          selectedRegion,
					Slug:            strings.TrimSpace(slug),
				}

				if selectedRegion != "" {
					_, _ = fmt.Fprintf(progress, "Creating project %s in region %s\n", request.Name, selectedRegion)
				} else {
					_, _ = fmt.Fprintf(progress, "Creating project %s (region auto-selected)\n", request.Name)
				}
				createdProject, job, err := client.CreateProject(ctx, request)
				if err != nil {
					return fmt.Errorf("create project: %w", err)
				}

				if job.ID != "" {
					_, _ = fmt.Fprintf(progress, "Waiting for provision job %s\n", job.ID)
					if job, err = waitForJob(ctx, cmd.ErrOrStderr(), client, job.ID, waitTimeout); err != nil {
						return err
					}
					if job.State != "completed" {
						return fmt.Errorf("project provisioning ended in %s: %s", job.State, job.Error)
					}
				}

				linkConfig := config.ProjectConfig{
					AppPath:       detection.AppPath,
					APIURL:        authConfig.APIURL,
					Region:        selectedRegion,
					DatabaseLayer: detection.DatabaseLayer,
					EnvFile:       detection.EnvFile,
					Framework:     detection.Framework,
					Profile:       detection.Profile,
					ProjectID:     createdProject.ID,
					ProjectName:   createdProject.Name,
					ProjectSlug:   createdProject.Slug,
				}

				if err := a.writeProjectEnv(cmd, client, createdProject.ID, linkConfig, envFileOverride, true, overwriteEnv); err != nil {
					return err
				}

				if a.jsonOutput() {
					return a.printCreateJSONSummary(cmd, detection, createdProject, job)
				}
				printLinkSummary(cmd, detection, linkConfig)
				printCreateNotes(cmd, createdProject)
				return nil
			}()
			if err != nil && a.jsonOutput() {
				// Best effort: the structured error is for agents; the real
				// error still propagates for the exit code and stderr.
				_ = printJSON(cmd.OutOrStdout(), createErrorPayload(err))
			}
			return err
		},
	}

	command.Flags().StringVar(&envFileOverride, "env-file", "", "Target env file")
	command.Flags().BoolVar(&overwriteEnv, "overwrite-env", false, "Overwrite existing env values (e.g. a previous provider's DATABASE_URL) without prompting")
	command.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Let the server pick a region when none is specified")
	// Deprecated spelling kept as a hidden alias: --yes read as a destructive
	// confirm, which `create` is not - it only opts out of the region prompt.
	command.Flags().BoolVar(&nonInteractive, "yes", false, "Let the server pick a region when none is specified")
	_ = command.Flags().MarkHidden("yes")
	command.Flags().StringVar(&projectName, "name", "", "Project name")
	command.Flags().StringVar(&region, "region", "", "Region for project placement (server picks one when omitted)")
	command.Flags().StringVar(&slug, "slug", "", "Project slug override")
	command.Flags().StringVar(&environment, "environment", "", "Environment label: production (default) or non_production (unlocks overwrite-restore)")
	command.Flags().StringVar(&postgresVersion, "postgres-version", "", "Postgres major version: 16, 17, or 18 (server default when omitted)")
	command.Flags().DurationVar(&waitTimeout, "wait-timeout", defaultWaitTimeout, "Maximum time to wait for the provision job")
	return command
}

func (a *app) newLinkCommand() *cobra.Command {
	var envFileOverride string
	var overwriteEnv bool
	var projectRef string

	command := &cobra.Command{
		Use:     "link",
		Aliases: []string{"connect"},
		Short:   "Link the current directory to an existing CapyDB project",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			detection, err := a.detectProject(envFileOverride)
			if err != nil {
				return err
			}

			authConfig, err := a.resolveAuthOrLogin(cmd)
			if err != nil {
				return err
			}

			client, err := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
			if err != nil {
				return err
			}
			resolvedProject, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}

			linkConfig := config.ProjectConfig{
				AppPath:       detection.AppPath,
				APIURL:        authConfig.APIURL,
				Region:        resolvedProject.Region,
				DatabaseLayer: detection.DatabaseLayer,
				EnvFile:       detection.EnvFile,
				Framework:     detection.Framework,
				Profile:       detection.Profile,
				ProjectID:     resolvedProject.ID,
				ProjectName:   resolvedProject.Name,
				ProjectSlug:   resolvedProject.Slug,
			}

			if err := a.writeProjectEnv(cmd, client, resolvedProject.ID, linkConfig, envFileOverride, true, overwriteEnv); err != nil {
				return err
			}
			if err := a.persistResolvedAuth(authConfig); err != nil {
				return err
			}

			printLinkSummary(cmd, detection, linkConfig)
			return nil
		},
	}

	command.Flags().StringVar(&envFileOverride, "env-file", "", "Target env file")
	command.Flags().BoolVar(&overwriteEnv, "overwrite-env", false, "Overwrite existing env values (e.g. a previous provider's DATABASE_URL) without prompting")
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func (a *app) newUnlinkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink",
		Short: "Remove the local CapyDB project link",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.ProjectConfigPath(a.cwd)
			if err := os.Remove(path); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No local project link found.")
					return nil
				}
				return fmt.Errorf("remove local project link: %w", err)
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Removed local project link.")
			return nil
		},
	}
}

func (a *app) newEnvCommand() *cobra.Command {
	var envFileOverride string

	envCommand := &cobra.Command{
		Use:   "env",
		Short: "Manage local env files for the linked CapyDB project",
	}

	pullCommand := &cobra.Command{
		Use:   "pull",
		Short: "Refresh local env vars from the linked CapyDB project",
		RunE: func(cmd *cobra.Command, args []string) error {
			linkConfig, err := config.LoadProjectConfig(a.cwd)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("no local project link found; run `capydb link` or `capydb create` first")
				}
				return err
			}

			authConfig, err := a.resolveAuthOrLogin(cmd, linkConfig.APIURL)
			if err != nil {
				return err
			}

			client, err := a.newAPIClient(authConfig.APIURL, authConfig.APIKey)
			if err != nil {
				return err
			}
			if err := a.writeProjectEnv(cmd, client, linkConfig.ProjectID, linkConfig, envFileOverride, false, false); err != nil {
				return err
			}
			if err := a.persistResolvedAuth(authConfig); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated %s from project %s\n", firstNonEmpty(envFileOverride, linkConfig.EnvFile), linkConfig.ProjectID)
			for _, step := range project.BuildNextSteps(projectDetectionFromConfig(linkConfig)) {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", step)
			}
			return nil
		},
	}

	pullCommand.Flags().StringVar(&envFileOverride, "env-file", "", "Override the target env file")
	envCommand.AddCommand(pullCommand)
	return envCommand
}

func (a *app) detectProject(envFileOverride string) (project.Detection, error) {
	detection, err := project.Detect(a.cwd, envFileOverride)
	if err == nil {
		return detection, nil
	}

	var ambiguous *project.AmbiguousProjectError
	if !errors.As(err, &ambiguous) {
		return project.Detection{}, err
	}
	if !stdinIsInteractive() {
		return project.Detection{}, err
	}

	return selectAppCandidate(ambiguous.Candidates)
}

func (a *app) resolveAuth(prompt bool, fallbackAPIURL ...string) (resolvedAuth, error) {
	apiURL := a.resolveAPIURL(firstNonEmpty(fallbackAPIURL...))
	appURL := a.resolveAppURL(apiURL)

	if trimmed := strings.TrimSpace(a.apiKey); trimmed != "" {
		return resolvedAuth{APIKey: trimmed, APIURL: apiURL, AppURL: appURL}, nil
	}
	if trimmed := strings.TrimSpace(os.Getenv("CAPYDB_API_KEY")); trimmed != "" {
		return resolvedAuth{APIKey: trimmed, APIURL: apiURL, AppURL: appURL}, nil
	}

	userConfig, err := config.LoadUserConfig()
	if err == nil {
		if orgID, entry, ok := userConfig.Active(); ok && strings.TrimSpace(entry.APIKey) != "" {
			organizationID := orgID
			if organizationID == config.DefaultOrgKey {
				organizationID = ""
			}
			return resolvedAuth{
				APIKey:           strings.TrimSpace(entry.APIKey),
				APIURL:           a.resolveAPIURL(firstNonEmpty(apiURL, entry.APIURL)),
				AppURL:           a.resolveAppURL(firstNonEmpty(appURL, entry.AppURL)),
				OrganizationID:   organizationID,
				OrganizationName: entry.Name,
				OrganizationSlug: entry.Slug,
			}, nil
		}
	}

	if !prompt || !stdinIsInteractive() {
		return resolvedAuth{}, authErrorf("no api key available; pass --api-key, set CAPYDB_API_KEY, or run `capydb login`")
	}

	value, err := promptLine("CapyDB API key")
	if err != nil {
		return resolvedAuth{}, err
	}
	if strings.TrimSpace(value) == "" {
		return resolvedAuth{}, fmt.Errorf("api key cannot be empty")
	}
	return resolvedAuth{
		APIKey:  strings.TrimSpace(value),
		APIURL:  apiURL,
		AppURL:  appURL,
		Persist: true,
	}, nil
}

func (a *app) resolveAPIURL(fallback string) string {
	if trimmed := strings.TrimSpace(a.apiURL); trimmed != "" {
		return strings.TrimRight(trimmed, "/")
	}
	if trimmed := strings.TrimSpace(os.Getenv("CAPYDB_API_URL")); trimmed != "" {
		return strings.TrimRight(trimmed, "/")
	}
	if trimmed := strings.TrimSpace(fallback); trimmed != "" {
		return strings.TrimRight(trimmed, "/")
	}

	userConfig, err := config.LoadUserConfig()
	if err == nil {
		if _, entry, ok := userConfig.Active(); ok && strings.TrimSpace(entry.APIURL) != "" {
			return strings.TrimRight(strings.TrimSpace(entry.APIURL), "/")
		}
	}
	return config.DefaultAPIURL()
}

// persistResolvedAuth upserts the credentials into the per-organization user
// config and makes that organization active.
func (a *app) persistResolvedAuth(authConfig resolvedAuth) error {
	if !authConfig.Persist {
		return nil
	}

	userConfig, err := config.LoadUserConfig()
	if err != nil {
		return err
	}
	userConfig.Upsert(authConfig.OrganizationID, config.OrganizationConfig{
		APIKey: authConfig.APIKey,
		APIURL: authConfig.APIURL,
		AppURL: authConfig.AppURL,
		Name:   authConfig.OrganizationName,
		Slug:   authConfig.OrganizationSlug,
	})
	return config.SaveUserConfig(userConfig)
}

func (a *app) saveAuthAndPrint(cmd *cobra.Command, authConfig resolvedAuth) error {
	if err := a.persistResolvedAuth(authConfig); err != nil {
		return err
	}

	path, err := config.UserConfigPath()
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Saved CLI auth to %s\n", path)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "API URL: %s\n", authConfig.APIURL)
	if strings.TrimSpace(authConfig.OrganizationName) != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Organization: %s (%s)\n", authConfig.OrganizationName, firstNonEmpty(authConfig.OrganizationSlug, authConfig.OrganizationID))
	}
	return nil
}

func (a *app) writeProjectEnv(cmd *cobra.Command, client *api.Client, projectID string, linkConfig config.ProjectConfig, envFileOverride string, confirmOverwrite, forceOverwrite bool) error {
	ctx := cmd.Context()
	projectDetails, _, err := client.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("fetch project: %w", err)
	}
	connections, err := client.GetProjectConnection(ctx, projectID)
	if err != nil {
		return fmt.Errorf("fetch project connections: %w", err)
	}

	envPath := linkConfig.EnvFile
	if trimmed := strings.TrimSpace(envFileOverride); trimmed != "" {
		envPath = trimmed
	}

	detection := projectDetectionFromConfig(linkConfig)
	plan := project.BuildEnvPlan(detection, connections.DirectURL, connections.PooledURL)
	envAbsPath := envTargetPath(a.cwd, linkConfig.AppPath, envPath)

	// Refresh the K/V endpoint when the project has a store. Only the REST URL:
	// the token, and the RESP URL that carries it as its password, are not
	// recoverable (the control plane keeps the hash), so both are written once
	// by `capydb kv create --write-env` and left alone here. A
	// project without a store contributes nothing and says nothing - a line of
	// output on every pull would be noise.
	if store, err := client.GetKVStore(ctx, projectID); err == nil {
		if restURL := strings.TrimSpace(store.Credentials.RestURL); restURL != "" {
			plan.Vars[kvRestURLVar] = restURL
		}
	} else if !capydbclient.IsNotFound(err) {
		return fmt.Errorf("fetch kv store: %w", err)
	}

	// forceOverwrite (--overwrite-env) skips the interactive conflict prompt:
	// a nil resolver overwrites silently, which is what migration/automation
	// flows want when repointing DATABASE_URL from another provider.
	var resolver envfile.ConflictResolver
	if confirmOverwrite && !forceOverwrite {
		resolver = a.envOverwriteResolver(cmd)
	}
	if err := envfile.UpsertWithResolver(envAbsPath, plan.Vars, resolver); err != nil {
		return err
	}

	linkConfig.Region = firstNonEmpty(linkConfig.Region, projectDetails.Region)
	linkConfig.DatabaseURLVar = plan.DatabaseURLVar
	linkConfig.DirectURLVar = plan.DirectURLVar
	linkConfig.PooledURLVar = plan.PooledURLVar
	linkConfig.EnvFile = envPath
	linkConfig.OrganizationID = projectDetails.OrganizationID
	linkConfig.ProjectID = projectDetails.ID
	linkConfig.ProjectName = projectDetails.Name
	linkConfig.ProjectSlug = projectDetails.Slug

	if err := config.SaveProjectConfig(a.cwd, linkConfig); err != nil {
		return err
	}

	// Ensure both the local link directory and the credential-bearing env file
	// are git-ignored. The env file holds the full postgres:// URL with the
	// password, so it must never be committed.
	envIgnore := envIgnoreEntry(linkConfig.AppPath, envPath)
	if err := gitignore.EnsureLocalConfigIgnored(a.cwd, envIgnore); err != nil {
		return err
	}

	warnEnvShadowing(cmd, a.cwd, envPath)
	return nil
}

// warnEnvShadowing reports env keys that now point at two different databases.
//
// The env file this command just wrote is only one of several a repo carries,
// and the conflict resolver above only sees that one file. A leftover
// DATABASE_URL in a sibling file does not surface as an error anywhere: the
// framework picks one file by precedence while tools that pin a path
// (drizzle.config.ts, seed scripts) pick another, so the app can run on CapyDB
// while migrations still write to the old provider. Surfacing it at write time
// is the only moment the user is already thinking about env files.
func warnEnvShadowing(cmd *cobra.Command, root, writtenEnvPath string) {
	conflicts, err := scan.DetectEnvConflicts(root)
	if err != nil || len(conflicts) == 0 {
		return
	}
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "\nwarning: %d env key(s) point at different databases in different files:\n", len(conflicts))
	for _, conflict := range conflicts {
		_, _ = fmt.Fprintf(errOut, "  - %s\n", conflict.Describe())
	}
	_, _ = fmt.Fprintf(errOut, "CapyDB wrote %s. Remove the database vars from the other file(s) so one file owns them,\n", writtenEnvPath)
	_, _ = fmt.Fprintln(errOut, "and check anything calling dotenv/load_dotenv with an explicit path: `capydb migrate scan` lists those.")
}

// envIgnoreEntry resolves the .gitignore entry for the env file the CLI writes,
// relative to the repo root (cwd). Absolute or directory-qualified env paths are
// passed through; bare filenames are joined with the detected app path.
func envIgnoreEntry(appPath, envPath string) string {
	envPath = strings.TrimSpace(envPath)
	if envPath == "" {
		return ""
	}
	if filepath.IsAbs(envPath) || strings.Contains(envPath, string(filepath.Separator)) {
		return envPath
	}
	if trimmed := strings.TrimSpace(appPath); trimmed != "" {
		return filepath.Join(trimmed, envPath)
	}
	return envPath
}

// envOverwriteResolver returns a ConflictResolver that warns about an existing,
// differing env value and asks for confirmation on an interactive terminal. In
// non-interactive (CI) contexts it prints a clear warning and proceeds with the
// overwrite so automated link/create flows keep working.
func (a *app) envOverwriteResolver(cmd *cobra.Command) envfile.ConflictResolver {
	return func(key, existing, incoming string) (bool, error) {
		if !stdinIsInteractive() {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: overwriting existing %s in env file\n", key)
			return true, nil
		}

		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s already has a value in the env file that differs from the CapyDB value.\n", key)
		answer, err := promptLine(fmt.Sprintf("Overwrite %s? [y/N]", key))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, nil
		default:
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Keeping existing %s.\n", key)
			return false, nil
		}
	}
}

func resolveCLILoginDeviceName(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	hostname, err := os.Hostname()
	if err == nil && strings.TrimSpace(hostname) != "" {
		return strings.TrimSpace(hostname)
	}
	return "local-" + runtime.GOOS
}

func resolveCLILoginName(value, deviceName, source string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	prefix := "CLI"
	if source == "agent" {
		prefix = "Agent"
	}
	if strings.TrimSpace(deviceName) == "" {
		return "CapyDB " + prefix
	}
	return prefix + " on " + strings.TrimSpace(deviceName)
}

func resolveCLILoginExpiry(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		value := parsed.UTC()
		if !value.After(time.Now().UTC()) {
			return nil, fmt.Errorf("invalid --expires-in value %q; expiration must be in the future", trimmed)
		}
		return &value, nil
	}

	duration, err := parseCLILoginDuration(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid --expires-in value %q; use 24h, 7d, 30d, or an RFC3339 timestamp", trimmed)
	}
	value := time.Now().UTC().Add(duration)
	return &value, nil
}

func parseCLILoginDuration(raw string) (time.Duration, error) {
	if duration, err := time.ParseDuration(raw); err == nil {
		if duration <= 0 {
			return 0, fmt.Errorf("duration must be positive")
		}
		return duration, nil
	}

	if len(raw) < 2 {
		return 0, fmt.Errorf("unsupported duration")
	}

	unit := raw[len(raw)-1]
	amount, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}

	switch unit {
	case 'd', 'D':
		return time.Duration(amount) * 24 * time.Hour, nil
	case 'w', 'W':
		return time.Duration(amount) * 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported duration unit")
	}
}

// waitForCLILoginSession polls the login session until it is authorized.
// 404/410 (session unknown or gone) and 401 (poll token rejected) fail fast -
// the session will never recover, so spinning until expiry only hides the
// problem. Transient network errors are tolerated up to a small cap.
func waitForCLILoginSession(ctx context.Context, client *api.Client, sessionID, pollToken string, expiresAt time.Time) (api.CLILoginSessionStatus, error) {
	const maxConsecutiveNetworkFailures = 5

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	consecutiveNetworkFailures := 0
	for {
		status, err := client.PollCLILoginSession(ctx, sessionID, pollToken)
		if err == nil {
			consecutiveNetworkFailures = 0
			if status.State == "completed" && strings.TrimSpace(status.PlaintextAPIKey) != "" {
				return status, nil
			}
		} else {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				switch apiErr.StatusCode {
				case 404, 410:
					return api.CLILoginSessionStatus{}, fmt.Errorf("login session expired or not found; run `capydb login` again")
				case 401:
					return api.CLILoginSessionStatus{}, fmt.Errorf("login session poll was rejected (unauthorized); run `capydb login` again")
				default:
					return api.CLILoginSessionStatus{}, fmt.Errorf("poll login session: %w", err)
				}
			}
			consecutiveNetworkFailures++
			if consecutiveNetworkFailures >= maxConsecutiveNetworkFailures {
				return api.CLILoginSessionStatus{}, fmt.Errorf("poll login session: giving up after %d consecutive failures: %w", consecutiveNetworkFailures, err)
			}
		}

		if !expiresAt.IsZero() && time.Now().UTC().After(expiresAt) {
			return api.CLILoginSessionStatus{}, fmt.Errorf("login session expired; run `capydb login` again")
		}

		select {
		case <-ctx.Done():
			return api.CLILoginSessionStatus{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func envTargetPath(cwd, appPath, envPath string) string {
	envPath = strings.TrimSpace(envPath)
	if filepath.IsAbs(envPath) {
		return envPath
	}
	if strings.Contains(envPath, string(filepath.Separator)) {
		return filepath.Join(cwd, envPath)
	}
	base := cwd
	if trimmed := strings.TrimSpace(appPath); trimmed != "" {
		base = filepath.Join(cwd, trimmed)
	}
	return filepath.Join(base, envPath)
}

func projectDetectionFromConfig(cfg config.ProjectConfig) project.Detection {
	return project.Detection{
		AppPath:       cfg.AppPath,
		DatabaseLayer: cfg.DatabaseLayer,
		EnvFile:       cfg.EnvFile,
		Framework:     cfg.Framework,
		Profile:       cfg.Profile,
		ProjectName:   cfg.ProjectName,
	}
}

// printCreateJSONSummary emits the single machine-readable stdout document
// for `capydb create --output json`: identifiers, the env file written, and
// the env var NAMES - values stay in the file and must not reach transcripts.
func (a *app) printCreateJSONSummary(cmd *cobra.Command, detection project.Detection, createdProject api.Project, job api.Job) error {
	linkConfig, err := config.LoadProjectConfig(a.cwd)
	if err != nil {
		return fmt.Errorf("reload project link for summary: %w", err)
	}

	envVars := make([]string, 0, 3)
	for _, name := range []string{linkConfig.DatabaseURLVar, linkConfig.DirectURLVar, linkConfig.PooledURLVar} {
		if strings.TrimSpace(name) != "" {
			envVars = append(envVars, name)
		}
	}

	return printJSON(cmd.OutOrStdout(), struct {
		Region      string   `json:"region,omitempty"`
		EnvFile     string   `json:"env_file"`
		EnvVars     []string `json:"env_vars"`
		Framework   string   `json:"framework,omitempty"`
		JobID       string   `json:"job_id,omitempty"`
		JobState    string   `json:"job_state,omitempty"`
		ProjectID   string   `json:"project_id"`
		ProjectName string   `json:"project_name"`
		ProjectSlug string   `json:"project_slug,omitempty"`
	}{
		Region:      linkConfig.Region,
		EnvFile:     firstNonEmpty(filepath.Join(detection.AppPath, linkConfig.EnvFile), linkConfig.EnvFile),
		EnvVars:     jsonList(envVars),
		Framework:   firstNonEmpty(detection.Framework, detection.Profile),
		JobID:       job.ID,
		JobState:    job.State,
		ProjectID:   createdProject.ID,
		ProjectName: createdProject.Name,
		ProjectSlug: createdProject.Slug,
	})
}

// createErrorPayload maps a create failure onto the structured error contract
// documented at capydb.dev/agents.md: a stable `error` code plus, when there
// is a concrete next step, an `action`/`url` pair the agent can relay.
func createErrorPayload(err error) map[string]string {
	payload := map[string]string{
		"error":   "create_failed",
		"message": err.Error(),
	}

	if billingErr, ok := errors.AsType[*billingInactiveError](err); ok {
		payload["error"] = "billing_inactive"
		payload["action"] = "open_url"
		payload["url"] = billingErr.url
		return payload
	}

	var coded *exitcode.Error
	if errors.As(err, &coded) && coded.Code == exitcode.AuthError {
		payload["error"] = "auth_required"
		payload["action"] = "run_command"
		payload["command"] = "capydb login"
	}
	return payload
}

func printLinkSummary(cmd *cobra.Command, detection project.Detection, linkConfig config.ProjectConfig) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Linked %s to project %s\n", firstNonEmpty(detection.ProjectName, filepath.Base(linkConfig.AppPath), filepath.Base(linkConfig.ProjectName)), linkConfig.ProjectID)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "App path: %s\n", firstNonEmpty(detection.AppPath, "."))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Env file updated: %s\n", firstNonEmpty(filepath.Join(detection.AppPath, detection.EnvFile), detection.EnvFile))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Profile: %s\n", firstNonEmpty(detection.Profile, detection.Framework))
	for _, step := range project.BuildNextSteps(detection) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", step)
	}
}

// printCreateNotes states the two things about a fresh cell that are otherwise
// discovered later, and expensively: whether it sleeps, and that the index
// advisor needs an extension whose enable restarts the database. Both are
// cheap decisions on an empty cell and awkward ones once it carries traffic.
func printCreateNotes(cmd *cobra.Command, createdProject api.Project) {
	out := cmd.OutOrStdout()
	if createdProject.AlwaysOn {
		_, _ = fmt.Fprintln(out, "- Stays awake: this project will not pause when idle (the default for production).")
	} else {
		_, _ = fmt.Fprintln(out, "- Pauses when idle and resumes on the next connection, which costs the first")
		_, _ = fmt.Fprintln(out, "  caller a short wake. Turn it off with `capydb projects always-on on`.")
	}
	_, _ = fmt.Fprintln(out, "- `capydb advisor indexes` needs the pg_qualstats extension, and enabling it")
	_, _ = fmt.Fprintln(out, "  RESTARTS the database. Do it now if you want it - on a cell carrying traffic")
	_, _ = fmt.Fprintln(out, "  that becomes a maintenance window.")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// billingInactiveError carries the billing page URL so JSON consumers (AI
// agents) get a concrete next step instead of just prose.
type billingInactiveError struct {
	message string
	url     string
}

func (e *billingInactiveError) Error() string {
	return e.message
}

func ensureViewerCanProvision(viewer api.Viewer, appURL string) error {
	if viewer.Organization == nil {
		return fmt.Errorf("select an organization before creating a project")
	}

	if billingAllowsProvisioning(viewer.Organization.BillingPlan, viewer.Organization.BillingStatus, viewer.Organization.BillingPeriodEnd) {
		return nil
	}

	billingURL := strings.TrimRight(firstNonEmpty(appURL, config.DefaultAppURL("")), "/") + "/dashboard/settings/billing"
	return &billingInactiveError{
		message: fmt.Sprintf(
			"organization billing is %s on plan %s; pick a plan (1 month free) at %s before creating a project",
			viewer.Organization.BillingStatus,
			viewer.Organization.BillingPlan,
			billingURL,
		),
		url: billingURL,
	}
}

func billingAllowsProvisioning(plan, status string, periodEnd *time.Time) bool {
	if plan == "" || plan == "none" {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "trialing":
		return true
	case "past_due", "canceled":
		return periodEnd != nil && periodEnd.After(time.Now().UTC())
	default:
		return false
	}
}

func promptLine(label string) (string, error) {
	fmt.Printf("%s: ", label)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, os.ErrClosed) {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
	}
	return strings.TrimSpace(line), nil
}

func stdinIsInteractive() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CI")), "true") {
		return false
	}

	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// selectRegion resolves the placement region for a new project. An explicit
// ref is validated against the available regions; when no ref is given the
// server is allowed to pick (empty string) unless an interactive choice is
// possible and wanted.
func selectRegion(regions []string, ref string, nonInteractive bool) (string, error) {
	if trimmed := strings.TrimSpace(ref); trimmed != "" {
		if len(regions) == 0 {
			return trimmed, nil
		}
		for _, region := range regions {
			if strings.EqualFold(region, trimmed) {
				return region, nil
			}
		}
		return "", fmt.Errorf("region %q not available", trimmed)
	}

	// No region requested: let the server choose when we cannot or should not
	// prompt for one.
	if nonInteractive || len(regions) == 0 || !stdinIsInteractive() {
		return "", nil
	}
	if len(regions) == 1 {
		return regions[0], nil
	}

	fmt.Println("Select a region:")
	for index, region := range regions {
		fmt.Printf("  %d. %s\n", index+1, region)
	}

	value, err := promptLine("Region number (leave blank to let the server choose)")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	selection, err := strconv.Atoi(value)
	if err != nil || selection < 1 || selection > len(regions) {
		return "", fmt.Errorf("invalid region selection")
	}

	return regions[selection-1], nil
}

func selectAppCandidate(candidates []project.Detection) (project.Detection, error) {
	if len(candidates) == 0 {
		return project.Detection{}, fmt.Errorf("no app candidates found")
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	fmt.Println("Select the local app to link:")
	for index, candidate := range candidates {
		fmt.Printf(
			"  %d. %s [%s]\n",
			index+1,
			firstNonEmpty(candidate.AppPath, "."),
			firstNonEmpty(candidate.Profile, candidate.Framework),
		)
	}

	value, err := promptLine("App number")
	if err != nil {
		return project.Detection{}, err
	}
	selection, err := strconv.Atoi(value)
	if err != nil || selection < 1 || selection > len(candidates) {
		return project.Detection{}, fmt.Errorf("invalid app selection")
	}
	return candidates[selection-1], nil
}

// writerIsTerminal reports whether the writer is an interactive terminal, so
// progress output can use carriage-return rewrites instead of plain lines.
func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// defaultWaitTimeout bounds --wait job polling unless --wait-timeout overrides it.
const defaultWaitTimeout = 30 * time.Minute

// waitForJob polls a job until it reaches a terminal state or the overall
// timeout elapses (a zero/negative timeout falls back to defaultWaitTimeout).
// Polling backs off exponentially from 2s to 10s, tolerates up to 5
// consecutive poll errors (network blips), fails immediately on 401/403
// (auth failures never resolve themselves), and writes a lightweight progress
// indicator to errOut: a single \r-rewritten line on a TTY, plain periodic
// lines otherwise.
func waitForJob(ctx context.Context, errOut io.Writer, client *api.Client, jobID string, timeout time.Duration) (api.Job, error) {
	const (
		initialInterval        = 2 * time.Second
		maxInterval            = 10 * time.Second
		maxConsecutiveFailures = 5
	)

	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)

	interval := initialInterval
	start := time.Now()
	isTTY := writerIsTerminal(errOut)
	attempt := 0
	consecutiveFailures := 0
	lastLineLen := 0
	progressActive := false

	finishProgress := func() {
		if isTTY && progressActive {
			_, _ = fmt.Fprintln(errOut)
			progressActive = false
		}
	}

	for {
		attempt++
		job, err := client.GetJob(ctx, jobID)
		if err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
				finishProgress()
				return api.Job{}, fmt.Errorf("poll job %s: %w", jobID, err)
			}
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				finishProgress()
				return api.Job{}, fmt.Errorf("poll job %s: giving up after %d consecutive failures: %w", jobID, consecutiveFailures, err)
			}
		} else {
			consecutiveFailures = 0
			if jobDone(job) {
				finishProgress()
				return job, nil
			}

			line := fmt.Sprintf(
				"waiting for job %s (%s) - %s, attempt %d, elapsed %s",
				jobID,
				firstNonEmpty(job.Type, "unknown"),
				firstNonEmpty(job.State, "unknown"),
				attempt,
				time.Since(start).Truncate(time.Second),
			)
			if isTTY {
				padding := ""
				if lastLineLen > len(line) {
					padding = strings.Repeat(" ", lastLineLen-len(line))
				}
				_, _ = fmt.Fprintf(errOut, "\r%s%s", line, padding)
				lastLineLen = len(line)
				progressActive = true
			} else {
				_, _ = fmt.Fprintln(errOut, line)
			}
		}

		if time.Now().After(deadline) {
			finishProgress()
			return api.Job{}, exitcode.Errorf(
				exitcode.Timeout,
				"timed out waiting for job %s after %s; check `capydb jobs get --job-id %s` later or raise --wait-timeout",
				jobID,
				timeout,
				jobID,
			)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			finishProgress()
			return api.Job{}, ctx.Err()
		case <-timer.C:
		}
		interval *= 2
		if interval > maxInterval {
			interval = maxInterval
		}
	}
}
