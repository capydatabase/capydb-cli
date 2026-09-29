package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/configlint"
)

func doctorRoutes(t *testing.T, projectMissing bool) map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{
		"GET /status": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"status": "operational", "components": []any{}})
		},
		"GET /v1/me": func(w http.ResponseWriter, r *http.Request) { writeViewer(t, w, "org_1") },
		"GET /v1/projects/prj_1/connections": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"direct_url": "postgres://u:p@demo.db.capydb.dev:5432/demo",
				"pooled_url": "postgres://u:p@demo.db.capydb.dev:6432/demo",
			}})
		},
	}
	if projectMissing {
		routes["GET /v1/projects/prj_1"] = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "project not found"})
		}
	}
	return routes
}

func linkTestProject(t *testing.T, dir string) {
	t.Helper()
	if err := config.SaveProjectConfig(dir, config.ProjectConfig{
		ProjectID: "prj_1", ProjectName: "demo", EnvFile: ".env.local",
		DatabaseURLVar: "DATABASE_URL", DirectURLVar: "DATABASE_DIRECT_URL",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorFixAddsMissingEnvVarsWithoutOverwriting(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, doctorRoutes(t, false))
	dir := t.TempDir()
	linkTestProject(t, dir)
	writeTestFile(t, filepath.Join(dir, ".env.local"), "DATABASE_URL=\"postgres://u:p@demo.db.capydb.dev:6432/custom\"\n")

	output, _ := runCommand(t, dir, "doctor", "--fix", "--api-url", server.URL, "--api-key", "capy_test")
	env := readTestFile(t, filepath.Join(dir, ".env.local"))
	if !strings.Contains(env, `DATABASE_DIRECT_URL="postgres://u:p@demo.db.capydb.dev:5432/demo"`) {
		t.Fatalf("missing var not added:\n%s\n%s", env, output)
	}
	if !strings.Contains(env, "/custom") {
		t.Fatalf("an existing value was overwritten:\n%s", env)
	}
	if !strings.Contains(output, "[fixed] env_vars: added DATABASE_DIRECT_URL to .env.local") || !strings.Contains(output, "[pass] env_vars") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestDoctorFixDestructiveFixesNeedYes(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, doctorRoutes(t, true))
	dir := t.TempDir()
	linkTestProject(t, dir)

	output, _ := runCommand(t, dir, "doctor", "--fix", "--api-url", server.URL, "--api-key", "capy_test")
	if !fileExists(config.ProjectConfigPath(dir)) || !strings.Contains(output, "[skipped] project_link") {
		t.Fatalf("a destructive fix ran without --yes:\n%s", output)
	}
	output, _ = runCommand(t, dir, "doctor", "--fix", "--yes", "--api-url", server.URL, "--api-key", "capy_test")
	if fileExists(config.ProjectConfigPath(dir)) || !strings.Contains(output, "[fixed] project_link") {
		t.Fatalf("stale link not removed with --yes:\n%s", output)
	}
}

func TestDoctorFixRemovesShadowingVarsFromOtherEnvFiles(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, doctorRoutes(t, false))
	dir := t.TempDir()
	linkTestProject(t, dir)
	writeTestFile(t, filepath.Join(dir, ".env.local"), "DATABASE_URL=postgres://u:p@demo.db.capydb.dev:6432/demo\nDATABASE_DIRECT_URL=postgres://u:p@demo.db.capydb.dev:5432/demo\n")
	writeTestFile(t, filepath.Join(dir, ".env"), "APP_NAME=demo\nDATABASE_URL=postgres://u:p@ep-old.eu-central-1.aws.neon.tech/db\n")

	output, _ := runCommand(t, dir, "doctor", "--fix", "--yes", "--api-url", server.URL, "--api-key", "capy_test")
	if env := readTestFile(t, filepath.Join(dir, ".env")); env != "APP_NAME=demo\n" {
		t.Fatalf(".env = %q\n%s", env, output)
	}
	if !strings.Contains(output, "[fixed] env_shadowing: removed DATABASE_URL from .env") || !strings.Contains(output, "[pass] env_shadowing") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestDoctorFixDrizzleConfigIsIdempotent(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	dir := t.TempDir()
	drizzleConfig := "import { defineConfig } from \"drizzle-kit\";\nexport default defineConfig({\n  dialect: \"postgresql\",\n  schema: \"./src/schema.ts\",\n  dbCredentials: { url: process.env.DATABASE_URL! },\n});\n"
	writeTestFile(t, filepath.Join(dir, "drizzle.config.ts"), drizzleConfig)
	fixes := (&app{cwd: dir}).fixConfigFindings(mustLint(t, dir))
	got := readTestFile(t, filepath.Join(dir, "drizzle.config.ts"))
	if !strings.Contains(got, "  dialect: \"postgresql\",\n  schemaFilter: [\"public\"],\n") {
		t.Fatalf("schemaFilter not added:\n%s\n%+v", got, fixes)
	}
	again := (&app{cwd: dir}).fixConfigFindings(mustLint(t, dir))
	for _, fix := range again {
		if fix.Status == fixApplied {
			t.Fatalf("second run changed the file again: %+v", again)
		}
	}
	_ = os.Remove(filepath.Join(dir, "drizzle.config.ts"))
}

func mustLint(t *testing.T, dir string) []configlint.Finding {
	t.Helper()
	findings, err := configlint.Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}
