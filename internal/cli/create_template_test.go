package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStarterTemplatesApplyCleanly(t *testing.T) {
	url := liveDatabaseURL(t)
	for _, name := range starterTemplateNames() {
		script, err := starterTemplates[name].script()
		if err != nil {
			t.Fatal(err)
		}
		if name == "empty" {
			if script != "" {
				t.Fatal("the empty template must apply nothing")
			}
			continue
		}
		// Each template gets its own schema so both can use the same table
		// names; search_path puts the unqualified names there.
		liveExec(t, url, "CREATE SCHEMA "+quoteIdent(name)+"; SET search_path TO "+quoteIdent(name)+", public;\n"+script)
	}
	rows := liveQueryRows(t, url, `SELECT (SELECT count(*) FROM "drizzle-starter".posts) AS posts, (SELECT count(*) FROM "auth-starter".users) AS users`)
	if rows[0]["posts"].(int64) != 3 || rows[0]["users"].(int64) != 1 {
		t.Fatalf("seed rows = %+v", rows[0])
	}
}

// createRoutes is a fake control plane for `capydb create` whose new project
// connects to directURL.
func createRoutes(t *testing.T, directURL string) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/me":      func(w http.ResponseWriter, r *http.Request) { writeViewer(t, w, "org_1") },
		"GET /v1/regions": func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, map[string]any{"regions": []string{"hel1"}}) },
		"POST /v1/projects": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{"project": fakeProject, "job": map[string]any{}})
		},
		"GET /v1/projects/prj_1/connections": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"connections": map[string]any{"direct_url": directURL, "pooled_url": directURL}})
		},
		"GET /v1/projects/prj_1/kv": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "not found"})
		},
	}
}

func TestCreateTemplateAppliesSchemaAndSeedAfterCreate(t *testing.T) {
	url := liveDatabaseURL(t)
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, createRoutes(t, url))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), `{"name": "demo"}`)

	output, err := runCommand(t, dir, "create", "--template", "drizzle-starter", "--region", "hel1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("create --template: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Template drizzle-starter applied.") || !strings.Contains(output, "capydb init drizzle") {
		t.Fatalf("unexpected output:\n%s", output)
	}
	rows := liveQueryRows(t, url, "SELECT count(*) AS n FROM posts")
	if rows[0]["n"].(int64) != 3 {
		t.Fatalf("posts = %v", rows[0]["n"])
	}
}

func TestCreateTemplateFailureLeavesTheSQLForSeed(t *testing.T) {
	url := liveDatabaseURL(t)
	liveExec(t, url, "CREATE TABLE users (id int)") // collides with the template
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	server := newFakeControlPlane(t, nil, createRoutes(t, url))
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), `{"name": "demo"}`)

	output, err := runCommand(t, dir, "create", "--template", "auth-starter", "--region", "hel1", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("err = %v\n%s", err, output)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "capydb-template-auth-starter.sql")); statErr != nil {
		t.Fatalf("template SQL not saved: %v", statErr)
	}
	// One transaction: the sessions table from the same script must not exist.
	rows := liveQueryRows(t, url, "SELECT to_regclass('public.sessions') IS NULL AS absent")
	if rows[0]["absent"] != true {
		t.Fatal("a failed template left tables behind")
	}
}

func TestCreateRejectsUnknownTemplateBeforeCreating(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	// No routes: any request fails the test.
	server := newFakeControlPlane(t, nil, nil)
	_, err := runCommand(t, t.TempDir(), "create", "--template", "rails", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "unknown --template") {
		t.Fatalf("err = %v", err)
	}
}
