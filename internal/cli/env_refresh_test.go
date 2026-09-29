package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/config"
)

// env pull refreshes CapyDB's own values silently but does not replace a
// value that points at another provider without saying so.
func TestEnvPullWritesDirectURLAndFlagsForeignValues(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_env":
			writeJSON(t, w, map[string]any{"project": map[string]any{"id": "prj_env", "organization_id": "org_1", "name": "app", "slug": "app"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_env/kv":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/prj_env/connections":
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"direct_url": "postgresql://u:rotated@app.db.capydb.dev:5432/app",
				"pooled_url": "postgresql://u:rotated@app.db.capydb.dev:6432/app",
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	existing := strings.Join([]string{
		`DATABASE_URL="postgresql://u:old@app.db.capydb.dev:6432/app"`,
		`DIRECT_URL="postgresql://postgres:pw@db.abcdefghijklmnop.supabase.co:5432/postgres"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveProjectConfig(dir, config.ProjectConfig{
		APIURL: server.URL, EnvFile: ".env", Framework: "nextjs", DatabaseLayer: "drizzle", ProjectID: "prj_env",
	}); err != nil {
		t.Fatal(err)
	}

	output, err := runCommand(t, dir, "env", "pull", "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("env pull: %v\n%s", err, output)
	}
	if strings.Contains(output, "overwriting existing DATABASE_URL") {
		t.Errorf("a refresh of the same CapyDB host must be silent:\n%s", output)
	}
	if !strings.Contains(output, "warning: overwriting existing DIRECT_URL") {
		t.Errorf("replacing another provider's DIRECT_URL must be announced:\n%s", output)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`DATABASE_URL="postgresql://u:rotated@app.db.capydb.dev:6432/app"`,
		`DIRECT_URL="postgresql://u:rotated@app.db.capydb.dev:5432/app"`,
		`DATABASE_DIRECT_URL="postgresql://u:rotated@app.db.capydb.dev:5432/app"`,
		`DATABASE_POOL_URL="postgresql://u:rotated@app.db.capydb.dev:6432/app"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("env file missing %s:\n%s", want, data)
		}
	}
}
