package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

// `capydb migrate squash` wraps the standalone MIT capysquash engine
// (github.com/capydatabase/capysquash) the same way
// `migrate rls` wraps capyrls: the scan finds the drifted history, this verb
// points the tool at it. It stays an exec wrapper - the engine parses SQL
// with pg_query_go (cgo) and this CLI is CGO_ENABLED=0 cross-compiled, so
// linking it in-process is off the table.
//
// Read-only by default (analyze); consolidation runs only through an explicit
// --workflow safe|fast. The wrapper deliberately does not mirror the
// capysquash flag surface - custom rules, configs and linting are its own job.

const (
	capysquashInstallHint = "download a capysquash release from https://github.com/capydatabase/capysquash/releases\n" +
		"  or: go install github.com/capydatabase/capysquash/cmd/capysquash@latest (requires a C toolchain)"
	externalValidationContractVersion = "capysquash.external-validation.v1"
	// Engine v0.11.0 (the last pgsquash-engine tag) emits this name for the
	// same contract.
	legacyExternalValidationContractVersion = "pgsquash.external-validation.v1"
	validationDSNEnvironmentVariable        = "CAPYSQUASH_VALIDATION_DSN"
)

func (a *app) newMigrateSquashCommand() *cobra.Command {
	var (
		workflow    string
		validation  string
		projectRef  string
		outputDir   string
		waitTimeout time.Duration
		previewTTL  int
	)

	command := &cobra.Command{
		Use:   "squash [migrations-dir]",
		Short: "Analyze or consolidate a migration history via the capysquash engine",
		Long: "Long migration histories drift from the schema they claim to describe -\n" +
			"`capydb migrate scan` measures the drift, and the fix before a move is to\n" +
			"consolidate the history into a clean baseline. This command hands the\n" +
			"directory to the open-source capysquash engine (AST-level consolidation with\n" +
			"catalog-proven equivalence; Supabase/Drizzle/Prisma aware).\n" +
			"\n" +
			"By default it runs the read-only ANALYZE workflow and changes nothing. Pass\n" +
			"--workflow safe (conservative consolidation) or --workflow fast (standard\n" +
			"consolidation) to actually consolidate. Validation uses local Docker by\n" +
			"default. Pass --validation capydb to prove the candidate in one short-lived,\n" +
			"isolated preview cell without requiring local Docker.\n" +
			"\n" +
			"Requires the capysquash binary on PATH (the legacy pgsquash name is also accepted); install with:\n" +
			"  " + capysquashInstallHint,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch workflow {
			case "analyze", "safe", "fast":
			default:
				return usageErrorf("unknown --workflow %q (analyze, safe or fast)", workflow)
			}
			switch validation {
			case "local", "capydb":
			default:
				return usageErrorf("unknown --validation %q (local or capydb)", validation)
			}
			if validation == "capydb" && workflow == "analyze" {
				return usageErrorf("--validation capydb requires --workflow safe or --workflow fast")
			}
			if previewTTL < 1 || previewTTL > 168 {
				return usageErrorf("--preview-ttl-hours must be between 1 and 168")
			}

			binary, err := findCapysquashBinary()
			if err != nil {
				return err
			}

			root := a.cwd
			if len(args) == 1 {
				root = args[0]
			}
			target := resolveMigrationsDir(root)
			// The engine's commands take migration FILES, not a directory.
			files, err := listMigrationFiles(target)
			if err != nil {
				return err
			}

			if validation == "capydb" {
				return a.runManagedSquashValidation(cmd, binary, workflow, target, files, projectRef, outputDir, waitTimeout, previewTTL)
			}

			childArgs := append([]string{workflow}, files...)
			if output := strings.TrimSpace(outputDir); output != "" && workflow != "analyze" {
				childArgs = append(childArgs, "--output", output)
			}
			child := exec.CommandContext(cmd.Context(), binary, childArgs...)
			child.Stdout = cmd.OutOrStdout()
			child.Stderr = cmd.ErrOrStderr()
			child.Stdin = cmd.InOrStdin()
			if err := child.Run(); err != nil {
				cmd.SilenceUsage = true
				return &exitcode.Error{Code: exitcode.GenericError,
					Err: fmt.Errorf("%s %s failed: %w", filepath.Base(binary), workflow, err)}
			}
			if workflow == "analyze" {
				name := filepath.Base(binary)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"\nanalysis only - nothing was changed. To consolidate:\n"+
						"  capydb migrate squash --workflow safe %s   # conservative, local Docker validation\n"+
						"  capydb migrate squash --workflow safe --validation capydb %s   # isolated CapyDB validation\n"+
						"  capydb migrate squash --workflow fast %s   # standard, local schema-diff validation\n"+
						"(or use %s directly for custom rules and configs)\n",
					target, target, target, name)
			}
			return nil
		},
	}

	command.Flags().StringVar(&workflow, "workflow", "analyze", "analyze (read-only, default), safe or fast (both consolidate)")
	command.Flags().StringVar(&validation, "validation", "local", "validation backend for consolidation: local (Docker) or capydb (isolated preview cell)")
	command.Flags().StringVar(&projectRef, "project", "", "CapyDB project id, slug, or name (with --validation capydb)")
	command.Flags().StringVarP(&outputDir, "output", "o", "", "validated output directory (with --validation capydb; default: <migrations-parent>/squashed)")
	command.Flags().DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "maximum wait for each preview operation")
	command.Flags().IntVar(&previewTTL, "preview-ttl-hours", 1, "lifetime of the isolated validation preview")
	return command
}

