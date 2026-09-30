package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// logsFollowInterval paces the --follow poll loop. Each poll is one control-
// plane request that reads the tail from the database's host, so it mirrors
// the dashboard tail's cadence rather than hammering the API.
const logsFollowInterval = 4 * time.Second

func (a *app) newLogsCommand() *cobra.Command {
	var projectRef string
	var hours int
	var limit int
	var severity string
	var follow bool
	var search logSearchFlags

	command := &cobra.Command{
		Use:   "logs",
		Short: "Show or search a project's database logs",
		Long: `Show the project's Postgres log stream: a trailing window by default, or a live tail with
--follow. Severity is parsed from the log format; filter with --severity (e.g. --severity
error,fatal). Lines that report a condition carry its SQLSTATE, shown in brackets.

--search, --sqlstate, --since and --until search the archived logs of the last 30 days instead,
newest first:

  capydb logs --sqlstate 42P01                 undefined_table errors (last 24 hours)
  capydb logs --sqlstate 23 --since 7d         every integrity constraint violation this week
  capydb logs --search "deadlock" --severity error --since 2026-09-29T00:00:00Z

--sqlstate takes five-character codes or two-character classes, comma-separated; --search is a
case-insensitive substring. --since and --until take RFC 3339 times or a duration back from now
(90m, 12h, 7d); the window defaults to the last 24 hours and spans at most 30 days. The newest few
minutes are not searchable yet. A page holds up to --limit entries; pass the printed --cursor to
continue. Log search is available where CapyDB has enabled it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if hours < 1 || hours > 720 {
				return usageErrorf("--hours must be between 1 and 720")
			}
			if limit < 0 || limit > 500 {
				return usageErrorf("--limit must be between 0 and 500")
			}
			severities := splitSeverities(severity)
			searching := search.active(cmd)
			if searching && follow {
				return usageErrorf("--follow tails the live stream; it cannot be combined with --search, --sqlstate, --since, --until, or --cursor")
			}
			var searchQuery api.ProjectLogSearchQuery
			if searching {
				var err error
				if searchQuery, err = search.query(cmd, time.Now(), hours, limit, severities); err != nil {
					return err
				}
			}

			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			project, err := a.resolveProject(ctx, client, projectRef)
			if err != nil {
				return err
			}
			if searching {
				return a.searchProjectLogs(cmd, client, project.ID, searchQuery)
			}

			logs, err := client.GetProjectLogs(ctx, project.ID, api.ProjectLogsQuery{
				Hours:      hours,
				Limit:      limit,
				Severities: severities,
			})
			if err != nil {
				return fmt.Errorf("get logs: %w", err)
			}

			if a.jsonOutput() && !follow {
				return printJSON(cmd.OutOrStdout(), map[string]any{"logs": map[string]any{
					"entries":     jsonList(logs.Entries),
					"next_cursor": logs.NextCursor,
				}})
			}

			out := cmd.OutOrStdout()
			writeLogEntries(out, logs.Entries, a.jsonOutput())
			if !follow {
				if len(logs.Entries) == 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No log entries in the last %dh\n", hours)
				}
				return nil
			}

			return a.followProjectLogs(ctx, cmd.ErrOrStderr(), out, client, project.ID, logs.NextCursor, severities)
		},
	}

	command.Flags().StringVar(&projectRef, "project", "", "Project id, slug, or name")
	command.Flags().IntVar(&hours, "hours", 1, "Trailing window in hours (1-720; beyond 168 needs the platform log archive)")
	command.Flags().IntVar(&limit, "limit", 0, "Maximum entries per fetch (server default when omitted, max 500)")
	command.Flags().StringVar(&severity, "severity", "", "Comma-separated severity filter (debug, log, info, notice, warning, error, fatal, panic, detail)")
	command.Flags().BoolVarP(&follow, "follow", "f", false, "Keep the stream open and print new entries as they arrive")
	command.Flags().StringVar(&search.text, "search", "", "Search the archived logs for messages containing this text (case-insensitive)")
	command.Flags().StringVar(&search.sqlstate, "sqlstate", "", "Search for these SQLSTATE codes or classes, comma-separated (e.g. 42P01 or 23)")
	command.Flags().StringVar(&search.since, "since", "", "Search window start: RFC 3339 time or a duration back from now (e.g. 12h, 7d)")
	command.Flags().StringVar(&search.until, "until", "", "Search window end: RFC 3339 time or a duration back from now (default now)")
	command.Flags().StringVar(&search.cursor, "cursor", "", "Continue a search from the cursor the previous page printed")
	return command
}

// logSearchFlags are the flags that turn `capydb logs` into a search of the
// archived logs.
type logSearchFlags struct {
	text, sqlstate, since, until, cursor string
}

func (f logSearchFlags) active(cmd *cobra.Command) bool {
	for _, name := range []string{"search", "sqlstate", "since", "until", "cursor"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

// query builds the search request. An explicit --hours without --since sets
// the window start, so `--hours 6 --sqlstate 40P01` means what it reads as.
func (f logSearchFlags) query(cmd *cobra.Command, now time.Time, hours, limit int, severities []string) (api.ProjectLogSearchQuery, error) {
	query := api.ProjectLogSearchQuery{
		Cursor:     strings.TrimSpace(f.cursor),
		Limit:      limit,
		Query:      strings.TrimSpace(f.text),
		SQLStates:  splitSeverities(f.sqlstate),
		Severities: severities,
	}
	if len([]rune(query.Query)) > 200 {
		return api.ProjectLogSearchQuery{}, usageErrorf("--search takes at most 200 characters")
	}
	for i, code := range query.SQLStates {
		code = strings.ToUpper(code)
		if len(code) != 2 && len(code) != 5 {
			return api.ProjectLogSearchQuery{}, usageErrorf("--sqlstate %q is neither a five-character code nor a two-character class", code)
		}
		query.SQLStates[i] = code
	}
	var err error
	if query.Since, err = parseLogTime("--since", f.since, now); err != nil {
		return api.ProjectLogSearchQuery{}, err
	}
	if query.Until, err = parseLogTime("--until", f.until, now); err != nil {
		return api.ProjectLogSearchQuery{}, err
	}
	if query.Since.IsZero() && cmd.Flags().Changed("hours") {
		query.Since = now.Add(-time.Duration(hours) * time.Hour)
	}
	return query, nil
}

// parseLogTime reads an RFC 3339 time or a duration back from now ("90m",
// "12h", "7d"). Empty yields the zero time (the server default).
func parseLogTime(flag, value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	var duration time.Duration
	var err error
	if days, ok := strings.CutSuffix(value, "d"); ok {
		var n int
		n, err = strconv.Atoi(days)
		duration = time.Duration(n) * 24 * time.Hour
	} else {
		duration, err = time.ParseDuration(value)
	}
	if err != nil || duration <= 0 {
		return time.Time{}, usageErrorf("%s %q is neither an RFC 3339 time nor a duration such as 12h or 7d", flag, value)
	}
	return now.Add(-duration), nil
}

func (a *app) searchProjectLogs(cmd *cobra.Command, client *api.Client, projectID string, query api.ProjectLogSearchQuery) error {
	result, err := client.SearchProjectLogs(cmd.Context(), projectID, query)
	if err != nil {
		if apiErr, ok := errors.AsType[*api.APIError](err); ok && apiErr.StatusCode == http.StatusServiceUnavailable {
			return fmt.Errorf("log search is not enabled on this deployment; `capydb logs` without search flags still shows the recent stream (--hours, --severity, --follow): %w", err)
		}
		return fmt.Errorf("search logs: %w", err)
	}
	if a.jsonOutput() {
		return printJSON(cmd.OutOrStdout(), map[string]any{"search": result})
	}
	writeLogEntries(cmd.OutOrStdout(), result.Entries, false)
	errOut := cmd.ErrOrStderr()
	if len(result.Entries) == 0 {
		_, _ = fmt.Fprintln(errOut, "No matching log entries.")
	}
	if result.NextCursor != "" {
		_, _ = fmt.Fprintf(errOut, "More results: re-run with --cursor %s\n", result.NextCursor)
	}
	return nil
}

// followProjectLogs tails the log stream by polling the cursor-resume fetch
// until the context is cancelled (Ctrl-C). Transient fetch errors are
// reported to stderr and retried - a brief control-plane or host blip must
// not kill a long-running tail.
func (a *app) followProjectLogs(ctx context.Context, errOut, out io.Writer, client *api.Client, projectID, cursor string, severities []string) error {
	if !a.jsonOutput() {
		_, _ = fmt.Fprintln(errOut, "Following logs... (Ctrl-C to stop)")
	}
	ticker := time.NewTicker(logsFollowInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		logs, err := client.GetProjectLogs(ctx, projectID, api.ProjectLogsQuery{
			Cursor:     cursor,
			Severities: severities,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Auth failures and a deleted project are permanent: retrying
			// forever would loop silently on a revoked key. Fail fast with the
			// semantic exit code; only genuinely transient errors retry.
			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				switch apiErr.StatusCode {
				case 401, 403:
					return authErrorf("log tail stopped: %v", err)
				case 404:
					return notFoundErrorf("log tail stopped: project no longer exists: %v", err)
				}
			}
			_, _ = fmt.Fprintf(errOut, "log fetch failed (retrying): %v\n", err)
			continue
		}
		if logs.NextCursor != "" {
			cursor = logs.NextCursor
		}
		writeLogEntries(out, logs.Entries, a.jsonOutput())
	}
}

// writeLogEntries prints entries either as human-readable lines (timestamp,
// upper-cased severity, message) or as one JSON document per entry (JSONL,
// the scriptable shape for --follow pipelines).
func writeLogEntries(out io.Writer, entries []api.ProjectLogEntry, asJSON bool) {
	for _, entry := range entries {
		if asJSON {
			_ = printJSON(out, entry)
			continue
		}
		severity := strings.ToUpper(entry.Severity)
		if entry.Severity == "detail" {
			severity = "  ..."
		}
		message := entry.Message
		if entry.SQLState != "" {
			message = "[" + entry.SQLState + "] " + message
		}
		_, _ = fmt.Fprintf(out, "%s %-7s %s\n", entry.Timestamp.Local().Format("2006-01-02 15:04:05"), severity, message)
	}
}

func splitSeverities(raw string) []string {
	var severities []string
	for severity := range strings.SplitSeq(raw, ",") {
		severity = strings.TrimSpace(strings.ToLower(severity))
		if severity != "" {
			severities = append(severities, severity)
		}
	}
	return severities
}
