package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// postgresVersionFlagHelp is the --postgres-version description shared by
// every command that creates a database. The control plane validates the
// value; 19 is accepted only while CapyDB offers it as a beta.
const postgresVersionFlagHelp = "Postgres major version: 16, 17, 18, or 19 while it is offered as a beta (see `capydb postgres-versions`)"

func (a *app) newPostgresVersionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "postgres-versions",
		Short: "List the Postgres versions new databases can be created on",
		Long: "Lists the Postgres majors a new database can be created on, oldest first, with the " +
			"release channel each is offered on:\n\n" +
			"  previous  an older generally available major\n" +
			"  stable    the default for new databases\n" +
			"  current   the newest generally available major\n" +
			"  beta      an upstream beta, offered for evaluation only: no uptime or durability\n" +
			"            commitment, extensions may be missing, and the database may need\n" +
			"            recreating instead of upgrading when the major is released\n\n" +
			"Pass the version to --postgres-version on `capydb create` or `capydb ephemeral create`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := a.resolveClient(true, a.linkedProjectAPIURL())
			if err != nil {
				return err
			}
			versions, err := client.ListPostgresVersions(cmd.Context())
			if err != nil {
				return fmt.Errorf("list postgres versions: %w", err)
			}
			if a.jsonOutput() {
				return printJSON(cmd.OutOrStdout(), map[string]any{"versions": versions})
			}
			writePostgresVersionTable(cmd.OutOrStdout(), versions)
			return nil
		},
	}
}

func writePostgresVersionTable(out io.Writer, versions []api.PostgresVersion) {
	writer := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "VERSION\tCHANNEL\tDEFAULT\tPRODUCTION READY")
	for _, version := range versions {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", version.Version, version.Channel, yesNo(version.Default), yesNo(version.ProductionReady))
	}
	_ = writer.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// writePostgresWarning prints a beta database's warning; the control plane
// asks that it be shown wherever the database is shown.
func writePostgresWarning(out io.Writer, warning string) {
	if warning = strings.TrimSpace(warning); warning != "" {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", warning)
	}
}

// postgresLabel renders a database's major with its release channel, e.g.
// "18 (current)"; the channel is omitted when the control plane did not send
// one.
func postgresLabel(version, channel string) string {
	version = firstNonEmpty(version, "-")
	if channel = strings.TrimSpace(channel); channel != "" {
		return version + " (" + channel + ")"
	}
	return version
}
