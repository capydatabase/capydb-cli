package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/envfile"
	"github.com/capydatabase/capydbclient"
)

// Environment variable names for a K/V store. These are CapyDB's own rather
// than the `UPSTASH_*` pair, which is why `@upstash/redis`'s `Redis.fromEnv()`
// does not find them and `@capydb/kv` exists - see capydb-kv/README.md.
//
// kvRedisURLVar carries the RESP URL, which embeds the token as its password, so
// it exists in full only on the create and rotate responses - the same two
// moments the token itself does - and is written and printed under the same
// rules.
const (
	kvRestURLVar   = "CAPYKV_REST_URL"
	kvRestTokenVar = "CAPYKV_REST_TOKEN"
	kvRedisURLVar  = "CAPYKV_REDIS_URL"
)

func (a *app) newKVCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "kv",
		Short:   "Manage the project's K/V store (key-value and rate limiting)",
		Aliases: []string{"kvstore"},
	}

	command.AddCommand(a.newKVStatusCommand())
	command.AddCommand(a.newKVCreateCommand())
	command.AddCommand(a.newKVCredentialsCommand())
	command.AddCommand(a.newKVRotateTokenCommand())
	command.AddCommand(a.newKVFlushCommand())
	command.AddCommand(a.newKVDeleteCommand())
	return command
}

// resolveKVProject is the shared preamble: authenticate, then resolve the
// project the store belongs to.
func (a *app) resolveKVProject(cmd *cobra.Command, projectRef string) (*api.Client, api.Project, error) {
	client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
	if err != nil {
		return nil, api.Project{}, err
	}
	project, err := a.resolveProject(cmd.Context(), client, projectRef)
	if err != nil {
		return nil, api.Project{}, err
	}
	return client, project, nil
}

func (a *app) newKVStatusCommand() *cobra.Command {
	var projectRef string
	command := &cobra.Command{
		Use:   "status",
		Short: "Show the project's K/V store",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}
			store, err := client.GetKVStore(cmd.Context(), project.ID)
			if err != nil {
				if capydbclient.IsNotFound(err) {
					if a.jsonOutput() {
						return printJSON(cmd.OutOrStdout(), map[string]any{"kv_store": nil})
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Project %s has no K/V store. Create one with `capydb kv create`.\n", project.Name)
					return nil
				}
				return fmt.Errorf("fetch kv store: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"kv_store": store})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\n", store.ID)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "state: %s\n", store.State)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "capacity: %d MB\n", store.MaxMemoryMB)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "eviction: %s\n", store.MaxMemoryPolicy)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "persistence: %s\n", store.Persistence)
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func (a *app) newKVCreateCommand() *cobra.Command {
	var projectRef string
	var writeEnv bool
	var envFileOverride string
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "create",
		Short: "Provision the project's K/V store and print its token once",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}
			// Before the call, not after: the response is the only place the
			// plaintext exists, so a misconfigured --write-env must fail while
			// there is still nothing to lose.
			envTarget, err := a.resolveKVEnvTarget(writeEnv, envFileOverride)
			if err != nil {
				return err
			}

			store, job, err := client.CreateKVStore(cmd.Context(), project.ID)
			if err != nil {
				return fmt.Errorf("create kv store: %w", err)
			}

			if err := a.reportKVSecret(cmd, store, envTarget, "Created"); err != nil {
				return err
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "K/V store creation")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	addKVEnvFlags(command, &writeEnv, &envFileOverride)
	addWaitFlags(command, &wait, &waitTimeout, "K/V store creation")
	return command
}

