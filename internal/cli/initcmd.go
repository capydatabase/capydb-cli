package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// newInitCommand groups project scaffolding: `capydb init drizzle|prisma|kysely`
// (or `capydb init --orm <name>`) wires a linked project for that ORM with the
// CapyDB connection rules baked in - DDL and migrations on the direct URL,
// application traffic on the pooled one.
func (a *app) newInitCommand() *cobra.Command {
	var options initOptions
	var orm string

	command := &cobra.Command{
		Use:   "init",
		Short: "Scaffold ORM integrations for the linked project",
		Long: "Scaffolds an ORM wired to the linked project: `capydb init drizzle`, `capydb init prisma`, or `capydb init kysely` " +
			"(`capydb init --orm <name>` is the same). Run `capydb link` (or `capydb create`) first so the project and its environment variables exist.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			scaffold, ok := initScaffolds[strings.ToLower(strings.TrimSpace(orm))]
			if !ok {
				if strings.TrimSpace(orm) == "" {
					return cmd.Help()
				}
				return usageErrorf("unknown --orm %q; available: %s", orm, strings.Join(initScaffoldNames(), ", "))
			}
			return a.runInit(cmd, scaffold, options)
		},
	}
	command.Flags().StringVar(&orm, "orm", "", "ORM to scaffold: "+strings.Join(initScaffoldNames(), ", "))
	addInitFlags(command, &options)

	for _, name := range initScaffoldNames() {
		command.AddCommand(a.newInitORMCommand(initScaffolds[name]))
	}
	return command
}

// initOptions are the flags every scaffold shares.
type initOptions struct {
	dir        string
	force      bool
	projectRef string
	schemaPath string
}

func addInitFlags(command *cobra.Command, options *initOptions) {
	command.Flags().StringVar(&options.dir, "dir", ".", "Directory to scaffold into")
	command.Flags().BoolVar(&options.force, "force", false, "Overwrite existing files")
	command.Flags().StringVar(&options.projectRef, "project", "", "Project id, slug, or name")
	command.Flags().StringVar(&options.schemaPath, "schema", "", "Path of the generated schema/types file, relative to --dir (default per ORM)")
}

// initFile is one file a scaffold writes, relative to --dir.
type initFile struct {
	path    string
	content string
}

// initScaffold describes one ORM integration.
type initScaffold struct {
	name        string
	short       string
	long        string
	schemaPath  string
	packages    string
	devPackages string
	// files renders the scaffold; schema is the path of the generated file.
	files func(ctx context.Context, client *api.Client, project api.Project, schema string) ([]initFile, error)
	// nextSteps are printed after the install hint.
	nextSteps func(schema string) []string
}

