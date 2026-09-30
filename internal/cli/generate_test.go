package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
)

// typesEndpoint serves GET .../schema/types, recording the query it was asked
// with, and answers with content that names the requested language.
func typesEndpoint(t *testing.T, query *url.Values) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*query = r.URL.Query()
		language := query.Get("language")
		if language == "go" && query.Get("package") == "my-db" {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, map[string]any{"error": `package "my-db" is not a valid Go package name`})
			return
		}
		filename := map[string]string{"go": "capydb_types.go", "python": "capydb_types.py"}[language]
		writeJSON(t, w, map[string]any{"types": map[string]any{
			"content": "// generated " + language + " " + query.Get("style") + query.Get("package") + "\n", "filename": filename,
			"language": language, "style": query.Get("style"),
		}})
	}
}

func TestGenerateGoAsksTheControlPlane(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var query url.Values
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema/types": typesEndpoint(t, &query),
	})

	dir := t.TempDir()
	out := filepath.Join(dir, "internal", "db", "models.go")
	output, err := runCommand(t, dir, "generate", "go", "--api-url", server.URL, "--api-key", "capy_test", "--project", "prj_1", "--package", "models", "--out", out)
	if err != nil {
		t.Fatalf("generate go: %v\n%s", err, output)
	}
	if query.Get("language") != "go" || query.Get("package") != "models" || query.Get("style") != "" {
		t.Fatalf("query = %v", query)
	}
	content, err := os.ReadFile(out)
	if err != nil || string(content) != "// generated go models\n" {
		t.Fatalf("file = %q (%v)", content, err)
	}
}

func TestGeneratePythonSendsTheStyle(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var query url.Values
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/preview-databases/pdb_1/schema/types": typesEndpoint(t, &query),
	})
	output, err := runCommand(t, t.TempDir(), "generate", "python", "--api-url", server.URL, "--api-key", "capy_test", "--preview", "pdb_1", "--style", "pydantic", "--print")
	if err != nil {
		t.Fatalf("generate python: %v\n%s", err, output)
	}
	if query.Get("language") != "python" || query.Get("style") != "pydantic" || query.Get("package") != "" {
		t.Fatalf("query = %v", query)
	}
	if output != "// generated python pydantic\n" {
		t.Fatalf("unexpected output: %q", output)
	}

	// The default style is dataclass.
	if _, err := runCommand(t, t.TempDir(), "generate", "python", "--api-url", server.URL, "--api-key", "capy_test", "--preview", "pdb_1", "--print"); err != nil || query.Get("style") != "dataclass" {
		t.Fatalf("default style = %q (%v)", query.Get("style"), err)
	}
}

func TestGenerateSurfacesServerValidation(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var query url.Values
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/schema/types": typesEndpoint(t, &query),
	})
	_, err := runCommand(t, t.TempDir(), "generate", "go", "--package", "my-db", "--print", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "not a valid Go package name") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerateRejectsWatchWithPrint(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	if _, err := runCommand(t, t.TempDir(), "generate", "types", "--watch", "--print", "--api-key", "capy_test", "--api-url", "http://127.0.0.1:1"); err == nil {
		t.Error("expected a usage error for --watch with --print")
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
