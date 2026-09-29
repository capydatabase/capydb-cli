package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newInitCommand groups project scaffolding. `capydb init drizzle` wires a
// linked project for Drizzle ORM: config, client factory and a schema
// generated from the live database.
func (a *app) newInitCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "init",
		Short: "Scaffold framework integrations for the linked project",
	}
	command.AddCommand(a.newInitDrizzleCommand())
	return command
}

const drizzleConfigTemplate = `import { defineConfig } from "drizzle-kit";

export default defineConfig({
  dialect: "postgresql",
  schema: "./%s",
  out: "./drizzle",
  // drizzle-kit v1 manages ALL schemas by default. Keep push/pull scoped to
  // the schemas you own: extension-created schemas (e.g. cron from pg_cron)
  // and platform objects must not be offered for DROP.
  schemaFilter: ["public"],
  // pg_stat_statements powers the cell's slow-query view and lives in public on
  // older cells; its view is extension-owned, so drizzle-kit would try to DROP
  // it on every push. Exclude it so push/pull leaves it alone.
  tablesFilter: ["!pg_stat_statements"],
  dbCredentials: {
    // DDL and migrations go over the direct connection; the pooled URL
    // (:6432, transaction-mode PgBouncer) is for application traffic only.
    url: process.env.DATABASE_DIRECT_URL ?? process.env.DATABASE_URL!,
  },
});
`

const drizzleClientTemplate = `import { createDb } from "@capydb/drizzle";

// createDb resolves CAPYDB_DATABASE_URL / DATABASE_URL and applies the
// CapyDB pooler rules automatically (prepare: false and a tiny pool through
// the :6432 transaction-mode pooler). Import tables from ./schema directly in
// queries; for the relational query API (db.query.*), build relations with
// drizzle's defineRelations and pass them: createDb({ relations }).
export const db = createDb();

export * from "./schema";
`

func (a *app) newInitDrizzleCommand() *cobra.Command {
	var (
		dir        string
		force      bool
		projectRef string
		schemaPath string
	)

	command := &cobra.Command{
		Use:   "drizzle",
		Short: "Scaffold Drizzle ORM wired to the linked database",
		Long: "Writes drizzle.config.ts, a src/db client factory using @capydb/drizzle, and a Drizzle schema generated from the live database. " +
			"Run `capydb link` (or `capydb create`) first so the project and its environment variables exist.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}

			types, err := client.GenerateProjectSchemaTypes(ctx, project.ID, "drizzle", "")
			if err != nil {
				return fmt.Errorf("generate drizzle schema: %w", err)
			}

			schemaFile := filepath.Join(dir, filepath.FromSlash(schemaPath))
			clientFile := filepath.Join(filepath.Dir(schemaFile), "index.ts")
			configFile := filepath.Join(dir, "drizzle.config.ts")

			files := []struct {
				path    string
				content string
			}{
				{configFile, fmt.Sprintf(drizzleConfigTemplate, filepath.ToSlash(schemaPath))},
				{schemaFile, types.Content},
				{clientFile, drizzleClientTemplate},
			}

			if !force {
				for _, file := range files {
					if _, err := os.Stat(file.path); err == nil {
						return fmt.Errorf("%s already exists; re-run with --force to overwrite", file.path)
					}
				}
			}

			written := make([]string, 0, len(files))
			for _, file := range files {
				if dir := filepath.Dir(file.path); dir != "." && dir != "" {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return fmt.Errorf("create %s: %w", dir, err)
					}
				}
				if err := os.WriteFile(file.path, []byte(file.content), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", file.path, err)
				}
				written = append(written, file.path)
			}

			installHint := installCommandHint(dir)

			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"files":   written,
					"install": installHint,
					"project": project.ID,
				})
			}

			out := cmd.ErrOrStderr()
			for _, path := range written {
				_, _ = fmt.Fprintf(out, "Wrote %s\n", path)
			}
			_, _ = fmt.Fprintf(out, "\nNext steps:\n")
			_, _ = fmt.Fprintf(out, "  1. Install dependencies: %s\n", installHint)
			_, _ = fmt.Fprintf(out, "  2. Ensure DATABASE_URL / DATABASE_DIRECT_URL are set (capydb env pull)\n")
			_, _ = fmt.Fprintf(out, "  3. Query away: import { db } from \"./%s\"\n", filepath.ToSlash(filepath.Join(filepath.Dir(schemaPath), "index")))
			return nil
		},
	}

	command.Flags().StringVar(&dir, "dir", ".", "Directory to scaffold into")
	command.Flags().BoolVar(&force, "force", false, "Overwrite existing files")
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&schemaPath, "schema", "src/db/schema.ts", "Path for the generated Drizzle schema (relative to --dir)")
	return command
}

// installCommandHint picks the install command for the project's package
// manager, detected from its lockfile. drizzle-orm/drizzle-kit use the v1 `rc`
// dist-tag: @capydb/drizzle types against drizzle v1, and the platform policy
// is bleeding-edge.
func installCommandHint(dir string) string {
	packages := "@capydb/drizzle drizzle-orm@rc postgres"
	devPackages := "drizzle-kit@rc"
	switch {
	case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
		return fmt.Sprintf("pnpm add %s && pnpm add -D %s", packages, devPackages)
	case fileExists(filepath.Join(dir, "bun.lock")), fileExists(filepath.Join(dir, "bun.lockb")):
		return fmt.Sprintf("bun add %s && bun add -d %s", packages, devPackages)
	case fileExists(filepath.Join(dir, "yarn.lock")):
		return fmt.Sprintf("yarn add %s && yarn add -D %s", packages, devPackages)
	default:
		return fmt.Sprintf("npm install %s && npm install -D %s", packages, devPackages)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
