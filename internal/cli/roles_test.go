package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

func TestAppRoleEnvVars(t *testing.T) {
	connections := api.ProjectConnectionInfo{
		DirectURL: "postgresql://owner@h:5432/app",
		PooledURL: "postgresql://owner@h:6432/app",
		App: &api.AppRoleConnectionInfo{
			Username:  "app_user",
			DirectURL: "postgresql://app_user@h:5432/app",
			PooledURL: "postgresql://app_user@h:6432/app",
		},
	}

	// DATABASE_URL pooled (Next.js, Drizzle): the app URL is pooled too.
	vars := appRoleEnvVars(connections, connections.PooledURL)
	if vars[appURLVar] != connections.App.PooledURL || vars[appPoolURLVar] != connections.App.PooledURL {
		t.Fatalf("pooled stack vars = %v", vars)
	}
	// DATABASE_URL direct (pgx, SQLAlchemy): the app URL is direct.
	vars = appRoleEnvVars(connections, connections.DirectURL)
	if vars[appURLVar] != connections.App.DirectURL || vars[appPoolURLVar] != connections.App.PooledURL {
		t.Fatalf("direct stack vars = %v", vars)
	}
	connections.App = nil
	if vars := appRoleEnvVars(connections, connections.PooledURL); vars != nil {
		t.Fatalf("no app role wrote %v", vars)
	}
}

func TestEnvPullWritesAppRoleURLs(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/projects/prj_env":
			writeJSON(t, w, map[string]any{"project": map[string]any{"id": "prj_env", "organization_id": "org_1", "name": "app", "slug": "app"}})
		case "GET /v1/projects/prj_env/kv":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case "GET /v1/projects/prj_env/connections":
			writeJSON(t, w, map[string]any{"connections": map[string]any{
				"username":   "owner",
				"direct_url": "postgresql://owner:pw@app.db.capydb.dev:5432/app",
				"pooled_url": "postgresql://owner:pw@app.db.capydb.dev:6432/app",
				"app": map[string]any{
					"username":   "app_user",
					"direct_url": "postgresql://app_user:apw@app.db.capydb.dev:5432/app",
					"pooled_url": "postgresql://app_user:apw@app.db.capydb.dev:6432/app",
				},
			}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := config.SaveProjectConfig(dir, config.ProjectConfig{
		APIURL: server.URL, EnvFile: ".env", Framework: "nextjs", DatabaseLayer: "drizzle", ProjectID: "prj_env",
	}); err != nil {
		t.Fatal(err)
	}
	if output, err := runCommand(t, dir, "env", "pull", "--api-key", "capy_test_key"); err != nil {
		t.Fatalf("env pull: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`DATABASE_URL="postgresql://owner:pw@app.db.capydb.dev:6432/app"`,
		`DATABASE_APP_URL="postgresql://app_user:apw@app.db.capydb.dev:6432/app"`,
		`DATABASE_APP_POOL_URL="postgresql://app_user:apw@app.db.capydb.dev:6432/app"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("env file missing %s:\n%s", want, data)
		}
	}
}

func appRoleServer(t *testing.T, status map[string]any, calls *[]string) *httptest.Server {
	t.Helper()
	job := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			*calls = append(*calls, name)
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_" + name, "state": "queued", "type": "project.app_role_" + name}})
		}
	}
	return newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/projects/prj_1/roles/app":         func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, map[string]any{"app_role": status}) },
		"POST /v1/projects/prj_1/roles/app":        job("enable"),
		"POST /v1/projects/prj_1/roles/app/rotate": job("rotate"),
	})
}

func TestRolesAppShow(t *testing.T) {
	var calls []string
	server := appRoleServer(t, map[string]any{"available": true, "enabled": true, "username": "app_user", "created_at": "2026-09-30T10:00:00Z"}, &calls)
	output, err := runCommand(t, t.TempDir(), "roles", "app", "show", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, output)
	}
	for _, want := range []string{"enabled: yes", "username: app_user", "created_at:", "DATABASE_APP_POOL_URL"} {
		if !strings.Contains(output, want) {
			t.Fatalf("show missing %q:\n%s", want, output)
		}
	}
	output, err = runCommand(t, t.TempDir(), "roles", "app", "show", "--project", "prj_1", "-o", "json", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil || !strings.Contains(output, `"app_role"`) || !strings.Contains(output, `"enabled": true`) {
		t.Fatalf("show json: %v\n%s", err, output)
	}
}

func TestRolesAppEnable(t *testing.T) {
	var calls []string
	server := appRoleServer(t, map[string]any{"available": true, "enabled": false}, &calls)
	output, err := runCommand(t, t.TempDir(), "roles", "app", "enable", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, output)
	}
	if len(calls) != 1 || calls[0] != "enable" || !strings.Contains(output, "Enabling app role") || !strings.Contains(output, "capydb env pull") {
		t.Fatalf("calls=%v output:\n%s", calls, output)
	}

	// Already enabled: nothing is queued.
	calls = nil
	server = appRoleServer(t, map[string]any{"available": true, "enabled": true, "username": "app_user"}, &calls)
	output, err = runCommand(t, t.TempDir(), "roles", "app", "enable", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil || len(calls) != 0 || !strings.Contains(output, "already has its runtime login") {
		t.Fatalf("already enabled: calls=%v err=%v\n%s", calls, err, output)
	}

	// Not offered: refused before the POST.
	server = appRoleServer(t, map[string]any{"available": false, "enabled": false}, &calls)
	_, err = runCommand(t, t.TempDir(), "roles", "app", "enable", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || len(calls) != 0 || !strings.Contains(err.Error(), "not offered") {
		t.Fatalf("not offered: calls=%v err=%v", calls, err)
	}
}

func TestRolesAppRotate(t *testing.T) {
	var calls []string
	server := appRoleServer(t, map[string]any{"available": true, "enabled": true}, &calls)
	output, err := runCommand(t, t.TempDir(), "roles", "app", "rotate", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil || len(calls) != 1 || calls[0] != "rotate" || !strings.Contains(output, "old password stops working") {
		t.Fatalf("rotate: calls=%v err=%v\n%s", calls, err, output)
	}
}
