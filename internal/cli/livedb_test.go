package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// liveDatabaseURL returns a Postgres superuser URL for tests that need a real
// database, or skips. Each caller gets a fresh database dropped at cleanup.
//
//	docker run -d -e POSTGRES_PASSWORD=pw -p 127.0.0.1:55517:5432 postgres:17
//	CAPYDB_CLI_TEST_DATABASE_URL=postgres://postgres:pw@127.0.0.1:55517/postgres go test ./internal/cli/
func liveDatabaseURL(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("CAPYDB_CLI_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("CAPYDB_CLI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	name := fmt.Sprintf("capydb_cli_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// liveExec runs a multi-statement script against url.
func liveExec(t *testing.T, url, script string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.PgConn().Exec(ctx, script).ReadAll(); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// liveQueryRows runs query and returns the rows the way the SQL endpoint's
// JSON decodes them (column name -> value).
func liveQueryRows(t *testing.T, url, query string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	collected, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return collected
}
