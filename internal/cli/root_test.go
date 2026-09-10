package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
)

func TestCreateCommandWritesEnvAndProjectConfig(t *testing.T) {
	t.Setenv("CI", "true")

	projectID := "project_123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeJSON(t, w, map[string]any{
				"organization": map[string]any{
					"id":             "org_123",
					"name":           "Test Org",
					"billing_plan":   "ship",
					"billing_status": "active",
				},
				"principal": map[string]any{
					"organization_id": "org_123",
					"scopes":          []string{"*"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/regions":
			writeJSON(t, w, map[string]any{
				"regions": []string{"swedencentral"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"job": map[string]any{},
				"project": map[string]any{
					"id":                  projectID,
					"organization_id":     "org_123",
					"primary_instance_id": "inst_123",
					"region":              "swedencentral",
					"name":                "test-app",
					"slug":                "test-app",
					"state":               "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID:
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"id":                  projectID,
					"organization_id":     "org_123",
					"primary_instance_id": "inst_123",
					"region":              "swedencentral",
					"name":                "test-app",
					"slug":                "test-app",
					"state":               "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/kv":
			// A project without a K/V store: `env pull` must skip it silently.
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/connections":
			writeJSON(t, w, map[string]any{
				"connections": map[string]any{
					"direct_url": "postgresql://direct-user:secret@capydb.dev:5432/test_app?sslmode=require",
					"pooled_url": "postgresql://direct-user:secret@capydb.dev:6432/test_app?sslmode=require",
					"username":   "direct-user",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"dependencies":{"next":"15.0.0"}}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"create",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--yes",
		"--name", "test-app",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute create: %v", err)
	}

	envData, err := os.ReadFile(filepath.Join(tempDir, ".env.local"))
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	envText := string(envData)
	for _, expected := range []string{
		`DATABASE_URL="postgresql://direct-user:secret@capydb.dev:6432/test_app?sslmode=require"`,
		`DATABASE_DIRECT_URL="postgresql://direct-user:secret@capydb.dev:5432/test_app?sslmode=require"`,
		`DATABASE_POOL_URL="postgresql://direct-user:secret@capydb.dev:6432/test_app?sslmode=require"`,
	} {
		if !strings.Contains(envText, expected) {
			t.Fatalf("env file missing %q\n%s", expected, envText)
		}
	}

	projectConfig, err := config.LoadProjectConfig(tempDir)
	if err != nil {
		t.Fatalf("load project config: %v", err)
	}
	if projectConfig.ProjectID != projectID {
		t.Fatalf("unexpected project id: %s", projectConfig.ProjectID)
	}
	if projectConfig.EnvFile != ".env.local" {
		t.Fatalf("unexpected env file: %s", projectConfig.EnvFile)
	}

	gitignoreData, err := os.ReadFile(filepath.Join(tempDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignoreData), ".capydb/") {
		t.Fatalf(".gitignore does not ignore .capydb:\n%s", string(gitignoreData))
	}
	if !strings.Contains(string(gitignoreData), ".env.local") {
		t.Fatalf(".gitignore does not ignore the credential-bearing env file:\n%s", string(gitignoreData))
	}
}

func TestEnvPullRefreshesExistingEnvFile(t *testing.T) {
	t.Setenv("CI", "true")

	projectID := "project_456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID:
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"id":              projectID,
					"organization_id": "org_456",
					"region":          "swedencentral",
					"name":            "app",
					"slug":            "app",
					"state":           "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/kv":
			// A project without a K/V store: `env pull` must skip it silently.
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/connections":
			writeJSON(t, w, map[string]any{
				"connections": map[string]any{
					"direct_url": "postgresql://user:new-secret@capydb.dev:5432/app?sslmode=require",
					"pooled_url": "postgresql://user:new-secret@capydb.dev:6432/app?sslmode=require",
					"username":   "user",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, ".env"), []byte("# existing\nDATABASE_URL=\"old\"\n"), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:    server.URL,
		EnvFile:   ".env",
		Framework: "generic",
		ProjectID: projectID,
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"env",
		"pull",
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute env pull: %v", err)
	}

	envData, err := os.ReadFile(filepath.Join(tempDir, ".env"))
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	envText := string(envData)
	if !strings.Contains(envText, "# existing") {
		t.Fatalf("expected existing comment to remain:\n%s", envText)
	}
	if !strings.Contains(envText, `DATABASE_URL="postgresql://user:new-secret@capydb.dev:5432/app?sslmode=require"`) {
		t.Fatalf("expected DATABASE_URL to refresh:\n%s", envText)
	}
}

func TestStatusRemotePrintsProjectHealth(t *testing.T) {
	t.Setenv("CI", "true")

	projectID := "project_status"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			writeJSON(t, w, map[string]any{
				"service":    "capydb-controlplane",
				"status":     "operational",
				"updated_at": "2026-05-08T10:00:00Z",
				"components": []map[string]any{
					{"name": "Control plane API", "status": "operational"},
					{"name": "Metadata database", "status": "operational"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID:
			if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
				t.Fatalf("unexpected authorization header: %q", got)
			}
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"environment":   "production",
					"id":            projectID,
					"latest_job_id": "job_status",
					"name":          "Status App",
					"state":         "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_status":
			writeJSON(t, w, map[string]any{
				"job": map[string]any{
					"id":    "job_status",
					"state": "completed",
					"type":  "project.backup",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/observability":
			writeJSON(t, w, map[string]any{
				"observability": map[string]any{
					"connection_count":         2,
					"connection_limit":         15,
					"connection_usage_percent": 13.3,
					"database_size_bytes":      1048576,
					"storage_limit_bytes":      1073741824,
					"storage_usage_percent":    0.1,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/backups":
			writeJSON(t, w, map[string]any{
				"backups": []map[string]any{{
					"backup_key":         "logical/project_status.dump.zst",
					"created_at":         "2026-05-08T09:30:00Z",
					"id":                 "backup_status",
					"state":              "verified",
					"verification_state": "verified",
				}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:      server.URL,
		ProjectID:   projectID,
		ProjectName: "Status App",
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"--api-key", "capy_test_key",
		"status",
		"--remote",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute status --remote: %v", err)
	}

	text := output.String()
	for _, expected := range []string{
		"api_status: operational",
		"remote_project_state: ready",
		"connections: 2/15",
		"latest_backup_verify: verified",
		"latest_job_state: completed",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("status output missing %q\n%s", expected, text)
		}
	}
}

func TestPreviewCreateResolvesProjectByName(t *testing.T) {
	t.Setenv("CI", "true")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":              "project_789",
					"organization_id": "org_789",
					"region":          "swedencentral",
					"name":            "test-app",
					"slug":            "test-app",
					"state":           "ready",
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/project_789/preview-databases":
			writeJSON(t, w, map[string]any{
				"job": map[string]any{
					"id":    "job_preview_1",
					"state": "pending",
					"type":  "branch.create",
				},
				"preview": map[string]any{
					"id":             "prv_123",
					"name":           "qa-db",
					"mode":           "clone",
					"state":          "provisioning",
					"project_id":     "project_789",
					"ttl_expires_at": "2026-04-02T12:00:00Z",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/preview-databases/prv_123/connections":
			writeJSON(t, w, map[string]any{
				"connections": map[string]any{
					"direct_url": "postgresql://preview:secret@capydb.dev:5432/preview_db?sslmode=require",
					"pooled_url": "postgresql://preview:secret@capydb.dev:6432/preview_db?sslmode=require",
					"username":   "preview",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"preview",
		"create",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "test-app",
		"--mode", "clone",
		"--name", "qa-db",
		"--ttl-hours", "48",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute preview create: %v", err)
	}

	text := output.String()
	if !strings.Contains(text, "Created preview qa-db (prv_123) for project test-app") {
		t.Fatalf("unexpected output:\n%s", text)
	}
	if !strings.Contains(text, "postgresql://preview:secret@capydb.dev:6432/preview_db?sslmode=require") {
		t.Fatalf("expected pooled URL in output:\n%s", text)
	}
}

func TestBackupsListUsesLinkedProject(t *testing.T) {
	t.Setenv("CI", "true")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_321":
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"id":              "project_321",
					"organization_id": "org_321",
					"region":          "swedencentral",
					"name":            "linked-app",
					"slug":            "linked-app",
					"state":           "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_321/backups":
			writeJSON(t, w, map[string]any{
				"backups": []map[string]any{{
					"id":         "bkp_1",
					"backup_key": "capydbv1/db/project_321/2026-04-01T10:00:00Z.zst",
					"label":      "nightly",
					"state":      "ready",
					"created_at": "2026-04-01T10:00:00Z",
					"project_id": "project_321",
				}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:    server.URL,
		EnvFile:   ".env",
		Framework: "generic",
		ProjectID: "project_321",
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"backups",
		"list",
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute backups list: %v", err)
	}

	text := output.String()
	if !strings.Contains(text, "nightly") || !strings.Contains(text, "bkp_1") {
		t.Fatalf("unexpected output:\n%s", text)
	}
}

func TestStudioPrintOnlyUsesLinkedProjectWithoutAPIKey(t *testing.T) {
	t.Setenv("CAPYDB_APP_URL", "https://capydb.dev")

	tempDir := t.TempDir()
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:    "https://capydb.dev/api/capydb",
		EnvFile:   ".env",
		Framework: "generic",
		ProjectID: "project_999",
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"studio",
		"--print-only",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute studio: %v", err)
	}

	text := strings.TrimSpace(output.String())
	if text != "https://capydb.dev/dashboard/projects/project_999/connections" {
		t.Fatalf("unexpected studio url: %s", text)
	}
}

func TestStudioPageCanOpenProjectStudio(t *testing.T) {
	t.Parallel()

	got, err := buildDashboardURL("https://capydb.dev/", "acme", "api-prod", "project_999", "studio")
	if err != nil {
		t.Fatalf("build dashboard url: %v", err)
	}
	if got != "https://capydb.dev/dashboard/acme/api-prod/studio" {
		t.Fatalf("unexpected studio page URL: %s", got)
	}
}

func TestLoginBrowserFlowSavesUserConfig(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/cli/login/sessions":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode login request: %v", err)
			}
			if request["name"] != "CapyDB CLI on local-dev" {
				t.Fatalf("unexpected key name: %#v", request["name"])
			}
			if request["device_name"] != "local-dev" {
				t.Fatalf("unexpected device name: %#v", request["device_name"])
			}
			if _, ok := request["expires_at"].(string); !ok {
				t.Fatalf("expected expires_at in request body: %#v", request)
			}
			writeJSON(t, w, map[string]any{
				"expires_at": "2026-04-23T12:30:00Z",
				"poll_token": "poll_123",
				"session_id": "cli_123",
				"state":      "pending",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/cli/login/sessions/cli_123":
			if got := r.Header.Get("X-CapyDB-Poll-Token"); got != "poll_123" {
				t.Fatalf("unexpected poll token header: %q", got)
			}
			writeJSON(t, w, map[string]any{
				"authorized_at":     "2026-04-23T12:21:00Z",
				"expires_at":        "2026-04-23T12:30:00Z",
				"organization_id":   "org_123",
				"organization_name": "Test Org",
				"organization_slug": "test-org",
				"plaintext_api_key": "capy_live_123",
				"session_id":        "cli_123",
				"state":             "completed",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"login",
		"--api-url", server.URL,
		"--app-url", "https://capydb.dev",
		"--device-name", "local-dev",
		"--expires-in", "7d",
		"--name", "CapyDB CLI on local-dev",
		"--no-open",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute login: %v", err)
	}

	userConfig, err := config.LoadUserConfig()
	if err != nil {
		t.Fatalf("load user config: %v", err)
	}
	orgID, entry, ok := userConfig.Active()
	if !ok {
		t.Fatalf("expected an active organization in the saved config: %#v", userConfig)
	}
	if entry.APIKey != "capy_live_123" {
		t.Fatalf("unexpected api key: %s", entry.APIKey)
	}
	if orgID != "org_123" {
		t.Fatalf("unexpected organization id: %s", orgID)
	}
	if entry.Slug != "test-org" {
		t.Fatalf("unexpected organization slug: %s", entry.Slug)
	}
}

// isolateUserConfig redirects os.UserConfigDir into a temp directory so tests
// never read or clobber the developer's real CLI config. HOME covers macOS
// (~/Library/Application Support) and XDG_CONFIG_HOME covers Linux.
// TestMain clears the CapyDB environment for every test in the package.
//
// Isolation used to be opt-in via isolateUserConfig, and 11 of 13 tests in this
// file did not opt in - so a maintainer who exports CAPYDB_API_KEY (the normal
// way to use this CLI) saw six failures locally that CI never sees: the real
// key reached httptest servers as a 401, and the "fails without auth" tests
// found auth. Clearing here makes the safe state the default; a test that wants
// one of these set still calls t.Setenv and wins.
func TestMain(m *testing.M) {
	for _, name := range capydbEnvVars {
		if err := os.Unsetenv(name); err != nil {
			panic("unset " + name + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

// capydbEnvVars is every environment variable the CLI reads for credentials or
// endpoints. Kept complete deliberately: a var missing here leaks the
// developer's real environment into the tests.
var capydbEnvVars = []string{
	"CAPYDB_API_KEY",
	"CAPYDB_API_URL",
	"CAPYDB_APP_URL",
	"CAPYDB_AGENT",
	"CAPYDB_HTTP_TIMEOUT",
	"CAPYDB_REPO",
}

// isolateUserConfig gives the test its own config directory AND an environment
// with no CapyDB credentials in it.
//
// Clearing the environment is not belt-and-braces: a maintainer who exports
// CAPYDB_API_KEY (the normal way to use this CLI) had six tests fail on their
// machine and pass in CI, because the real key reached an httptest server as a
// 401, and the "should fail without auth" tests found auth. A suite that only
// passes on a clean machine is a suite whose local runs cannot be trusted -
// which is exactly when you need them.
//
// t.Setenv first so the original value is restored at test end, then Unsetenv
// so the variable is genuinely absent rather than empty.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	for _, name := range capydbEnvVars {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func TestLinkCommandDetectsNestedAppAndResolvesProjectByName(t *testing.T) {
	t.Setenv("CI", "true")

	projectID := "project_123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":              projectID,
					"organization_id": "org_123",
					"region":          "swedencentral",
					"name":            "web-app",
					"slug":            "web-app",
					"state":           "ready",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID:
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"id":              projectID,
					"organization_id": "org_123",
					"region":          "swedencentral",
					"name":            "web-app",
					"slug":            "web-app",
					"state":           "ready",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/kv":
			// A project without a K/V store: `env pull` must skip it silently.
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"error": "kv store not found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/"+projectID+"/connections":
			writeJSON(t, w, map[string]any{
				"connections": map[string]any{
					"direct_url": "postgresql://direct-user:secret@capydb.dev:5432/test_app?sslmode=require",
					"pooled_url": "postgresql://direct-user:secret@capydb.dev:6432/test_app?sslmode=require",
					"username":   "direct-user",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"private":true,"workspaces":["apps/*"]}`), 0o644); err != nil {
		t.Fatalf("write root package.json: %v", err)
	}
	appDir := filepath.Join(tempDir, "apps", "web")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "package.json"), []byte(`{"dependencies":{"next":"15.0.0"}}`), 0o644); err != nil {
		t.Fatalf("write app package.json: %v", err)
	}

	application := &app{cwd: tempDir}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"link",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "web-app",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute link: %v", err)
	}

	envData, err := os.ReadFile(filepath.Join(appDir, ".env.local"))
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(envData), `DATABASE_URL="postgresql://direct-user:secret@capydb.dev:6432/test_app?sslmode=require"`) {
		t.Fatalf("unexpected env file:\n%s", string(envData))
	}

	projectConfig, err := config.LoadProjectConfig(tempDir)
	if err != nil {
		t.Fatalf("load project config: %v", err)
	}
	if projectConfig.AppPath != filepath.Join("apps", "web") {
		t.Fatalf("unexpected app path: %s", projectConfig.AppPath)
	}
	if projectConfig.ProjectSlug != "web-app" {
		t.Fatalf("unexpected project slug: %s", projectConfig.ProjectSlug)
	}

	gitignoreData, err := os.ReadFile(filepath.Join(tempDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignoreData), filepath.Join("apps", "web", ".env.local")) {
		t.Fatalf(".gitignore does not ignore the nested app env file:\n%s", string(gitignoreData))
	}
}

func TestLoginPollFailsFastWhenSessionNotFound(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	pollCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/cli/login/sessions":
			writeJSON(t, w, map[string]any{
				"expires_at": time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
				"poll_token": "poll_404",
				"session_id": "cli_404",
				"state":      "pending",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/cli/login/sessions/cli_404":
			pollCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"cli login session not found"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"login",
		"--api-url", server.URL,
		"--app-url", "https://capydb.dev",
		"--no-open",
	})

	err := command.Execute()
	if err == nil {
		t.Fatalf("expected login to fail fast on 404")
	}
	if !strings.Contains(err.Error(), "login session expired or not found; run `capydb login` again") {
		t.Fatalf("unexpected error: %v", err)
	}
	if pollCalls != 1 {
		t.Fatalf("poll calls = %d, want 1 (must fail on the first 404 instead of spinning)", pollCalls)
	}
}

func TestParseCLILoginDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{input: "24h", want: 24 * time.Hour},
		{input: "7d", want: 7 * 24 * time.Hour},
		{input: "2w", want: 14 * 24 * time.Hour},
		{input: "-1h", wantErr: true},
		{input: "abc", wantErr: true},
	}

	for _, tt := range tests {
		got, err := parseCLILoginDuration(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("parseCLILoginDuration(%q) error = nil, want error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseCLILoginDuration(%q) error = %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("parseCLILoginDuration(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode json: %v", err)
	}
}

func TestCreateErrorPayloadMapsBillingAndAuthFailures(t *testing.T) {
	billingErr := ensureViewerCanProvision(api.Viewer{Organization: &api.Organization{BillingPlan: "none", BillingStatus: "inactive"}}, "https://capydb.dev")
	payload := createErrorPayload(billingErr)
	if payload["error"] != "billing_inactive" {
		t.Fatalf("error = %q, want billing_inactive", payload["error"])
	}
	if payload["action"] != "open_url" || payload["url"] != "https://capydb.dev/dashboard/settings/billing" {
		t.Fatalf("payload = %v, want open_url action with billing url", payload)
	}

	payload = createErrorPayload(authErrorf("no api key available"))
	if payload["error"] != "auth_required" || payload["command"] != "capydb login" {
		t.Fatalf("payload = %v, want auth_required with capydb login command", payload)
	}

	payload = createErrorPayload(errors.New("boom"))
	if payload["error"] != "create_failed" || payload["message"] != "boom" {
		t.Fatalf("payload = %v, want create_failed passthrough", payload)
	}
}

func TestResolveCLILoginNameLabelsAgentSessions(t *testing.T) {
	if got := resolveCLILoginName("", "laptop", "agent"); got != "Agent on laptop" {
		t.Fatalf("agent name = %q, want %q", got, "Agent on laptop")
	}
	if got := resolveCLILoginName("", "laptop", "cli"); got != "CLI on laptop" {
		t.Fatalf("cli name = %q, want %q", got, "CLI on laptop")
	}
	if got := resolveCLILoginName("custom", "laptop", "agent"); got != "custom" {
		t.Fatalf("explicit name = %q, want custom", got)
	}
}