func (a *app) newKVCredentialsCommand() *cobra.Command {
	var projectRef string
	command := &cobra.Command{
		Use:   "credentials",
		Short: "Show the K/V endpoints (the token is not recoverable)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}
			credentials, err := client.GetKVCredentials(cmd.Context(), project.ID)
			if err != nil {
				return fmt.Errorf("fetch kv credentials: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), credentials)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kvRestURLVar, credentials.RestURL)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "redis_url: %s\n", credentials.RedisURL)
			if credentials.TokenRequired {
				// Saying "the token is missing" would read as a bug. It is not
				// retrievable by design, and rotating is the only way to get one.
				_, _ = fmt.Fprintf(
					cmd.ErrOrStderr(),
					"\nThe token is not shown: only its hash is stored, so it cannot be read back.\n"+
						"The RESP URL above carries no password. Run `capydb kv rotate-token` to mint a new one.\n",
				)
			}
			return nil
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	return command
}

func (a *app) newKVRotateTokenCommand() *cobra.Command {
	var projectRef string
	var confirmFlag bool
	var writeEnv bool
	var envFileOverride string

	command := &cobra.Command{
		Use:   "rotate-token",
		Short: "Mint a new K/V token and stop the old one immediately",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}

			// Before the rotation: a failed env-file setup afterwards would
			// leave the old token revoked and the new one nowhere.
			envTarget, err := a.resolveKVEnvTarget(writeEnv, envFileOverride)
			if err != nil {
				return err
			}

			confirmed, err := confirmKVAction(
				cmd,
				project,
				confirmFlag,
				fmt.Sprintf(
					"This will ROTATE the K/V token for project %q (%s); every client using the current token stops working at once, with no grace period.",
					project.Name,
					project.ID,
				),
			)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("token rotation not confirmed; pass --confirm or confirm interactively")
			}

			// Synchronous on purpose: the response is the only place the new
			// plaintext exists, so there is no job to wait for.
			store, err := client.RotateKVToken(cmd.Context(), project.ID)
			if err != nil {
				return fmt.Errorf("rotate kv token: %w", err)
			}
			return a.reportKVSecret(cmd, store, envTarget, "Rotated the token for")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm the rotation without prompting")
	addKVEnvFlags(command, &writeEnv, &envFileOverride)
	return command
}

func (a *app) newKVFlushCommand() *cobra.Command {
	var projectRef string
	var confirmFlag bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "flush",
		Short: "Delete every key in the K/V store (the store itself survives)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}

			confirmed, err := confirmKVAction(
				cmd,
				project,
				confirmFlag,
				fmt.Sprintf(
					"This will DELETE EVERY KEY in the K/V store for project %q (%s). A K/V store has no backup, so there is nothing to restore from.",
					project.Name,
					project.ID,
				),
			)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("flush not confirmed; pass --confirm or confirm interactively")
			}

			job, err := client.FlushKVStore(cmd.Context(), project.ID)
			if err != nil {
				return fmt.Errorf("flush kv store: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued K/V flush job %s for project %s\n", job.ID, project.Name)
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "The endpoint and token are unchanged.")
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "K/V flush")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm the flush without prompting")
	addWaitFlags(command, &wait, &waitTimeout, "K/V flush")
	return command
}

func (a *app) newKVDeleteCommand() *cobra.Command {
	var projectRef string
	var confirmFlag bool
	var wait bool
	var waitTimeout time.Duration

	command := &cobra.Command{
		Use:   "delete",
		Short: "Delete the project's K/V store and its data",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.resolveKVProject(cmd, projectRef)
			if err != nil {
				return err
			}

			confirmed, err := confirmKVAction(
				cmd,
				project,
				confirmFlag,
				fmt.Sprintf(
					"This will DELETE the K/V store for project %q (%s) and everything in it. The database is untouched.",
					project.Name,
					project.ID,
				),
			)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("deletion not confirmed; pass --confirm or confirm interactively")
			}

			job, err := client.DeleteKVStore(cmd.Context(), project.ID)
			if err != nil {
				return fmt.Errorf("delete kv store: %w", err)
			}
			if !a.jsonOutput() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Queued K/V deletion job %s for project %s\n", job.ID, project.Name)
			}
			return a.maybeWaitForJob(cmd, client, job, wait, waitTimeout, "K/V store deletion")
		},
	}
	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().BoolVar(&confirmFlag, "confirm", false, "Confirm the deletion without prompting")
	addWaitFlags(command, &wait, &waitTimeout, "K/V store deletion")
	return command
}

func addKVEnvFlags(command *cobra.Command, writeEnv *bool, envFileOverride *string) {
	command.Flags().BoolVar(writeEnv, "write-env", false, "Write "+kvRestURLVar+", "+kvRestTokenVar+" and "+kvRedisURLVar+" into the linked project's env file")
	command.Flags().StringVar(envFileOverride, "env-file", "", "Env file to write when --write-env is set (default: the linked project's env file)")
}

