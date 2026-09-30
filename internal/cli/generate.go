package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// newGenerateCommand groups the code generators that render the linked
// database's live schema as source code. Every language is generated
// server-side, one implementation shared with the API and the MCP server.
func (a *app) newGenerateCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "generate",
		Short: "Generate code from the database schema",
		Long:  "Generates typed source code (TypeScript interfaces, Zod schemas, a Drizzle schema, Go structs, or Python models) from the live schema of the linked project or a preview database.",
	}
	command.AddCommand(a.newGenerateSubcommand(generatorSpec{
		use: "types", short: "Generate TypeScript types from the database schema", language: "typescript",
	}))
	command.AddCommand(a.newGenerateSubcommand(generatorSpec{
		use: "zod", short: "Generate Zod schemas from the database schema", language: "zod",
	}))
	command.AddCommand(a.newGenerateSubcommand(generatorSpec{
		use: "drizzle", short: "Generate a Drizzle schema from the database schema", language: "drizzle",
	}))
	command.AddCommand(a.newGenerateSubcommand(generatorSpec{
		use: "go", short: "Generate Go structs and column constants from the database schema", language: "go",
	}))
	command.AddCommand(a.newGenerateSubcommand(generatorSpec{
		use: "python", short: "Generate Python dataclasses or pydantic models from the database schema", language: "python",
	}))
	return command
}

// generatorSpec describes one `generate` subcommand.
type generatorSpec struct {
	use      string
	short    string
	language string
}

// generateOptions are the per-invocation flag values. style is the
// TypeScript or Python output shape; goPackage the Go package name.
type generateOptions struct {
	goPackage  string
	outPath    string
	print      bool
	previewID  string
	projectRef string
	style      string
	watch      bool
	watchOpts  schemaWatchOptions
}

func (a *app) newGenerateSubcommand(spec generatorSpec) *cobra.Command {
	var options generateOptions

	command := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateGenerateOptions(options); err != nil {
				return err
			}
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			target, err := a.resolveSchemaTarget(cmd.Context(), client, options.previewID, options.projectRef)
			if err != nil {
				return err
			}
			if options.watch {
				return a.runGenerateWatch(cmd, client, target, spec, options)
			}
			types, err := renderGenerated(cmd.Context(), client, target, spec, options)
			if err != nil {
				return err
			}
			return a.emitGenerated(cmd, types, options)
		},
	}

	// No -o shorthand: the root command's persistent --output (-o) owns it.
	command.Flags().StringVar(&options.outPath, "out", "", "Output file path (defaults to the generator's suggested filename)")
	command.Flags().BoolVar(&options.print, "print", false, "Print the generated code to stdout instead of writing a file")
	command.Flags().StringVar(&options.previewID, "preview", "", "Generate from a preview database instead of the project database")
	command.Flags().StringVar(&options.projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&options.watch, "watch", false, "Keep running and regenerate whenever the database schema changes")
	command.Flags().DurationVar(&options.watchOpts.interval, "watch-interval", defaultWatchInterval, "With --watch: how often to check for schema changes (backs off while nothing changes)")
	switch spec.language {
	case "typescript":
		command.Flags().StringVar(&options.style, "style", "", "TypeScript output shape: capydb (default) or supabase (compatible with supabase-js generics)")
	case "go":
		command.Flags().StringVar(&options.goPackage, "package", "db", "Go package name for the generated file")
	case "python":
		command.Flags().StringVar(&options.style, "style", "dataclass", "Python output shape: dataclass (standard library) or pydantic")
	}
	return command
}

// validateGenerateOptions rejects flag combinations. Values (--style,
// --package) are validated by the control plane, which owns the generators.
func validateGenerateOptions(options generateOptions) error {
	if options.watch && options.print {
		return usageErrorf("--watch writes a file on every change; it cannot be combined with --print")
	}
	return nil
}

// schemaTarget is what a generator reads: a project database or a preview.
type schemaTarget struct {
	project   api.Project
	previewID string
}

func (a *app) resolveSchemaTarget(ctx context.Context, client *api.Client, previewID, projectRef string) (schemaTarget, error) {
	if trimmed := strings.TrimSpace(previewID); trimmed != "" {
		return schemaTarget{previewID: trimmed}, nil
	}
	project, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return schemaTarget{}, err
	}
	return schemaTarget{project: project}, nil
}

func fetchTargetSchema(ctx context.Context, client *api.Client, target schemaTarget) (api.DatabaseSchema, error) {
	if target.previewID != "" {
		schema, err := client.GetPreviewSchema(ctx, target.previewID)
		if err != nil {
			return api.DatabaseSchema{}, fmt.Errorf("fetch preview schema: %w", err)
		}
		return schema, nil
	}
	schema, err := client.GetProjectSchema(ctx, target.project.ID)
	if err != nil {
		return api.DatabaseSchema{}, fmt.Errorf("fetch project schema: %w", err)
	}
	return schema, nil
}

// renderGenerated asks the control plane to render the target's schema.
func renderGenerated(ctx context.Context, client *api.Client, target schemaTarget, spec generatorSpec, options generateOptions) (api.GeneratedTypes, error) {
	request := api.TypegenRequest{Language: spec.language, Style: options.style}
	if spec.language == "go" {
		request.Package = options.goPackage
	}
	var types api.GeneratedTypes
	var err error
	if target.previewID != "" {
		types, err = client.GeneratePreviewSchemaTypes(ctx, target.previewID, request)
	} else {
		types, err = client.GenerateProjectSchemaTypes(ctx, target.project.ID, request)
	}
	if err != nil {
		return api.GeneratedTypes{}, fmt.Errorf("generate %s: %w", spec.language, err)
	}
	return types, nil
}

// writeGenerated writes the file and returns the path it went to.
func writeGenerated(types api.GeneratedTypes, outPath string) (string, error) {
	target := strings.TrimSpace(outPath)
	if target == "" {
		target = types.Filename
	}
	if dir := filepath.Dir(target); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create output directory: %w", err)
		}
	}
	if err := os.WriteFile(target, []byte(types.Content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", target, err)
	}
	return target, nil
}

func (a *app) emitGenerated(cmd *cobra.Command, types api.GeneratedTypes, options generateOptions) error {
	if options.print {
		if a.jsonOutput() {
			return printJSON(cmd.OutOrStdout(), types)
		}
		_, _ = fmt.Fprint(cmd.OutOrStdout(), types.Content)
		return nil
	}

	target, err := writeGenerated(types, options.outPath)
	if err != nil {
		return err
	}
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{
			"language": types.Language,
			"path":     target,
			"style":    types.Style,
		})
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %s\n", target)
	return nil
}
