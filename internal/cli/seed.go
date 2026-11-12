package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

// seedTarget is the database a seed runs against.
type seedTarget struct {
	project   api.Project
	previewID string
	directURL string
}

func (t seedTarget) production() bool {
	return t.previewID == "" && t.project.Environment != "non_production"
}

func (t seedTarget) describe() string {
	if t.previewID != "" {
		return "preview " + t.previewID
	}
	return fmt.Sprintf("project %s (%s)", t.project.Name, firstNonEmpty(t.project.Environment, "production"))
}

// seedPlan is what `capydb seed` will run: a SQL file or a command.
type seedPlan struct {
	// Kind is "sql" or "command".
	Kind    string `json:"kind"`
	File    string `json:"file,omitempty"`
	Command string `json:"command,omitempty"`
	// Source says where the plan came from (argument, --run, package.json
	// script, prisma seed config, conventional file).
	Source string `json:"source"`
}

// seedFileCandidates are the conventional seed-file locations, in order.
var seedFileCandidates = []string{
	"seed.sql",
	filepath.Join("supabase", "seed.sql"),
	filepath.Join("db", "seed.sql"),
	filepath.Join("database", "seed.sql"),
	filepath.Join("prisma", "seed.sql"),
	filepath.Join("drizzle", "seed.sql"),
}

// detectSeedPlan picks what to run when no file or --run was given: a
// package.json seed script (db:seed, then seed), Prisma's seed command, or a
// conventional seed.sql - in that order, because a script usually wraps the
// file when both exist.
func detectSeedPlan(dir string) (seedPlan, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return seedPlan{}, fmt.Errorf("read package.json: %w", err)
	}
	if err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
			Prisma  struct {
				Seed string `json:"seed"`
			} `json:"prisma"`
		}
		if err := json.Unmarshal(raw, &pkg); err != nil {
			return seedPlan{}, fmt.Errorf("parse package.json: %w", err)
		}
		for _, name := range []string{"db:seed", "seed"} {
			if strings.TrimSpace(pkg.Scripts[name]) != "" {
				return seedPlan{Kind: "command", Command: packageScriptCommand(dir, name), Source: "package.json script " + name}, nil
			}
		}
		if seed := strings.TrimSpace(pkg.Prisma.Seed); seed != "" {
			return seedPlan{Kind: "command", Command: seed, Source: "package.json prisma.seed"}, nil
		}
	}
	for _, candidate := range seedFileCandidates {
		if fileExists(filepath.Join(dir, candidate)) {
			return seedPlan{Kind: "sql", File: filepath.Join(dir, candidate), Source: "conventional file"}, nil
		}
	}
	return seedPlan{}, usageErrorf("nothing to seed with: pass a .sql file, --run \"<command>\", or add a db:seed script to package.json")
}

// packageScriptCommand runs a package.json script with the package manager
// the lockfile names.
func packageScriptCommand(dir, script string) string {
	switch {
	case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm run " + script
	case fileExists(filepath.Join(dir, "bun.lock")), fileExists(filepath.Join(dir, "bun.lockb")):
		return "bun run " + script
	case fileExists(filepath.Join(dir, "yarn.lock")):
		return "yarn run " + script
	default:
		return "npm run " + script
	}
}

