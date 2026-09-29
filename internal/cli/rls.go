package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	capyrls "github.com/capydatabase/capyrls"
	capyrlslive "github.com/capydatabase/capyrls/live"
)

// `capydb migrate rls` converts Supabase row-level-security policies into
// portable, vanilla Postgres. It wraps the capyrls engine
// (github.com/capydatabase/capyrls), which also ships standalone for people who
// are not moving to CapyDB. The converter's rule matches the codemod's:
// anything needing human judgment is reported, never guessed at - policies it
// cannot port mechanically are emitted commented out and listed.
//
// The default role model here is "single", unlike standalone capyrls:
// a CapyDB project connects as the credential that owns its tables, so the
// working setup is FORCE ROW LEVEL SECURITY plus the service escape, and needs
// nothing enabled. The default target is CapyDB, where the split role model
// creates no roles: it grants to app_user, the runtime role the platform
// creates once the project enables it, and the owner is the service path. The
// engine refuses a custom --app-role there (it could not be created); this
// command has no such flag, so that cannot happen here.

func (a *app) newMigrateRLSCommand() *cobra.Command {
	var (
		mode            string
		roleModel       string
		keepForAll      bool
		noServiceEscape bool
		uidType         string
		target          string
		outDir          string
		sourceURL       string
	)

	command := &cobra.Command{
		Use:   "rls [path]",
		Short: "Convert Supabase RLS policies to portable, vanilla Postgres",
		Long: `Converts Supabase row-level-security policies so they run on plain Postgres
(CapyDB or anywhere else). Reads a Supabase migrations directory, a schema
dump, or any SQL files; writes an ordered SQL bundle plus a report describing
the session context your application must set.

Supabase's auth.uid()/auth.jwt() helpers and the anon/authenticated roles do
not exist outside Supabase - unconverted policies abort an import or never
match. The default output re-homes them onto transaction-local GUCs
(app.user_id, promoted JWT claims) read by small accessor functions; pass
--mode supabase-compat for a shim that keeps policies verbatim instead.

Point it at a repo root and it finds supabase/migrations on its own. For the
most faithful input, introspect the LIVE database instead of parsing files -
migration folders drift from what is actually deployed (dropped-and-recreated
policies, SQL-editor hotfixes that never became migrations):
  capydb migrate rls --source-url "$SUPABASE_DIRECT_URL"

Supabase's auth.uid() returns uuid, and so does the converted accessor by
default. If your identity provider's subjects are not uuids (Clerk's
user_2abc... ids), pass --uid-type text: the accessor then returns text, and
the report names every uuid column the user id is compared to - change those
to text before applying, or the apply stops with "operator does not exist:
text = uuid".

The bundle is for CapyDB by default, where the default role model is
"single": the app connects as the credential that owns the tables, so the
bundle FORCEs row security and adds a service escape (app.role = 'service', or
claims with "role": "service_role" in compat mode).

--role-model split separates the two instead: request traffic runs as
app_user, the runtime role CapyDB creates once the project enables it
(POST /v1/projects/{id}/roles/app), and row security applies to it; the owner
bypasses row security (nothing is FORCEd) and is the service path for
migrations and admin jobs. The bundle creates no roles - it checks app_user
exists, then grants to it - and anon/authenticated become predicates on
app_user. Pass --target postgres to build a split bundle for another Postgres,
which creates its own app_user/app_service roles.

Policies that reference auth.users or Supabase-managed schemas are surfaced
in the report, not silently dropped, and so are SECURITY DEFINER functions
that write to tables the bundle FORCEs (they no longer bypass row security).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := a.cwd
			if len(args) == 1 {
				root = args[0]
			}
			options := capyrls.Options{
				NoSplitAll:      keepForAll,
				NoServiceEscape: noServiceEscape,
			}
			switch mode {
			case "vanilla":
				options.Mode = capyrls.ModeVanilla
			case "supabase-compat", "compat":
				options.Mode = capyrls.ModeCompat
			default:
				return usageErrorf("unknown --mode %q (vanilla or supabase-compat)", mode)
			}
			switch roleModel {
			case "single":
				options.RoleModel = capyrls.RoleSingle
			case "split":
				options.RoleModel = capyrls.RoleSplit
			default:
				return usageErrorf("unknown --role-model %q (single or split)", roleModel)
			}
			switch uidType {
			case "uuid":
				options.UIDType = capyrls.UIDUUID
			case "text":
				options.UIDType = capyrls.UIDText
			default:
				return usageErrorf("unknown --uid-type %q (uuid or text)", uidType)
			}
			switch target {
			case "capydb":
				options.Target = capyrls.TargetCapyDB
			case "postgres":
				options.Target = capyrls.TargetPostgres
			default:
				return usageErrorf("unknown --target %q (capydb or postgres)", target)
			}
			var result *capyrls.Result
			var sourceDescription string
			if url := strings.TrimSpace(sourceURL); url != "" {
				if len(args) == 1 {
					return usageErrorf("--source-url and a path argument are mutually exclusive")
				}
				catalog, err := introspectRLSSource(cmd.Context(), url)
				if err != nil {
					return fmt.Errorf("migrate rls: %w", err)
				}
				sourceDescription = "live database"
				result, err = capyrls.ConvertCatalog(catalog, options)
				if err != nil {
					return fmt.Errorf("migrate rls: %w", err)
				}
			} else {
				sources, description, err := collectRLSSources(root)
				if err != nil {
					return err
				}
				sourceDescription = description
				result, err = capyrls.Convert(sources, options)
				if err != nil {
					return fmt.Errorf("migrate rls: %w", err)
				}
			}

			written, err := writeRLSBundle(result, outDir)
			if err != nil {
				return err
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"rls": map[string]any{
						"source": sourceDescription,
						"files":  written,
						"report": result.Report,
					},
				})
			}
			writeRLSSummary(cmd, result, sourceDescription, written)
			return nil
		},
	}

	command.Flags().StringVar(&mode, "mode", "vanilla", "Output convention: vanilla (app.* GUC accessors) or supabase-compat (auth.* shim)")
	command.Flags().StringVar(&roleModel, "role-model", "single", "single: FORCE RLS, app connects as owner (default); split: runtime role app_user, RLS applies to it (on CapyDB the project's runtime role must be enabled; elsewhere creates app_user/app_service)")
	command.Flags().BoolVar(&keepForAll, "keep-for-all", false, "Keep FOR ALL policies instead of splitting them per command")
	command.Flags().BoolVar(&noServiceEscape, "no-service-escape", false, "Single role model: skip the service escape (app.role = 'service', or claims role service_role in compat mode)")
	command.Flags().StringVar(&uidType, "uid-type", "uuid", "Type the user-id accessor returns: uuid (Supabase's), or text for non-uuid subjects such as Clerk's user_... ids")
	command.Flags().StringVar(&target, "target", "capydb", "Platform the bundle is for: capydb (creates no roles), or postgres for any other Postgres (split creates its own roles)")
	command.Flags().StringVar(&outDir, "out", "capyrls", "Directory to write the SQL bundle and report into")
	command.Flags().StringVar(&sourceURL, "source-url", "", "Introspect the LIVE database (direct endpoint, read-only) instead of parsing SQL files")
	return command
}

// introspectRLSSource loads the RLS catalog from a running database - the
// server has already normalized every policy expression, and nothing depends
// on migration files being complete.
func introspectRLSSource(ctx context.Context, dsn string) (*capyrls.Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open source database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return capyrlslive.Load(ctx, db)
}

// collectRLSSources resolves the input path: a single SQL file, a directory
// containing supabase/migrations, or any directory of .sql files.
func collectRLSSources(root string) ([]capyrls.Source, string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(root)
		if err != nil {
			return nil, "", err
		}
		return []capyrls.Source{{Name: filepath.Base(root), SQL: string(data)}}, root, nil
	}

	searchRoot := root
	if nested := filepath.Join(root, "supabase", "migrations"); dirExists(nested) {
		searchRoot = nested
	}

	var files []string
	walkErr := filepath.WalkDir(searchRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if _, skip := codemodSkipDirs[entry.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".sql") {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, "", walkErr
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no .sql files found under %s - pass a schema dump (pg_dump --schema-only) or a migrations directory", searchRoot)
	}

	sources := make([]capyrls.Source, 0, len(files))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, "", err
		}
		rel, relErr := filepath.Rel(searchRoot, file)
		if relErr != nil {
			rel = filepath.Base(file)
		}
		sources = append(sources, capyrls.Source{Name: rel, SQL: string(data)})
	}
	return sources, searchRoot, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func writeRLSBundle(result *capyrls.Result, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	written := []string{}
	for _, file := range result.Files {
		target := filepath.Join(outDir, filepath.FromSlash(file.Name))
		if err := os.WriteFile(target, []byte(file.SQL), 0o644); err != nil {
			return nil, err
		}
		written = append(written, target)
	}
	reportPath := filepath.Join(outDir, "capyrls_report.md")
	if err := os.WriteFile(reportPath, []byte(result.Report.Markdown()), 0o644); err != nil {
		return nil, err
	}
	written = append(written, reportPath)
	return written, nil
}

func writeRLSSummary(cmd *cobra.Command, result *capyrls.Result, sourceDescription string, written []string) {
	out := cmd.OutOrStdout()
	converted, skipped, blocked := 0, 0, 0
	for _, outcome := range result.Report.Policies {
		switch outcome.Status {
		case "converted":
			converted++
		case "skipped":
			skipped++
		case "blocked":
			blocked++
		}
	}
	_, _ = fmt.Fprintf(out, "source: %s\n", sourceDescription)
	_, _ = fmt.Fprintf(out, "policies: %d converted, %d skipped, %d need attention\n", converted, skipped, blocked)
	_, _ = fmt.Fprintln(out, "wrote:")
	for _, path := range written {
		_, _ = fmt.Fprintf(out, "  %s\n", path)
	}

	if len(result.Report.GUCs) > 0 {
		_, _ = fmt.Fprintln(out, "\nYour app sets this context per transaction (see the report for snippets):")
		for _, guc := range result.Report.GUCs {
			_, _ = fmt.Fprintf(out, "  %s (%s)\n", guc.Name, guc.Type)
		}
	}

	if blocked > 0 {
		_, _ = fmt.Fprintln(out, "\nNeeds manual attention:")
		for _, outcome := range result.Report.Policies {
			if outcome.Status == "blocked" {
				_, _ = fmt.Fprintf(out, "  %s on %s: %s\n", outcome.Policy, outcome.Table, outcome.Detail)
			}
		}
	}
	if len(result.Report.Warnings) > 0 {
		_, _ = fmt.Fprintln(out, "\nWarnings:")
		for _, warning := range result.Report.Warnings {
			_, _ = fmt.Fprintf(out, "  %s\n", warning)
		}
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\nApply order: prelude, roles/force file, policies. Read %s first.\n", filepath.Join(filepath.Dir(written[0]), "capyrls_report.md"))
}
