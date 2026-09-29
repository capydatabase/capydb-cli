package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/jackc/pgx/v5"
)

// execSQLScript runs a multi-statement script CapyDB itself ships (templates)
// over one connection. The simple query protocol runs every statement of the
// string in one implicit transaction, so a failing statement leaves nothing
// behind. It does not understand psql meta-commands or COPY FROM stdin; user
// files go through runPsqlFile instead.
func execSQLScript(ctx context.Context, connectionURL, script string) error {
	conn, err := pgx.Connect(ctx, connectionURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.PgConn().Exec(ctx, script).ReadAll(); err != nil {
		return err
	}
	return nil
}

// psqlPath finds psql or explains how to install it.
func psqlPath() (string, error) {
	path, err := exec.LookPath("psql")
	if err != nil {
		return "", fmt.Errorf("psql not found in PATH; install the Postgres client tools (e.g. `brew install libpq` or `apt install postgresql-client`)")
	}
	return path, nil
}

// runPsqlFile applies a user's SQL file with psql: every psql feature a dump
// or seed file may use works (COPY ... FROM stdin, meta-commands), the first
// error stops the run, and --single-transaction makes the file all or nothing.
func runPsqlFile(ctx context.Context, stdout, stderr io.Writer, connectionURL, file string) error {
	path, err := psqlPath()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, path,
		psqlConnectionURL(connectionURL),
		"--no-psqlrc", "--quiet", "--single-transaction",
		"--set", "ON_ERROR_STOP=1",
		"--file", file,
	)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("psql %s: %w", file, err)
	}
	return nil
}