var initScaffolds = map[string]initScaffold{
	"drizzle": {
		name:  "drizzle",
		short: "Scaffold Drizzle ORM wired to the linked database",
		long: "Writes drizzle.config.ts, a src/db client factory using @capydb/drizzle, and a Drizzle schema generated from the live database. " +
			"Run `capydb link` (or `capydb create`) first so the project and its environment variables exist.",
		schemaPath: "src/db/schema.ts",
		// drizzle-orm/drizzle-kit use the v1 `rc` dist-tag: @capydb/drizzle
		// types against drizzle v1, and the platform policy is bleeding-edge.
		packages:    "@capydb/drizzle drizzle-orm@rc postgres",
		devPackages: "drizzle-kit@rc",
		files: func(ctx context.Context, client *api.Client, project api.Project, schema string) ([]initFile, error) {
			types, err := client.GenerateProjectSchemaTypes(ctx, project.ID, "drizzle", "")
			if err != nil {
				return nil, fmt.Errorf("generate drizzle schema: %w", err)
			}
			return []initFile{
				{"drizzle.config.ts", fmt.Sprintf(drizzleConfigTemplate, schema)},
				{schema, types.Content},
				{filepath.ToSlash(filepath.Join(filepath.Dir(schema), "index.ts")), drizzleClientTemplate},
			}, nil
		},
		nextSteps: func(schema string) []string {
			return []string{
				"Ensure DATABASE_URL / DATABASE_DIRECT_URL are set (capydb env pull)",
				fmt.Sprintf("Query away: import { db } from \"./%s\"", filepath.ToSlash(filepath.Join(filepath.Dir(schema), "index"))),
			}
		},
	},
	"prisma": {
		name:  "prisma",
		short: "Scaffold Prisma ORM (v7) wired to the linked database",
		long: "Writes prisma.config.ts (migrations and introspection on the direct URL), prisma/schema.prisma, and a client module using the " +
			"@prisma/adapter-pg driver adapter on the pooled URL. Models come from the live database with `prisma db pull` - Prisma's own introspection, " +
			"so the schema is exactly what Prisma expects. Targets Prisma ORM 7 (driver adapters, prisma.config.ts).",
		schemaPath:  "prisma/schema.prisma",
		// Pinned to 7: the prisma CLI's latest dist-tag points at an 8.x
		// release candidate whose CLI has no `db pull`/`generate`.
		packages:    "@prisma/client@7 @prisma/adapter-pg@7 pg dotenv",
		devPackages: "prisma@7 @types/pg",
		files: func(_ context.Context, _ *api.Client, _ api.Project, schema string) ([]initFile, error) {
			clientPath := "src/db.ts"
			generated := "src/generated/prisma"
			output, err := filepath.Rel(filepath.Dir(filepath.FromSlash(schema)), filepath.FromSlash(generated))
			if err != nil {
				return nil, err
			}
			importPath, err := filepath.Rel(filepath.Dir(filepath.FromSlash(clientPath)), filepath.FromSlash(generated+"/client"))
			if err != nil {
				return nil, err
			}
			return []initFile{
				{"prisma.config.ts", fmt.Sprintf(prismaConfigTemplate, schema, filepath.ToSlash(filepath.Join(filepath.Dir(schema), "migrations")))},
				{schema, fmt.Sprintf(prismaSchemaTemplate, filepath.ToSlash(output))},
				{clientPath, fmt.Sprintf(prismaClientTemplate, relativeImport(importPath))},
			}, nil
		},
		nextSteps: func(string) []string {
			return []string{
				"Ensure DATABASE_URL / DATABASE_DIRECT_URL are set (capydb env pull)",
				"Pull the models from the live database: npx prisma db pull",
				"Generate the client: npx prisma generate",
				"Query away: import { prisma } from \"./src/db\"",
			}
		},
	},
	"kysely": {
		name:  "kysely",
		short: "Scaffold Kysely wired to the linked database",
		long: "Writes TypeScript types generated from the live database (`capydb generate types`) and a src/db client that maps them onto Kysely's " +
			"table types - columns Postgres fills in are optional on insert, generated columns cannot be written - on the pooled URL. " +
			"Regenerate the types after a migration with `capydb generate types --out <path>` (or keep `--watch` running).",
		schemaPath:  "src/db/database.types.ts",
		packages:    "kysely pg",
		devPackages: "@types/pg",
		files: func(ctx context.Context, client *api.Client, project api.Project, schema string) ([]initFile, error) {
			types, err := client.GenerateProjectSchemaTypes(ctx, project.ID, "typescript", "")
			if err != nil {
				return nil, fmt.Errorf("generate typescript types: %w", err)
			}
			typesModule := "./" + strings.TrimSuffix(filepath.Base(schema), ".ts")
			return []initFile{
				{schema, types.Content},
				{filepath.ToSlash(filepath.Join(filepath.Dir(schema), "index.ts")), fmt.Sprintf(kyselyClientTemplate, typesModule)},
			}, nil
		},
		nextSteps: func(schema string) []string {
			return []string{
				"Ensure DATABASE_URL is set (capydb env pull)",
				fmt.Sprintf("Query away: import { db } from \"./%s\"", filepath.ToSlash(filepath.Join(filepath.Dir(schema), "index"))),
				fmt.Sprintf("After a migration: capydb generate types --out %s", schema),
			}
		},
	},
}

func initScaffoldNames() []string {
	names := make([]string, 0, len(initScaffolds))
	for name := range initScaffolds {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// relativeImport renders a relative module specifier ("./x", "../x").
func relativeImport(path string) string {
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "../") {
		return path
	}
	return "./" + path
}

func (a *app) newInitORMCommand(scaffold initScaffold) *cobra.Command {
	var options initOptions
	command := &cobra.Command{
		Use:   scaffold.name,
		Short: scaffold.short,
		Long:  scaffold.long,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runInit(cmd, scaffold, options)
		},
	}
	addInitFlags(command, &options)
	return command
}