// seedEnv is the environment a seed command runs with: every database
// variable name the CapyDB integrations write, all set to the DIRECT URL.
// Seeding is bulk writes plus, often, DDL-adjacent work (TRUNCATE, sequence
// resets) that the transaction pooler does not serve well; a script that
// reads its pooled variable still gets a working connection. Values already
// in the environment are replaced so a seed can never reach a database other
// than the target by accident.
func seedEnv(base []string, directURL string, extraNames ...string) []string {
	names := map[string]bool{
		"DATABASE_URL": true, "DATABASE_DIRECT_URL": true, "DATABASE_POOL_URL": true,
		"DIRECT_URL": true, "CAPYDB_DATABASE_URL": true, "POSTGRES_URL": true,
		// What the Vercel and Netlify integrations push and `vercel env pull` writes, plus the
		// Vercel Postgres names many templates read: a .env holding production under one of
		// these must not leak past the override.
		"DATABASE_URL_UNPOOLED": true, "POSTGRES_URL_NON_POOLING": true, "POSTGRES_PRISMA_URL": true,
	}
	for _, name := range extraNames {
		if strings.TrimSpace(name) != "" {
			names[name] = true
		}
	}
	env := make([]string, 0, len(base)+len(names))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if !names[key] {
			env = append(env, entry)
		}
	}
	for name := range names {
		env = append(env, name+"="+directURL)
	}
	return env
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command)
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func (a *app) newSeedCommand() *cobra.Command {
	var (
		confirmProduction bool
		dryRun            bool
		noRestorePoint    bool
		previewID         string
		projectRef        string
		runCommand        string
	)

	command := &cobra.Command{
		Use:   "seed [file.sql]",
		Short: "Load seed data into a project or preview database",
		Long: `Runs a seed against the linked project (or --project, or --preview):

  capydb seed seed.sql              apply a SQL file with psql, in one transaction
  capydb seed --run "pnpm tsx src/db/seed.ts"
                                    run a command (drizzle-seed, Prisma, any script) with
                                    DATABASE_URL, DATABASE_DIRECT_URL, DIRECT_URL,
                                    CAPYDB_DATABASE_URL and friends set to the target's direct URL
  capydb seed                       detect: package.json "db:seed"/"seed" script, then
                                    Prisma's "prisma.seed", then seed.sql, supabase/seed.sql,
                                    db/seed.sql, database/seed.sql, prisma/seed.sql, drizzle/seed.sql

Production is guarded: seeding a production project needs --confirm-production (or typing the
project name at the prompt), and creates a restore point first so the seed can be undone with
` + "`capydb restore --restore-point <id>`" + ` (skip with --no-restore-point). On the Vibe plan,
which restores from backups rather than to a point in time, the seed takes a backup first, waits
for it, and pins the restore point to that backup. Previews and non_production projects are
seeded without asking. --dry-run shows what would run.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			progress := cmd.OutOrStdout()
			if a.jsonOutput() {
				progress = cmd.ErrOrStderr()
			}

			var plan seedPlan
			switch {
			case len(args) == 1 && strings.TrimSpace(runCommand) != "":
				return usageErrorf("pass a SQL file or --run, not both")
			case len(args) == 1:
				if !strings.EqualFold(filepath.Ext(args[0]), ".sql") {
					return usageErrorf("%s is not a .sql file; run scripts with --run \"<command>\"", args[0])
				}
				if !fileExists(args[0]) {
					return usageErrorf("%s does not exist", args[0])
				}
				plan = seedPlan{Kind: "sql", File: args[0], Source: "argument"}
			case strings.TrimSpace(runCommand) != "":
				plan = seedPlan{Kind: "command", Command: strings.TrimSpace(runCommand), Source: "--run"}
			default:
				detected, err := detectSeedPlan(a.cwd)
				if err != nil {
					return err
				}
				plan = detected
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			target, err := a.resolveSeedTarget(ctx, client, previewID, projectRef)
			if err != nil {
				return err
			}

			what := plan.File
			if plan.Kind == "command" {
				what = plan.Command
			}
			if dryRun {
				if a.jsonOutput() {
					return printJSON(cmd.OutOrStdout(), map[string]any{"dry_run": true, "plan": plan, "target": target.describe(), "production": target.production()})
				}
				_, _ = fmt.Fprintf(progress, "Would seed %s with %s (%s).\n", target.describe(), what, plan.Source)
				return nil
			}

			var restorePoint *api.RestorePoint
			if target.production() {
				confirmed, err := confirmProjectDestructiveAction(cmd, target.project, confirmProduction,
					"Project %q (%s) is a PRODUCTION project; the seed writes into its live database.\n")
				if err != nil {
					return err
				}
				if !confirmed {
					return usageErrorf("refusing to seed production project %s without confirmation: pass --confirm-production, seed a preview (--preview), or mark the project non_production", target.project.Name)
				}
				if !noRestorePoint {
					point, undo, err := createSeedRestorePoint(ctx, progress, cmd.ErrOrStderr(), client, target.project, what)
					if err != nil {
						return fmt.Errorf("create restore point before seeding (pass --no-restore-point to seed without one): %w", err)
					}
					restorePoint = &point
					_, _ = fmt.Fprintf(progress, "Created restore point %s; undo the seed with `%s`.\n", point.ID, undo)
				}
			}

			_, _ = fmt.Fprintf(progress, "Seeding %s with %s (%s)\n", target.describe(), what, plan.Source)
			switch plan.Kind {
			case "sql":
				err = runPsqlFile(ctx, progress, cmd.ErrOrStderr(), target.directURL, plan.File)
			default:
				process := shellCommand(ctx, plan.Command)
				process.Dir = a.cwd
				process.Env = seedEnv(os.Environ(), target.directURL, a.linkedEnvVarNames()...)
				process.Stdin = os.Stdin
				process.Stdout = progress
				process.Stderr = cmd.ErrOrStderr()
				if runErr := process.Run(); runErr != nil {
					err = fmt.Errorf("seed command %q: %w", plan.Command, runErr)
				}
			}
			if err != nil {
				if restorePoint != nil {
					return fmt.Errorf("%w (restore point %s was taken before the seed)", err, restorePoint.ID)
				}
				return err
			}

			if a.jsonOutput() {
				payload := map[string]any{"seeded": true, "plan": plan, "target": target.describe()}
				if restorePoint != nil {
					payload["restore_point_id"] = restorePoint.ID
				}
				return printJSON(cmd.OutOrStdout(), payload)
			}
			_, _ = fmt.Fprintln(progress, "Seed complete.")
			return nil
		},
	}

	command.Flags().StringVar(&runCommand, "run", "", "Command to run with the database env vars set to the target (e.g. \"pnpm tsx src/db/seed.ts\")")
	command.Flags().StringVar(&previewID, "preview", "", "Seed a preview database instead of the project database")
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&confirmProduction, "confirm-production", false, "Allow seeding a production project without the interactive prompt")
	command.Flags().BoolVar(&noRestorePoint, "no-restore-point", false, "Do not create a restore point before seeding a production project")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would run and against which database, without running it")
	return command
}

// seedBackupWaitTimeout bounds the wait for the backup a seed on the Vibe plan
// takes first.
const seedBackupWaitTimeout = 30 * time.Minute

// createSeedRestorePoint pins where a production seed can be undone from and
// returns the point plus the restore command that undoes the seed. Plans with
// point-in-time recovery pin the current instant (kind pitr), which costs
// nothing. Vibe restores from backups only - the API refuses a pitr restore
// there - so on Vibe the seed takes a backup first, waits for it, and pins the
// restore point to that backup (kind backup). A backup whose restore
// verification did not finish needs --allow-unverified to restore from, so the
// printed command carries it then.
func createSeedRestorePoint(ctx context.Context, progress, errOut io.Writer, client *api.Client, project api.Project, what string) (api.RestorePoint, string, error) {
	label := "before capydb seed " + time.Now().UTC().Format("2006-01-02T15:04:05Z")
	request := api.CreateRestorePointRequest{
		Kind:  "pitr",
		Label: label,
		Note:  "created by capydb seed (" + what + ")",
	}
	verified := true
	if project.Plan == "vibe" {
		_, _ = fmt.Fprintf(progress, "Backing up %s before the seed (the Vibe plan restores from backups, not to a point in time)\n", project.Name)
		backup, err := takeLabelledBackup(ctx, errOut, client, project.ID, label)
		if err != nil {
			return api.RestorePoint{}, "", err
		}
		request.Kind = "backup"
		request.BackupKey = backup.BackupKey
		verified = backup.VerificationState == "verified"
	}
	point, err := client.CreateRestorePoint(ctx, project.ID, request)
	if err != nil {
		return api.RestorePoint{}, "", err
	}
	undo := "capydb restore --restore-point " + point.ID
	if !verified {
		undo += " --allow-unverified"
	}
	return point, undo, nil
}

// takeLabelledBackup queues a backup under label, waits for it, and returns the
// backup it recorded. The job does not carry the backup's key, so the backup is
// found again by its label, which the caller makes unique.
func takeLabelledBackup(ctx context.Context, errOut io.Writer, client *api.Client, projectID, label string) (api.Backup, error) {
	job, err := client.CreateBackup(ctx, projectID, label)
	if err != nil {
		return api.Backup{}, fmt.Errorf("queue backup: %w", err)
	}
	job, err = waitForJob(ctx, errOut, client, job.ID, seedBackupWaitTimeout)
	if err != nil {
		return api.Backup{}, err
	}
	if err := ensureCompletedJob(job, "backup "+job.ID); err != nil {
		return api.Backup{}, err
	}
	backups, err := client.ListBackups(ctx, projectID)
	if err != nil {
		return api.Backup{}, fmt.Errorf("list backups: %w", err)
	}
	for _, backup := range backups {
		if backup.Label == label {
			return backup, nil
		}
	}
	return api.Backup{}, fmt.Errorf("backup job %s completed but no backup labelled %q is listed", job.ID, label)
}

func (a *app) resolveSeedTarget(ctx context.Context, client *api.Client, previewID, projectRef string) (seedTarget, error) {
	if trimmed := strings.TrimSpace(previewID); trimmed != "" {
		connections, err := client.GetPreviewConnection(ctx, trimmed)
		if err != nil {
			return seedTarget{}, fmt.Errorf("fetch preview connections: %w", err)
		}
		if strings.TrimSpace(connections.DirectURL) == "" {
			return seedTarget{}, fmt.Errorf("preview %s has no direct connection URL yet", trimmed)
		}
		return seedTarget{previewID: trimmed, directURL: connections.DirectURL}, nil
	}
	project, err := a.resolveProject(ctx, client, projectRef)
	if err != nil {
		return seedTarget{}, err
	}
	connections, err := client.GetProjectConnection(ctx, project.ID)
	if err != nil {
		return seedTarget{}, fmt.Errorf("fetch project connections: %w", err)
	}
	if strings.TrimSpace(connections.DirectURL) == "" {
		return seedTarget{}, fmt.Errorf("project %s has no direct connection URL yet", project.Name)
	}
	return seedTarget{project: project, directURL: connections.DirectURL}, nil
}

// linkedEnvVarNames returns the database variable names the local link
// recorded (they differ per framework), so a seed command sees the target
// under the name its own code reads.
func (a *app) linkedEnvVarNames() []string {
	linkConfig, err := config.LoadProjectConfig(a.cwd)
	if err != nil {
		return nil
	}
	return []string{linkConfig.DatabaseURLVar, linkConfig.DirectURLVar, linkConfig.PooledURLVar}
}
