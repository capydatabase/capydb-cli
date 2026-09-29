package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
)

var generateFixtureSchema = map[string]any{
	"database_name":    "demo",
	"postgres_version": "17",
	"extensions":       []any{},
	"schemas": []any{map[string]any{
		"name":  "public",
		"enums": []any{},
		"tables": []any{map[string]any{
			"name": "users", "kind": "table", "primary_key": []string{"id"},
			"foreign_keys": []any{}, "unique_constraints": []any{},
			"columns": []any{
				map[string]any{"name": "id", "udt_name": "uuid", "data_type": "uuid"},
				map[string]any{"name": "created_at", "udt_name": "timestamptz", "data_type": "timestamp with time zone", "is_nullable": true},
			},
		}},
	}},
}

func TestGenerateGoRendersLocallyFromTheSchemaEndpoint(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"schema": generateFixtureSchema})
		},
	})

	dir := t.TempDir()
	out := filepath.Join(dir, "internal", "db", "models.go")
	output, err := runCommand(t, dir, "generate", "go", "--api-url", server.URL, "--api-key", "capy_test", "--project", "prj_1", "--package", "models", "--out", out)
	if err != nil {
		t.Fatalf("generate go: %v\n%s", err, output)
	}
	content, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package models", "type UsersRow struct", "CreatedAt *time.Time"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("missing %q in\n%s", want, content)
		}
	}
}

func TestGeneratePythonPrintsPydantic(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"schema": generateFixtureSchema})
		},
	})
	output, err := runCommand(t, t.TempDir(), "generate", "python", "--api-url", server.URL, "--api-key", "capy_test", "--project", "prj_1", "--style", "pydantic", "--print")
	if err != nil {
		t.Fatalf("generate python: %v\n%s", err, output)
	}
	if !strings.Contains(output, "class UsersRow(BaseModel):") || !strings.Contains(output, "created_at: datetime.datetime | None") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestGenerateRejectsInvalidFlags(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	for _, args := range [][]string{
		{"generate", "go", "--package", "my-db"},
		{"generate", "python", "--style", "attrs"},
		{"generate", "types", "--watch", "--print"},
	} {
		if _, err := runCommand(t, t.TempDir(), append(args, "--api-key", "capy_test", "--api-url", "http://127.0.0.1:1")...); err == nil {
			t.Errorf("%v: expected a usage error", args)
		}
	}
}

func watchTestSchema(table string) api.DatabaseSchema {
	return api.DatabaseSchema{Schemas: []api.SchemaNamespace{{Name: "public", Tables: []api.SchemaTable{{Name: table, Kind: "table"}}}}}
}

func TestWatchSchemaRegeneratesOnChangeAndBacksOff(t *testing.T) {
	a, b := watchTestSchema("a"), watchTestSchema("b")
	steps := []func() (schemaPoll, error){
		func() (schemaPoll, error) { return schemaPoll{schema: &a}, nil },
		func() (schemaPoll, error) { return schemaPoll{schema: &a}, nil },
		func() (schemaPoll, error) { return schemaPoll{skipped: true}, nil },
		func() (schemaPoll, error) { return schemaPoll{}, errors.New("network down") },
		func() (schemaPoll, error) { return schemaPoll{schema: &b}, nil },
		func() (schemaPoll, error) { return schemaPoll{schema: &b}, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := 0
	poll := func(context.Context) (schemaPoll, error) {
		step := steps[call]
		call++
		return step()
	}
	var generated []string
	onChange := func(_ context.Context, schema api.DatabaseSchema) error {
		generated = append(generated, schema.Schemas[0].Tables[0].Name)
		return nil
	}
	var sleeps []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if call == len(steps) {
			cancel()
			return context.Canceled
		}
		return nil
	}

	progress := new(bytes.Buffer)
	if err := watchSchema(ctx, time.Second, poll, onChange, sleep, progress); err != nil {
		t.Fatal(err)
	}
	if strings.Join(generated, ",") != "a,b" {
		t.Fatalf("generated = %v, want a then b", generated)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, time.Second, 2 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", sleeps, want)
		}
	}
	if !strings.Contains(progress.String(), "paused") || !strings.Contains(progress.String(), "network down") {
		t.Fatalf("progress did not report the pause and the error:\n%s", progress.String())
	}
}

func TestWatchSchemaCapsBackoffAndStopsOnAuthFailure(t *testing.T) {
	var sleeps []time.Duration
	polls := 0
	poll := func(context.Context) (schemaPoll, error) {
		polls++
		return schemaPoll{}, &api.APIError{StatusCode: http.StatusUnauthorized, Message: "invalid api key"}
	}
	sleep := func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	err := watchSchema(context.Background(), 40*time.Second, poll, nil, sleep, new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "watch stopped") {
		t.Fatalf("err = %v, want the watch to stop on repeated auth failures", err)
	}
	if polls != maxWatchAuthFailures {
		t.Fatalf("polls = %d, want %d", polls, maxWatchAuthFailures)
	}
	for _, d := range sleeps {
		if d > maxWatchInterval {
			t.Fatalf("slept %s, above the %s cap", d, maxWatchInterval)
		}
	}
}

func TestWatchSchemaRetriesAFailedRegeneration(t *testing.T) {
	a := watchTestSchema("a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	onChange := func(context.Context, api.DatabaseSchema) error {
		attempts++
		if attempts == 1 {
			return errors.New("disk full")
		}
		cancel()
		return nil
	}
	poll := func(context.Context) (schemaPoll, error) { return schemaPoll{schema: &a}, nil }
	sleep := func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := watchSchema(ctx, time.Second, poll, onChange, sleep, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the unchanged schema to be regenerated again after a failure", attempts)
	}
}
