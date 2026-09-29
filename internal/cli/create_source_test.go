package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreatePostgresVersion(t *testing.T) {
	cases := []struct {
		name        string
		requested   string
		sourceMajor int
		want        string
		wantNote    string
		wantErr     string
	}{
		{name: "no source keeps the request", requested: "18", want: "18"},
		{name: "no source and no request is the server default", want: ""},
		{name: "matches the source", sourceMajor: 17, want: "17", wantNote: "to match the source"},
		{name: "raises an old source to the minimum", sourceMajor: 14, want: "16", wantNote: "oldest major CapyDB offers is 16"},
		{name: "refuses a source newer than every offered major", sourceMajor: 19, wantErr: "newer than the newest major CapyDB offers (18)"},
		{name: "explicit older than source warns", requested: "16", sourceMajor: 17, want: "16", wantNote: "warning: --postgres-version 16 is older than the source"},
		{name: "explicit newer than source notes the upgrade", requested: "18", sourceMajor: 16, want: "18", wantNote: "newer than the source"},
		{name: "explicit equal to source is silent", requested: "17", sourceMajor: 17, want: "17"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, notes, err := createPostgresVersion(tc.requested, tc.sourceMajor)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
			joined := strings.Join(notes, "\n")
			if tc.wantNote == "" && joined != "" {
				t.Fatalf("unexpected notes: %q", joined)
			}
			if !strings.Contains(joined, tc.wantNote) {
				t.Fatalf("notes = %q, want %q", joined, tc.wantNote)
			}
		})
	}
}

// createServer answers everything `capydb create` calls and records the
// create-project request body (nil when POST /v1/projects never happened).
func createServer(t *testing.T, body *map[string]any) *httptest.Server {
	t.Helper()
	project := map[string]any{"id": "prj_new", "organization_id": "org_1", "name": "app", "slug": "app", "state": "ready"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/regions":
			writeJSON(t, w, map[string]any{"regions": []string{"hel1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			*body = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			writeJSON(t, w, map[string]any{"job": map[string]any{}, "project": project})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_new":
			writeJSON(t, w, map[string]any{"project": project})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_new/kv":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_new/connections":
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"direct_url": "postgresql://u:p@app.db.capydb.dev:5432/app",
				"pooled_url": "postgresql://u:p@app.db.capydb.dev:6432/app",
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
}

func TestCreateWithSourceURLRequestsSourceMajor(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var probed string
	previous := sourceServerMajor
	sourceServerMajor = func(_ context.Context, url string) (int, error) { probed = url; return 15, nil }
	t.Cleanup(func() { sourceServerMajor = previous })

	var body map[string]any
	server := createServer(t, &body)
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "create", "--name", "app", "--non-interactive",
		"--source-url", "postgres://u:p@old.example.com/app",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, output)
	}
	if probed != "postgres://u:p@old.example.com/app" {
		t.Fatalf("source probe got %q", probed)
	}
	if body["postgres_version"] != "16" {
		t.Fatalf("postgres_version = %v, want 16 (source 15 raised to the minimum)", body["postgres_version"])
	}
	if !strings.Contains(output, "oldest major CapyDB offers is 16") {
		t.Fatalf("output missing the version note:\n%s", output)
	}
}

func TestCreateWithUnreachableSourceFailsBeforeCreating(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	previous := sourceServerMajor
	sourceServerMajor = func(context.Context, string) (int, error) { return 0, errors.New("connection refused") }
	t.Cleanup(func() { sourceServerMajor = previous })

	var body map[string]any
	server := createServer(t, &body)
	defer server.Close()

	_, err := runCommand(t, t.TempDir(), "create", "--name", "app", "--non-interactive",
		"--source-url", "postgres://u:p@old.example.com/app",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err == nil || !strings.Contains(err.Error(), "--source-url: connection refused") {
		t.Fatalf("expected the probe error, got %v", err)
	}
	if body != nil {
		t.Fatal("the project was created although the source could not be read")
	}
}

func TestCreateHintsAtForeignDatabaseURLInEnvFile(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATABASE_URL=postgres://u:p@ep-cool-1.eu-central-1.aws.neon.tech/neondb\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	server := createServer(t, &body)
	defer server.Close()

	output, err := runCommand(t, dir, "create", "--name", "app", "--non-interactive", "--env-file", ".env", "--overwrite-env",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, output)
	}
	if !strings.Contains(output, "note: .env sets DATABASE_URL to a database outside CapyDB") {
		t.Fatalf("output missing the env hint:\n%s", output)
	}
	if _, set := body["postgres_version"]; set {
		t.Fatalf("the hint must not pick a version on its own: %v", body)
	}
}

func TestCreateJSONSummaryListsDirectURL(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var body map[string]any
	server := createServer(t, &body)
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "create", "--name", "app", "--non-interactive", "-o", "json",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("create -o json: %v\n%s", err, output)
	}
	start := strings.LastIndex(output, "\n{")
	var summary struct {
		EnvVars []string `json:"env_vars"`
	}
	if err := json.Unmarshal([]byte(output[start+1:]), &summary); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, output)
	}
	if !strings.Contains(strings.Join(summary.EnvVars, ","), "DIRECT_URL") || !strings.Contains(strings.Join(summary.EnvVars, ","), "DATABASE_DIRECT_URL") {
		t.Fatalf("env_vars = %v, want DIRECT_URL and DATABASE_DIRECT_URL", summary.EnvVars)
	}
}
