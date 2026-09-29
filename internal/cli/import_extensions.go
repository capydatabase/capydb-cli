package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

// importExtensionOptions are the flags withImportExtensions adds to
// `capydb import`.
type importExtensionOptions struct {
	from         string
	fromNeon     bool
	dataOnly     bool
	dryRun       bool
	supabaseDump string
	uidType      string
	rlsOut       string
}

// withImportExtensions adds provider presets (--from), the client-side
// data-only path (--data-only) and the Supabase dump restore
// (--from-supabase-dump) to `capydb import`, and --from to `import preflight`.
// They wrap the command rather than live in it: presets rewrite --source-url
// before the managed import sees it, and the two local paths replace the
// managed import entirely.
func (a *app) withImportExtensions(importCommand *cobra.Command) *cobra.Command {
	var options importExtensionOptions
	flags := importCommand.Flags()
	flags.StringVar(&options.from, "from", "", "Source provider preset that fixes up --source-url (direct/session endpoint, TLS) and warns about its traps: "+strings.Join(importPresetNames(), ", "))
	flags.BoolVar(&options.fromNeon, "from-neon", false, "Shorthand for --from neon")
	flags.BoolVar(&options.dataOnly, "data-only", false, "Copy rows from --source-url into the tables that already exist on the project, parents before children (runs from this machine; target tables must be empty)")
	flags.BoolVar(&options.dryRun, "dry-run", false, "With --data-only or --from-supabase-dump: show the plan without writing anything")
	flags.StringVar(&options.supabaseDump, "from-supabase-dump", "", "Restore a Supabase pg_dump custom-format file: auth shim, schema, data, then the RLS policies converted by capyrls (runs from this machine; needs pg_restore and psql)")
	flags.StringVar(&options.uidType, "uid-type", "uuid", "With --from-supabase-dump: type auth.uid() returns - uuid, or text for non-uuid subjects such as Clerk's user_... ids")
	flags.StringVar(&options.rlsOut, "rls-out", "capyrls", "With --from-supabase-dump: directory for the converted policy bundle and its report")

	originalPreRun := importCommand.PreRunE
	importCommand.PreRunE = func(cmd *cobra.Command, args []string) error {
		if err := applyPresetFlag(cmd, &options); err != nil {
			return err
		}
		if originalPreRun != nil {
			return originalPreRun(cmd, args)
		}
		return nil
	}

	originalRun := importCommand.RunE
	importCommand.RunE = func(cmd *cobra.Command, args []string) error {
		switch {
		case options.dataOnly && options.supabaseDump != "":
			return usageErrorf("--data-only and --from-supabase-dump are separate import paths; pick one")
		case options.dataOnly:
			return a.runDataOnlyImport(cmd, options)
		case options.supabaseDump != "":
			return a.runSupabaseDumpImport(cmd, options)
		case options.dryRun:
			return usageErrorf("--dry-run applies to --data-only and --from-supabase-dump")
		}
		return originalRun(cmd, args)
	}

	for _, sub := range importCommand.Commands() {
		if sub.Name() != "preflight" {
			continue
		}
		var preflightOptions importExtensionOptions
		sub.Flags().StringVar(&preflightOptions.from, "from", "", "Source provider preset (see `capydb import --from`)")
		sub.Flags().BoolVar(&preflightOptions.fromNeon, "from-neon", false, "Shorthand for --from neon")
		sub.PreRunE = func(cmd *cobra.Command, args []string) error {
			return applyPresetFlag(cmd, &preflightOptions)
		}
	}
	return importCommand
}

// applyPresetFlag rewrites the --source-url flag through the --from preset
// and prints what changed.
func applyPresetFlag(cmd *cobra.Command, options *importExtensionOptions) error {
	provider := strings.TrimSpace(options.from)
	if options.fromNeon {
		if provider != "" && !strings.EqualFold(provider, "neon") {
			return usageErrorf("--from-neon conflicts with --from %s", provider)
		}
		provider = "neon"
	}
	if provider == "" {
		return nil
	}
	sourceURL, err := cmd.Flags().GetString("source-url")
	if err != nil {
		return err
	}
	if strings.TrimSpace(sourceURL) == "" {
		return usageErrorf("--from %s needs --source-url", provider)
	}
	result, err := applyImportPreset(provider, sourceURL)
	if err != nil {
		return err
	}
	if err := cmd.Flags().Set("source-url", result.URL); err != nil {
		return err
	}
	errOut := cmd.ErrOrStderr()
	for _, change := range result.Changes {
		_, _ = fmt.Fprintf(errOut, "%s preset: %s\n", provider, change)
	}
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(errOut, "%s preset: note: %s\n", provider, warning)
	}
	return nil
}

