package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

const (
	defaultWatchInterval = 5 * time.Second
	maxWatchInterval     = time.Minute
	// maxWatchAuthFailures ends the watch when the key stops being accepted;
	// an expired or revoked key never recovers by waiting.
	maxWatchAuthFailures = 3
)

type schemaWatchOptions struct {
	interval time.Duration
}

// schemaPoll is one look at the schema. skipped means the database was not
// read (a paused cell): its schema cannot change until something wakes it,
// and reading it would wake it.
type schemaPoll struct {
	schema  *api.DatabaseSchema
	skipped bool
}

// watchSchema regenerates on every schema change until ctx ends.
//
// Reading the schema opens a connection to the cell, so it wakes a paused
// cell and counts as activity on an awake one. The loop therefore never reads
// a paused cell (poll reports skipped), and backs off while nothing changes:
// the interval doubles per unchanged or skipped look up to maxWatchInterval
// and drops back to the base interval right after a change. Errors back off
// the same way; only authentication failures end the watch.
func watchSchema(
	ctx context.Context,
	base time.Duration,
	poll func(context.Context) (schemaPoll, error),
	onChange func(context.Context, api.DatabaseSchema) error,
	sleep func(context.Context, time.Duration) error,
	progress io.Writer,
) error {
	if base <= 0 {
		base = defaultWatchInterval
	}
	interval := base
	lastHash := ""
	authFailures := 0
	pausedNoted := false

	for {
		result, err := poll(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			if apiErr, ok := errors.AsType[*api.APIError](err); ok && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
				authFailures++
				if authFailures >= maxWatchAuthFailures {
					return fmt.Errorf("watch stopped: %w", err)
				}
			}
			_, _ = fmt.Fprintf(progress, "watch: could not read the schema (retrying): %v\n", err)
			interval = backoff(interval)
		case result.skipped:
			authFailures = 0
			if !pausedNoted {
				_, _ = fmt.Fprintln(progress, "watch: the database is paused; its schema cannot change until it resumes, so it is not polled until then")
				pausedNoted = true
			}
			interval = backoff(interval)
		default:
			authFailures = 0
			pausedNoted = false
			hash, hashErr := schemaHash(*result.schema)
			if hashErr != nil {
				return hashErr
			}
			if hash == lastHash {
				interval = backoff(interval)
				break
			}
			if err := onChange(ctx, *result.schema); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Keep lastHash unchanged so the next look retries.
				_, _ = fmt.Fprintf(progress, "watch: regenerate failed (retrying): %v\n", err)
				interval = backoff(interval)
				break
			}
			lastHash = hash
			interval = base
		}

		if err := sleep(ctx, interval); err != nil {
			return nil
		}
	}
}

func backoff(interval time.Duration) time.Duration {
	interval *= 2
	if interval > maxWatchInterval {
		return maxWatchInterval
	}
	return interval
}

// schemaHash fingerprints the schema document. The document is deterministic
// (the API orders every list), so equal schemas hash equal.
func schemaHash(schema api.DatabaseSchema) (string, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("encode schema: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *app) runGenerateWatch(cmd *cobra.Command, client *api.Client, target schemaTarget, spec generatorSpec, options generateOptions) error {
	ctx := cmd.Context()
	progress := cmd.ErrOrStderr()

	poll := func(ctx context.Context) (schemaPoll, error) {
		if target.previewID == "" {
			// A control-plane read: it never touches the cell.
			project, _, err := client.GetProject(ctx, target.project.ID)
			if err != nil {
				return schemaPoll{}, err
			}
			if project.RuntimeStatus == "paused" {
				return schemaPoll{skipped: true}, nil
			}
		}
		schema, err := fetchTargetSchema(ctx, client, target)
		if err != nil {
			return schemaPoll{}, err
		}
		return schemaPoll{schema: &schema}, nil
	}

	// The schema document only detects a change; the file is rendered by the
	// control plane, like every other generate run.
	onChange := func(ctx context.Context, _ api.DatabaseSchema) error {
		types, err := renderGenerated(ctx, client, target, spec, options)
		if err != nil {
			return err
		}
		path, err := writeGenerated(types, options.outPath)
		if err != nil {
			return err
		}
		if a.jsonOutput() {
			// One compact JSON object per line (NDJSON): a watch emits an
			// event per regeneration instead of a single document.
			encoded, err := json.Marshal(map[string]any{
				"event":    "generated",
				"language": types.Language,
				"path":     path,
				"time":     time.Now().UTC().Format(time.RFC3339),
			})
			if err != nil {
				return fmt.Errorf("encode event: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		}
		_, _ = fmt.Fprintf(progress, "%s wrote %s\n", time.Now().Format("15:04:05"), path)
		return nil
	}

	_, _ = fmt.Fprintln(progress, "Watching the schema for changes (Ctrl-C to stop).")
	return watchSchema(ctx, options.watchOpts.interval, poll, onChange, sleepContext, progress)
}