// reportKVSecret surfaces the one-time credential pair from a create or rotate
// response, writing it to envTarget when --write-env resolved one.
//
// The token exists only in this response - only its SHA-256 hash is stored - so
// this is the single point where it can reach the operator. It is printed even
// in --output json, because a caller that asked for JSON parses stdout and the
// document would otherwise be missing the one field it exists to carry. Text
// output is read by a person, so when the token has already been put somewhere
// durable it is not also echoed into the terminal scrollback - unless the write
// failed, in which case the terminal is the only copy left and printing it
// takes priority over the error.
func (a *app) reportKVSecret(cmd *cobra.Command, store api.KVStore, envTarget, verb string) error {
	credentials := store.Credentials
	token := firstNonEmpty(store.Token, credentials.RestToken)

	written := false
	var writeErr error
	if envTarget != "" {
		writeErr = writeKVEnv(envTarget, credentials.RestURL, token, credentials.RedisURL)
		written = writeErr == nil
	}

	if a.jsonOutput() {
		if err := printJSON(cmd.OutOrStdout(), map[string]any{
			"kv_store":         store,
			kvRestURLVar:       credentials.RestURL,
			kvRestTokenVar:     token,
			"redis_url":        credentials.RedisURL,
			"token_shown_once": true,
		}); err != nil {
			return err
		}
		return writeErr
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s K/V store %s\n", verb, store.ID)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kvRestURLVar, credentials.RestURL)
	if written {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=<written to %s>\n", kvRestTokenVar, envTarget)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=<written to %s>\n", kvRedisURLVar, envTarget)
		_, _ = fmt.Fprintln(
			cmd.ErrOrStderr(),
			"\nThe token was written to the env file and is not printed here. Only its hash is stored, "+
				"so it cannot be shown again; if you lose it, rotate for a new one.",
		)
		return nil
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kvRestTokenVar, token)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", kvRedisURLVar, credentials.RedisURL)
	_, _ = fmt.Fprintln(
		cmd.ErrOrStderr(),
		"\nCopy the token now: only its hash is stored, so it cannot be shown again. If you lose it, rotate for a new one.",
	)
	if writeErr != nil {
		_, _ = fmt.Fprintf(
			cmd.ErrOrStderr(),
			"The env file was not written, so the values above are the only copy: %v\n",
			writeErr,
		)
	}
	return writeErr
}

// resolveKVEnvTarget resolves the file --write-env will write, or "" when the
// flag is not set. It is deliberately callable before any request: everything it
// needs is local, so a missing link or an unconfigured env file is caught while
// no store has been created and no token rotated.
func (a *app) resolveKVEnvTarget(writeEnv bool, envFileOverride string) (string, error) {
	if !writeEnv {
		return "", nil
	}

	linkConfig, err := config.LoadProjectConfig(a.cwd)
	envPath := strings.TrimSpace(envFileOverride)
	if err != nil {
		// Without a link there is no configured env file to fall back to, so an
		// explicit --env-file is the only way through.
		if envPath == "" {
			return "", fmt.Errorf("--write-env needs a linked project (run `capydb link`) or an explicit --env-file")
		}
		linkConfig = config.ProjectConfig{}
	}
	if envPath == "" {
		envPath = linkConfig.EnvFile
	}
	if envPath == "" {
		return "", fmt.Errorf("--write-env: no env file configured; pass --env-file")
	}
	return envTargetPath(a.cwd, linkConfig.AppPath, envPath), nil
}

// writeKVEnv merges the credentials into the resolved env file, through the
// same envfile.Upsert every other credential write uses, so unrelated keys and
// comments survive. The RESP URL rides along when the response carried one; it
// is the token in another shape, so it belongs in the file and not the terminal.
func writeKVEnv(target, restURL, token, redisURL string) error {
	if strings.TrimSpace(restURL) == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("--write-env: the response carried no credential pair to write")
	}
	values := map[string]string{
		kvRestURLVar:   restURL,
		kvRestTokenVar: token,
	}
	if strings.TrimSpace(redisURL) != "" {
		values[kvRedisURLVar] = redisURL
	}
	return envfile.Upsert(target, values)
}

// confirmKVAction guards the three irreversible K/V operations. Mirrors
// confirmCredentialRotation: --confirm for CI, retype-the-name interactively,
// and refuse rather than assume when stdin is not a terminal.
func confirmKVAction(cmd *cobra.Command, project api.Project, flagConfirmed bool, warning string) (bool, error) {
	if flagConfirmed {
		return true, nil
	}
	if !stdinIsInteractive() {
		return false, nil
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warning)
	answer, err := promptLine(fmt.Sprintf("Type the project name %q (or \"yes\") to confirm", project.Name))
	if err != nil {
		return false, err
	}
	trimmed := strings.TrimSpace(answer)
	if strings.EqualFold(trimmed, "yes") || trimmed == strings.TrimSpace(project.Name) {
		return true, nil
	}
	return false, nil
}