func (a *app) runDataOnlyImport(cmd *cobra.Command, options importExtensionOptions) error {
	ctx := cmd.Context()
	flags := cmd.Flags()
	sourceURL, _ := flags.GetString("source-url")
	dumpFile, _ := flags.GetString("file")
	follow, _ := flags.GetBool("follow")
	recreate, _ := flags.GetBool("recreate")
	confirm, _ := flags.GetBool("confirm")
	projectRef, _ := flags.GetString("project")
	switch {
	case strings.TrimSpace(sourceURL) == "":
		return usageErrorf("--data-only needs --source-url")
	case strings.TrimSpace(dumpFile) != "":
		return usageErrorf("--data-only copies from --source-url; for a dump file use `pg_restore --data-only` or `capydb import --file`")
	case follow, recreate:
		return usageErrorf("--data-only cannot be combined with --follow or --recreate")
	}
	progress := cmd.OutOrStdout()
	if a.jsonOutput() {
		progress = cmd.ErrOrStderr()
	}

	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return err
	}
	project, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return err
	}
	connections, err := client.GetProjectConnection(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("fetch project connections: %w", err)
	}
	schema, err := client.GetProjectSchema(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("fetch project schema: %w", err)
	}

	src, err := pgx.Connect(ctx, strings.TrimSpace(sourceURL))
	if err != nil {
		return fmt.Errorf("connect to the source: %w", err)
	}
	defer func() { _ = src.Close(context.Background()) }()
	dst, err := pgx.Connect(ctx, connections.DirectURL)
	if err != nil {
		return fmt.Errorf("connect to project %s: %w", project.Name, err)
	}
	defer func() { _ = dst.Close(context.Background()) }()

	plan, err := planDataOnlyImport(ctx, src, dst, schema)
	if err != nil {
		return err
	}
	if len(plan.Tables) == 0 {
		return fmt.Errorf("nothing to copy: no table of project %s exists in the source (apply your schema to the project first)", project.Name)
	}

	if options.dryRun {
		if a.jsonOutput() {
			return printJSON(cmd.OutOrStdout(), map[string]any{"dry_run": true, "plan": plan})
		}
		writeDataOnlyPlan(progress, plan, "Would copy")
		return nil
	}

	confirmed, err := confirmProjectDestructiveAction(cmd, project, confirm,
		"This will copy rows into the LIVE database for project %q (%s).\n")
	if err != nil {
		return err
	}
	if !confirmed {
		return fmt.Errorf("import not confirmed; pass --confirm or confirm interactively")
	}

	_, _ = fmt.Fprintf(progress, "Copying %d table(s) into %s, parents first:\n", len(plan.Tables), project.Name)
	if err := runDataOnlyCopy(ctx, src, dst, &plan, progress); err != nil {
		return fmt.Errorf("data-only import rolled back, nothing was written: %w", err)
	}
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{"imported": true, "plan": plan})
	}
	for _, note := range plan.Notes {
		_, _ = fmt.Fprintf(progress, "note: %s\n", note)
	}
	_, _ = fmt.Fprintln(progress, "Data-only import complete.")
	return nil
}

func writeDataOnlyPlan(out interface{ Write([]byte) (int, error) }, plan dataOnlyPlan, verb string) {
	_, _ = fmt.Fprintf(out, "%s %d table(s), in this order:\n", verb, len(plan.Tables))
	for i, table := range plan.Tables {
		_, _ = fmt.Fprintf(out, "  %d. %s (%d columns)\n", i+1, table.key(), len(table.Columns))
	}
	if plan.DeferConstraints {
		_, _ = fmt.Fprintln(out, "Foreign keys form a cycle; its deferrable constraints are checked at commit.")
	}
	for _, view := range plan.MaterializedViews {
		_, _ = fmt.Fprintf(out, "Then refresh %s.\n", view)
	}
	for _, note := range plan.Notes {
		_, _ = fmt.Fprintf(out, "note: %s\n", note)
	}
}