type capysquashExternalValidationResult struct {
	ContractVersion string   `json:"contract_version"`
	Success         bool     `json:"success"`
	Phase           string   `json:"phase"`
	ComparisonValid bool     `json:"comparison_valid"`
	HasDifferences  bool     `json:"has_differences"`
	Differences     []string `json:"differences"`
	Error           string   `json:"error,omitempty"`
}

func (a *app) runManagedSquashValidation(
	cmd *cobra.Command,
	binary, workflow, migrationsDir string,
	files []string,
	projectRef, requestedOutput string,
	waitTimeout time.Duration,
	previewTTL int,
) error {
	output := strings.TrimSpace(requestedOutput)
	if output == "" {
		output = filepath.Join(filepath.Dir(migrationsDir), "squashed")
	}
	output, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve squash output: %w", err)
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("output directory already exists: %s", output)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect squash output: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create squash output parent: %w", err)
	}

	stagingRoot, err := os.MkdirTemp(filepath.Dir(output), ".capydb-squash-verify-")
	if err != nil {
		return fmt.Errorf("create squash staging directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(stagingRoot); cleanupErr != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: remove squash staging directory: %v\n", cleanupErr)
		}
	}()
	candidateDir := filepath.Join(stagingRoot, "candidate")
	snapshotPath := filepath.Join(stagingRoot, "original.catalog.json")

	safety := "standard"
	if workflow == "safe" {
		safety = "conservative"
	}
	generateArgs := append([]string{"squash"}, files...)
	generateArgs = append(generateArgs,
		"--output", candidateDir,
		"--safety", safety,
		"--no-validate",
		"--i-know-what-im-doing",
		"--quiet",
		"--no-emoji",
	)
	if err := runCapysquash(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), binary, generateArgs, ""); err != nil {
		return fmt.Errorf("generate squash candidate: %w", err)
	}

	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return err
	}
	project, err := a.resolveProject(cmd.Context(), client, projectRef)
	if err != nil {
		return err
	}

	previewName := fmt.Sprintf("squash-verify-%x", time.Now().UnixNano())
	preview, job, err := client.CreatePreviewDatabase(cmd.Context(), project.ID, api.CreatePreviewRequest{
		Mode:     "empty",
		Name:     previewName,
		TTLHours: previewTTL,
	})
	if err != nil {
		return fmt.Errorf("create isolated validation preview: %w", err)
	}
	defer cleanupSquashPreview(client, preview.ID, waitTimeout, cmd.ErrOrStderr())

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created isolated validation preview %s\n", preview.Name)
	if job.ID != "" {
		job, err = waitForJob(cmd.Context(), cmd.ErrOrStderr(), client, job.ID, waitTimeout)
		if err != nil {
			return fmt.Errorf("wait for validation preview: %w", err)
		}
		if err := ensureCompletedJob(job, "validation preview creation"); err != nil {
			return err
		}
	}

	connections, err := client.GetPreviewConnection(cmd.Context(), preview.ID)
	if err != nil {
		return fmt.Errorf("fetch validation preview connection: %w", err)
	}
	if strings.TrimSpace(connections.DirectURL) == "" {
		return fmt.Errorf("validation preview did not return a direct connection URL")
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Applying original migrations and capturing the catalog")
	originalResult, err := runCapysquashExternal(
		cmd.Context(), cmd.ErrOrStderr(), binary, migrationsDir, connections.DirectURL,
		"--snapshot-output", snapshotPath,
	)
	if err != nil {
		return fmt.Errorf("capture original migration catalog: %w", err)
	}
	if !originalResult.Success || originalResult.Phase != "snapshot" {
		return fmt.Errorf("capture original migration catalog: engine returned an unsuccessful snapshot result")
	}

	resetJob, err := client.ResetPreviewDatabase(cmd.Context(), preview.ID)
	if err != nil {
		return fmt.Errorf("reset validation preview: %w", err)
	}
	if resetJob.ID != "" {
		resetJob, err = waitForJob(cmd.Context(), cmd.ErrOrStderr(), client, resetJob.ID, waitTimeout)
		if err != nil {
			return fmt.Errorf("wait for validation preview reset: %w", err)
		}
		if err := ensureCompletedJob(resetJob, "validation preview reset"); err != nil {
			return err
		}
	}
	connections, err = client.GetPreviewConnection(cmd.Context(), preview.ID)
	if err != nil {
		return fmt.Errorf("fetch reset validation preview connection: %w", err)
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Applying the candidate and comparing catalogs")
	comparison, compareErr := runCapysquashExternal(
		cmd.Context(), cmd.ErrOrStderr(), binary, candidateDir, connections.DirectURL,
		"--against-snapshot", snapshotPath,
	)
	if compareErr != nil {
		if comparison.ComparisonValid && comparison.HasDifferences {
			return fmt.Errorf("squash candidate is not equivalent:\n%s", strings.Join(comparison.Differences, "\n"))
		}
		return fmt.Errorf("validate squash candidate: %w", compareErr)
	}
	if !comparison.Success || !comparison.ComparisonValid || comparison.HasDifferences {
		return fmt.Errorf("squash candidate equivalence was not proven")
	}

	if err := os.Rename(candidateDir, output); err != nil {
		return fmt.Errorf("publish validated squash output: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Validated squash written to %s\n", output)
	return nil
}

func runCapysquash(ctx context.Context, stdout, stderr io.Writer, binary string, args []string, dsn string) error {
	child := exec.CommandContext(ctx, binary, args...)
	child.Stdout = stdout
	child.Stderr = stderr
	if dsn != "" {
		child.Env = append(os.Environ(), validationDSNEnvironmentVariable+"="+dsn)
	}
	if err := child.Run(); err != nil {
		return &exitcode.Error{Code: exitcode.GenericError, Err: fmt.Errorf("%s failed: %w", filepath.Base(binary), err)}
	}
	return nil
}

func runCapysquashExternal(
	ctx context.Context,
	stderr io.Writer,
	binary, migrationsPath, dsn, modeFlag, snapshotPath string,
) (capysquashExternalValidationResult, error) {
	var stdout bytes.Buffer
	args := []string{
		"validate-external", migrationsPath,
		"--dsn-env", validationDSNEnvironmentVariable,
		"--allow-existing-schema", "capydb",
		"--allow-existing-schema", "extensions",
		modeFlag, snapshotPath,
		"--json",
		"--quiet",
		"--no-emoji",
	}
	err := runCapysquash(ctx, &stdout, stderr, binary, args, dsn)

	var result capysquashExternalValidationResult
	decodeErr := json.NewDecoder(&stdout).Decode(&result)
	if decodeErr != nil {
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("decode capysquash validation result: %w", decodeErr)
	}
	if result.ContractVersion != externalValidationContractVersion &&
		result.ContractVersion != legacyExternalValidationContractVersion {
		return result, fmt.Errorf("unsupported capysquash validation contract %q", result.ContractVersion)
	}
	if err != nil {
		if result.Error != "" {
			return result, fmt.Errorf("%s", result.Error)
		}
		return result, err
	}
	return result, nil
}

func cleanupSquashPreview(client *api.Client, previewID string, waitTimeout time.Duration, errOut io.Writer) {
	cleanupTimeout := waitTimeout
	if cleanupTimeout <= 0 || cleanupTimeout > 5*time.Minute {
		cleanupTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	job, err := client.DeletePreviewDatabase(ctx, previewID)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: delete validation preview %s: %v\n", previewID, err)
		return
	}
	if job.ID == "" {
		return
	}
	job, err = waitForJob(ctx, io.Discard, client, job.ID, cleanupTimeout)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: wait for validation preview cleanup %s: %v\n", previewID, err)
		return
	}
	if err := ensureCompletedJob(job, "validation preview cleanup"); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: %v\n", err)
	}
}

// capysquashLookPath is swapped in tests.
var capysquashLookPath = exec.LookPath

// findCapysquashBinary prefers the platform CLI name and falls back to the
// bare engine binary.
func findCapysquashBinary() (string, error) {
	for _, name := range []string{"capysquash", "pgsquash"} {
		if path, err := capysquashLookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("capysquash is not on PATH - install it with:\n  %s", capysquashInstallHint)
}

// listMigrationFiles expands the target directory into its .sql files, sorted
// so migration ordering holds; a file target passes through as-is.
func listMigrationFiles(target string) ([]string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("read migrations path: %w", err)
	}
	if !info.IsDir() {
		return []string{target}, nil
	}
	var files []string
	if err := filepath.WalkDir(target, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no .sql files under %s - point me at a migrations directory or a single SQL file", target)
	}
	return files, nil
}

// resolveMigrationsDir mirrors `migrate rls`: pointed at a repo root, find the
// conventional migrations directory on its own.
func resolveMigrationsDir(root string) string {
	for _, candidate := range []string{
		filepath.Join(root, "supabase", "migrations"),
		filepath.Join(root, "migrations"),
		filepath.Join(root, "prisma", "migrations"),
		filepath.Join(root, "drizzle"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return root
}
