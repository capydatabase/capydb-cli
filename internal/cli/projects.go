package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func (a *app) newProjectsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "projects",
		Aliases: []string{"project"},
		Short:   "Inspect CapyDB projects",
	}

	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List projects for the active organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			projects, err := client.ListProjects(ctx, "")
			if err != nil {
				return fmt.Errorf("list projects: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"projects": jsonList(projects)})
			}
			if len(projects) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No projects available for this api key")
				return nil
			}

			writeProjectTable(cmd.OutOrStdout(), projects)
			return nil
		},
	}

	var envProjectRef string
	setEnvironmentCommand := &cobra.Command{
		Use:   "set-environment <production|non_production>",
		Short: "Set a project's environment label (non_production unlocks overwrite-restore)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			environment := strings.TrimSpace(args[0])
			if environment != "production" && environment != "non_production" {
				return usageErrorf("environment must be one of production or non_production")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, envProjectRef)
			if err != nil {
				return err
			}
			updated, err := client.UpdateProjectEnvironment(ctx, project.ID, environment)
			if err != nil {
				return fmt.Errorf("set environment: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"project": updated})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s environment set to %s\n", updated.Name, updated.Environment)
			return nil
		},
	}
	setEnvironmentCommand.Flags().StringVar(&envProjectRef, "project", "", "Project id, slug, or name")

	var alwaysOnProjectRef string
	alwaysOnCommand := &cobra.Command{
		Use:   "always-on <on|off>",
		Short: "Keep the database awake instead of pausing it when idle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var alwaysOn bool
			switch strings.ToLower(strings.TrimSpace(args[0])) {
			case "on", "true", "yes":
				alwaysOn = true
			case "off", "false", "no":
				alwaysOn = false
			default:
				return usageErrorf("expected on or off")
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, alwaysOnProjectRef)
			if err != nil {
				return err
			}
			updated, err := client.UpdateProjectAlwaysOn(ctx, project.ID, alwaysOn)
			if err != nil {
				return fmt.Errorf("set always-on: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"project": updated})
			}
			if updated.AlwaysOn {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s stays awake; it will not pause when idle\n", updated.Name)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s pauses when idle and resumes on the next connection\n", updated.Name)
			}
			return nil
		},
	}
	alwaysOnCommand.Flags().StringVar(&alwaysOnProjectRef, "project", "", "Project id, slug, or name")

	command.AddCommand(listCommand)
	command.AddCommand(setEnvironmentCommand)
	command.AddCommand(alwaysOnCommand)
	return command
}

func (a *app) newRegionsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "regions",
		Aliases: []string{"region"},
		Short:   "Inspect available placement regions",
	}

	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List regions available to the active organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}

			regions, err := client.ListRegions(ctx)
			if err != nil {
				return fmt.Errorf("list regions: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"regions": jsonList(regions)})
			}
			if len(regions) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No regions available for this api key")
				return nil
			}

			writeRegionTable(cmd.OutOrStdout(), regions)
			return nil
		},
	}

	command.AddCommand(listCommand)
	return command
}

func writeProjectTable(out io.Writer, projects []api.Project) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tNAME\tSLUG\tENVIRONMENT\tPLAN\tSTATE")
	for _, project := range projects {
		_, _ = fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\t%s\t%s\n",
			project.ID,
			project.Name,
			firstNonEmpty(project.Slug, "-"),
			firstNonEmpty(project.Environment, "-"),
			firstNonEmpty(project.Plan, "-"),
			firstNonEmpty(project.State, "-"),
		)
	}
	_ = writer.Flush()
}

func writeRegionTable(out io.Writer, regions []string) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "REGION")
	for _, region := range regions {
		_, _ = fmt.Fprintf(writer, "%s\n", region)
	}
	_ = writer.Flush()
}