func (a *app) runInit(cmd *cobra.Command, scaffold initScaffold, options initOptions) error {
	ctx := cmd.Context()
	schema := filepath.ToSlash(firstNonEmpty(options.schemaPath, scaffold.schemaPath))
	if filepath.IsAbs(schema) || strings.HasPrefix(filepath.Clean(schema), "..") {
		return usageErrorf("--schema must be a path inside --dir")
	}

	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return err
	}
	project, err := a.resolveProject(ctx, client, options.projectRef)
	if err != nil {
		return err
	}
	files, err := scaffold.files(ctx, client, project, schema)
	if err != nil {
		return err
	}

	if !options.force {
		for _, file := range files {
			if path := filepath.Join(options.dir, filepath.FromSlash(file.path)); fileExists(path) {
				return fmt.Errorf("%s already exists; re-run with --force to overwrite", path)
			}
		}
	}
	written := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.Join(options.dir, filepath.FromSlash(file.path))
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(path, []byte(file.content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, path)
	}

	installHint := installCommandHint(options.dir, scaffold.packages, scaffold.devPackages)
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{
			"files":   written,
			"install": installHint,
			"orm":     scaffold.name,
			"project": project.ID,
		})
	}

	out := cmd.ErrOrStderr()
	for _, path := range written {
		_, _ = fmt.Fprintf(out, "Wrote %s\n", path)
	}
	_, _ = fmt.Fprintf(out, "\nNext steps:\n")
	_, _ = fmt.Fprintf(out, "  1. Install dependencies: %s\n", installHint)
	for i, step := range scaffold.nextSteps(schema) {
		_, _ = fmt.Fprintf(out, "  %d. %s\n", i+2, step)
	}
	return nil
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

const prismaConfigTemplate = `import "dotenv/config";
import { defineConfig, env } from "prisma/config";

export default defineConfig({
  schema: "%s",
  migrations: { path: "%s" },
  datasource: {
    // Migrations and introspection go over the direct connection: the pooled
    // URL (:6432, transaction-mode PgBouncer) cannot hold the session state
    // and advisory locks prisma migrate needs. The client uses the pooled URL.
    url: process.env.DATABASE_DIRECT_URL ?? env("DATABASE_URL"),
  },
});
`

const prismaSchemaTemplate = `// Models come from the live database: run ` + "`npx prisma db pull`" + `.
// The connection URL lives in prisma.config.ts.

generator client {
  provider = "prisma-client"
  output   = "%s"
}

datasource db {
  provider = "postgresql"
}
`

const prismaClientTemplate = `import { PrismaPg } from "@prisma/adapter-pg";

import { PrismaClient } from "%s";

// Application traffic goes through the pooled URL (DATABASE_URL, :6432,
// transaction-mode PgBouncer). node-postgres sends unnamed statements, which
// work through it. Keep the pool small: every serverless instance opens its
// own connections.
const adapter = new PrismaPg({ connectionString: process.env.DATABASE_URL, max: 5 });

export const prisma = new PrismaClient({ adapter });
`

const kyselyClientTemplate = `import { Kysely, PostgresDialect, type ColumnType } from "kysely";
import pg from "pg";

import type { Database } from "%s";

// Kysely wants one type per table whose columns carry separate select,
// insert and update types. The generated file already has all three per
// table (Row, Insert, Update); this maps them onto Kysely's ColumnType: a
// column missing from Insert (generated, GENERATED ALWAYS) cannot be
// inserted, and an optional Insert key stays optional.
type KyselyTable<T extends { Row: object; Insert: object; Update: object }> = {
  [C in keyof T["Row"]]: ColumnType<
    T["Row"][C],
    C extends keyof T["Insert"] ? T["Insert"][C] : never,
    C extends keyof T["Update"] ? Exclude<T["Update"][C], undefined> : never
  >;
};

type Relations<S extends keyof Database> = {
  [T in keyof Database[S]["Tables"] & string as S extends "public" ? T : ` + "`${S & string}.${T}`" + `]: Database[S]["Tables"][T] extends {
    Row: object;
    Insert: object;
    Update: object;
  }
    ? KyselyTable<Database[S]["Tables"][T]>
    : never;
} & {
  [V in keyof Database[S]["Views"] & string as S extends "public" ? V : ` + "`${S & string}.${V}`" + `]: Database[S]["Views"][V] extends {
    Row: infer R;
  }
    ? R
    : never;
};

type UnionToIntersection<U> = (U extends unknown ? (arg: U) => void : never) extends (arg: infer I) => void ? I : never;

// Every schema's tables and views; relations outside public are "schema.table".
export type DB = UnionToIntersection<{ [S in keyof Database]: Relations<S> }[keyof Database]>;

// Application traffic goes through the pooled URL (DATABASE_URL, :6432,
// transaction-mode PgBouncer). node-postgres sends unnamed statements, which
// work through it. Keep the pool small: every serverless instance opens its
// own connections.
export const db = new Kysely<DB>({
  dialect: new PostgresDialect({
    pool: new pg.Pool({ connectionString: process.env.DATABASE_URL, max: 5 }),
  }),
});
`

// installCommandHint builds the install command for the project's package
// manager, detected from its lockfile.
func installCommandHint(dir, packages, devPackages string) string {
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
