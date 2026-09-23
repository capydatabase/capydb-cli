package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

// writeViewer answers /v1/me with a minimal org-bound viewer for org-scoped
// commands that resolve the organization id from the viewer.
func writeViewer(t *testing.T, w http.ResponseWriter, orgID string) {
	t.Helper()
	writeJSON(t, w, map[string]any{
		"organization": map[string]any{
			"id":             orgID,
			"name":           "Test Org",
			"slug":           "test-org",
			"billing_plan":   "ship",
			"billing_status": "active",
		},
		"principal": map[string]any{
			"organization_id": orgID,
			"scopes":          []string{"*"},
		},
	})
}

func runCommand(t *testing.T, cwd string, args ...string) (string, error) {
	t.Helper()
	application := &app{cwd: cwd}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestWebhooksCreatePrintsSecretOnce(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_hook")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/org_hook/webhook-endpoints":
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode webhook request: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"webhook_endpoint": map[string]any{
					"id":              "whe_1",
					"organization_id": "org_hook",
					"url":             "https://example.com/hooks",
					"event_types":     []string{"backup.completed"},
					"is_active":       true,
				},
				"plaintext_secret": "whsec_show_once",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"webhooks", "create",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--url", "https://example.com/hooks",
		"--event", "backup.completed",
		"--description", "ci hook",
	)
	if err != nil {
		t.Fatalf("execute webhooks create: %v", err)
	}

	if requestBody["url"] != "https://example.com/hooks" {
		t.Fatalf("unexpected url in request: %#v", requestBody)
	}
	if requestBody["description"] != "ci hook" {
		t.Fatalf("unexpected description in request: %#v", requestBody)
	}
	if !strings.Contains(output, "signing_secret: whsec_show_once") {
		t.Fatalf("expected signing secret in output:\n%s", output)
	}
	if !strings.Contains(output, "Created webhook endpoint whe_1") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestWebhooksDeliveriesListsRecentDeliveries(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_hook")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/organizations/org_hook/webhook-endpoints/whe_1/deliveries":
			if got := r.URL.Query().Get("limit"); got != "5" {
				t.Fatalf("unexpected limit: %q", got)
			}
			writeJSON(t, w, map[string]any{
				"deliveries": []map[string]any{{
					"id":              "whd_1",
					"endpoint_id":     "whe_1",
					"event_type":      "backup.completed",
					"state":           "delivered",
					"attempts":        1,
					"max_attempts":    5,
					"response_status": 200,
					"created_at":      "2026-06-01T10:00:00Z",
				}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"webhooks", "deliveries", "whe_1",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--limit", "5",
	)
	if err != nil {
		t.Fatalf("execute webhooks deliveries: %v", err)
	}
	if !strings.Contains(output, "whd_1") || !strings.Contains(output, "backup.completed") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestAPIKeysCreateSendsScopesAndProjectScope(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_keys")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":              "project_ci",
					"organization_id": "org_keys",
					"region":          "swedencentral",
					"name":            "ci-app",
					"slug":            "ci-app",
					"state":           "ready",
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/org_keys/api-keys":
			if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode api key request: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"api_key": map[string]any{
					"id":              "key_1",
					"name":            "ci key",
					"organization_id": "org_keys",
					"project_id":      "project_ci",
					"scopes":          []string{"projects:read", "backups:read"},
					"key_prefix":      "capy_ci",
					"is_active":       true,
					"created_at":      "2026-06-01T10:00:00Z",
					"source":          "api",
				},
				"plaintext_api_key": "capy_ci_plaintext_once",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"api-keys", "create",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--name", "ci key",
		"--scopes", "projects:read,backups:read",
		"--project", "ci-app",
	)
	if err != nil {
		t.Fatalf("execute api-keys create: %v", err)
	}

	scopes, ok := requestBody["scopes"].([]any)
	if !ok || len(scopes) != 2 || scopes[0] != "projects:read" || scopes[1] != "backups:read" {
		t.Fatalf("unexpected scopes in request: %#v", requestBody)
	}
	if requestBody["project_id"] != "project_ci" {
		t.Fatalf("expected resolved project id in request: %#v", requestBody)
	}
	if !strings.Contains(output, "api_key: capy_ci_plaintext_once") {
		t.Fatalf("expected plaintext key in output:\n%s", output)
	}
}

func TestAPIKeysCreateRequiresScopes(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	_, err := runCommand(t, t.TempDir(),
		"api-keys", "create",
		"--api-key", "capy_test_key",
		"--name", "ci key",
	)
	if err == nil {
		t.Fatalf("expected a usage error without --scopes")
	}
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.UsageError {
		t.Fatalf("expected usage exit code, got %v", err)
	}
}

func TestAuditListPrintsEvents(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":    "project_audit",
					"name":  "audit-app",
					"slug":  "audit-app",
					"state": "ready",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_audit/audit-events":
			writeJSON(t, w, map[string]any{
				"audit_events": []map[string]any{{
					"id":              "evt_1",
					"organization_id": "org_audit",
					"project_id":      "project_audit",
					"action":          "project.backup.create",
					"actor_kind":      "user",
					"actor_id":        "user_1",
					"created_at":      "2026-06-01T09:00:00Z",
					"metadata":        map[string]any{"label": "nightly"},
				}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"audit", "list",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "audit-app",
	)
	if err != nil {
		t.Fatalf("execute audit list: %v", err)
	}
	if !strings.Contains(output, "project.backup.create") || !strings.Contains(output, "user:user_1") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestExtensionsEnableWaitsForJob(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":    "project_ext",
					"name":  "ext-app",
					"slug":  "ext-app",
					"state": "ready",
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/project_ext/extensions":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode extension request: %v", err)
			}
			if request["name"] != "pgvector" {
				t.Fatalf("unexpected extension name: %#v", request)
			}
			writeJSON(t, w, map[string]any{
				"job": map[string]any{
					"id":    "job_ext",
					"state": "pending",
					"type":  "project.extension_enable",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job_ext":
			writeJSON(t, w, map[string]any{
				"job": map[string]any{
					"id":    "job_ext",
					"state": "completed",
					"type":  "project.extension_enable",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"extensions", "enable", "pgvector",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "ext-app",
		"--wait",
	)
	if err != nil {
		t.Fatalf("execute extensions enable: %v", err)
	}
	if !strings.Contains(output, "Queued enable of extension pgvector") {
		t.Fatalf("unexpected output:\n%s", output)
	}
	if !strings.Contains(output, "state: completed") {
		t.Fatalf("expected completed job state in output:\n%s", output)
	}
}

func TestExtensionsListJSONOutput(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":    "project_ext",
					"name":  "ext-app",
					"slug":  "ext-app",
					"state": "ready",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_ext/extensions":
			writeJSON(t, w, map[string]any{
				"extensions": []map[string]any{{
					"name":            "pgvector",
					"enabled":         true,
					"trusted":         true,
					"description":     "vector similarity search",
					"default_version": "0.8.0",
				}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"--output", "json",
		"extensions", "list",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "ext-app",
	)
	if err != nil {
		t.Fatalf("execute extensions list: %v", err)
	}

	var decoded struct {
		Extensions []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode json output: %v\n%s", err, output)
	}
	if len(decoded.Extensions) != 1 || decoded.Extensions[0].Name != "pgvector" || !decoded.Extensions[0].Enabled {
		t.Fatalf("unexpected json output:\n%s", output)
	}
}

func TestAlertsListAndAcknowledge(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	acked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":    "project_alert",
					"name":  "alert-app",
					"slug":  "alert-app",
					"state": "ready",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_alert/alerts":
			writeJSON(t, w, map[string]any{
				"alerts": []map[string]any{{
					"id":             "alert_1",
					"kind":           "storage",
					"severity":       "warning",
					"observed_value": 92,
					"limit_value":    90,
					"triggered_at":   "2026-06-01T08:00:00Z",
				}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/project_alert/alerts/alert_1/acknowledge":
			acked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"alerts", "list",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "alert-app",
	)
	if err != nil {
		t.Fatalf("execute alerts list: %v", err)
	}
	if !strings.Contains(output, "alert_1") || !strings.Contains(output, "storage") || !strings.Contains(output, "92") {
		t.Fatalf("unexpected output:\n%s", output)
	}

	output, err = runCommand(t, t.TempDir(),
		"alerts", "ack", "alert_1",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--project", "alert-app",
	)
	if err != nil {
		t.Fatalf("execute alerts ack: %v", err)
	}
	if !acked {
		t.Fatalf("acknowledge endpoint was not called")
	}
	if !strings.Contains(output, "Acknowledged alert alert_1") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestDoctorFailsWithoutAuth(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			writeJSON(t, w, map[string]any{
				"service":    "capydb-controlplane",
				"status":     "operational",
				"updated_at": "2026-06-01T10:00:00Z",
				"components": []map[string]any{},
			})
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"doctor",
		"--api-url", server.URL,
	)
	if err == nil {
		t.Fatalf("expected doctor to fail without auth")
	}
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.GenericError {
		t.Fatalf("expected generic exit code, got %v", err)
	}
	for _, expected := range []string{"[pass] config", "[pass] api", "[fail] auth", "[skip] project_link"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("doctor output missing %q:\n%s", expected, output)
		}
	}
}

func TestDoctorPassesWithAuthAndLink(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			writeJSON(t, w, map[string]any{
				"service":    "capydb-controlplane",
				"status":     "operational",
				"updated_at": "2026-06-01T10:00:00Z",
				"components": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_doc")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/project_doc":
			writeJSON(t, w, map[string]any{
				"project": map[string]any{
					"id":    "project_doc",
					"name":  "doc-app",
					"slug":  "doc-app",
					"state": "ready",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:      server.URL,
		ProjectID:   "project_doc",
		ProjectName: "doc-app",
		EnvFile:     ".env",
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	output, err := runCommand(t, tempDir,
		"doctor",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	)
	for _, expected := range []string{"[pass] config", "[pass] api", "[pass] auth", "[pass] project_link"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("doctor output missing %q:\n%s", expected, output)
		}
	}
	// The psql check depends on the host; only a missing psql may fail here.
	if err != nil && !strings.Contains(output, "[fail] psql") {
		t.Fatalf("doctor failed unexpectedly: %v\n%s", err, output)
	}
}

func TestConfigShowMasksAPIKey(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	userConfig := config.UserConfig{}
	userConfig.Upsert("org_cfg", config.OrganizationConfig{
		APIKey: "capy_secret_key_abcd",
		APIURL: "https://capydb.dev/api/capydb",
		AppURL: "https://capydb.dev",
		Name:   "Config Org",
		Slug:   "config-org",
	})
	if err := config.SaveUserConfig(userConfig); err != nil {
		t.Fatalf("save user config: %v", err)
	}

	output, err := runCommand(t, t.TempDir(), "config", "show")
	if err != nil {
		t.Fatalf("execute config show: %v", err)
	}

	if strings.Contains(output, "capy_secret_key_abcd") {
		t.Fatalf("config show leaked the full api key:\n%s", output)
	}
	for _, expected := range []string{
		"api_key_fingerprint: ****abcd",
		"active_org_id: org_cfg",
		"active_org_slug: config-org",
		"api_url: https://capydb.dev/api/capydb",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("config show output missing %q:\n%s", expected, output)
		}
	}
}

func TestVersionCheckReportsLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/capydatabase/capydb-cli/releases/latest" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		writeJSON(t, w, map[string]any{"tag_name": "v9.9.9"})
	}))
	defer server.Close()

	previousBase := releasesAPIBaseURL
	releasesAPIBaseURL = server.URL
	t.Cleanup(func() { releasesAPIBaseURL = previousBase })

	output, err := runCommand(t, t.TempDir(), "--output", "json", "version", "--check")
	if err != nil {
		t.Fatalf("execute version --check: %v", err)
	}

	var decoded versionReport
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode version output: %v\n%s", err, output)
	}
	if decoded.Version != "test" {
		t.Fatalf("unexpected version: %s", decoded.Version)
	}
	if decoded.LatestVersion != "v9.9.9" {
		t.Fatalf("unexpected latest version: %s", decoded.LatestVersion)
	}
	if decoded.UpdateAvailable == nil || !*decoded.UpdateAvailable {
		t.Fatalf("expected update_available=true: %s", output)
	}
}

func TestVersionCheckFailsGracefullyOffline(t *testing.T) {
	previousBase := releasesAPIBaseURL
	// A closed port: connection refused immediately, no 5s hang.
	releasesAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { releasesAPIBaseURL = previousBase })

	output, err := runCommand(t, t.TempDir(), "version", "--check")
	if err != nil {
		t.Fatalf("version --check must not fail hard when offline: %v", err)
	}
	if !strings.Contains(output, "capydb test") {
		t.Fatalf("expected version output:\n%s", output)
	}
	if !strings.Contains(output, "update check failed") {
		t.Fatalf("expected an update-check warning:\n%s", output)
	}
}

func TestUpdateAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		current string
		latest  string
		want    bool
	}{
		{current: "v1.2.3 (commit: abc, built: x, by: y)", latest: "v1.2.4", want: true},
		{current: "v1.2.3", latest: "v1.2.3", want: false},
		{current: "1.2.3", latest: "v1.2.3", want: false},
		{current: "dev (built from source)", latest: "v1.2.3", want: false},
		{current: "", latest: "v1.2.3", want: false},
	}
	for _, tt := range tests {
		if got := updateAvailable(tt.current, tt.latest); got != tt.want {
			t.Fatalf("updateAvailable(%q, %q) = %t, want %t", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestStatusJSONOutputIsMachineReadable(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	tempDir := t.TempDir()
	if err := config.SaveProjectConfig(tempDir, config.ProjectConfig{
		APIURL:      "https://capydb.dev/api/capydb",
		ProjectID:   "project_json",
		ProjectName: "json-app",
		EnvFile:     ".env",
	}); err != nil {
		t.Fatalf("save project config: %v", err)
	}

	output, err := runCommand(t, tempDir, "--output", "json", "status")
	if err != nil {
		t.Fatalf("execute status: %v", err)
	}

	var decoded statusReport
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode status json: %v\n%s", err, output)
	}
	if decoded.LoggedIn {
		t.Fatalf("expected logged_in=false with an isolated config:\n%s", output)
	}
	if decoded.LinkedProject == nil || decoded.LinkedProject.ProjectID != "project_json" {
		t.Fatalf("unexpected linked project: %s", output)
	}
}

func TestInvalidOutputModeIsUsageError(t *testing.T) {
	_, err := runCommand(t, t.TempDir(), "--output", "yaml", "status")
	if err == nil {
		t.Fatalf("expected an error for --output yaml")
	}
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.UsageError {
		t.Fatalf("expected usage exit code, got %v", err)
	}
}

func TestCompletionCommandIsAvailable(t *testing.T) {
	output, err := runCommand(t, t.TempDir(), "completion", "zsh")
	if err != nil {
		t.Fatalf("execute completion zsh: %v", err)
	}
	if !strings.Contains(output, "compdef") {
		t.Fatalf("expected a zsh completion script:\n%s", output)
	}
}

func TestWebhooksTestQueuesDelivery(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_hook")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/org_hook/webhook-endpoints/whe_1/test":
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"delivery": map[string]any{
					"id":              "whd_test",
					"endpoint_id":     "whe_1",
					"organization_id": "org_hook",
					"event_type":      "webhook.test",
					"state":           "pending",
					"attempts":        0,
					"max_attempts":    1,
					"created_at":      "2026-07-28T10:00:00Z",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"webhooks", "test", "whe_1",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	)
	if err != nil {
		t.Fatalf("execute webhooks test: %v", err)
	}
	if !strings.Contains(output, "Queued test event delivery whd_test for webhook endpoint whe_1") {
		t.Fatalf("unexpected output:\n%s", output)
	}
	if !strings.Contains(output, "capydb webhooks deliveries whe_1") {
		t.Fatalf("expected the deliveries follow-up hint in output:\n%s", output)
	}
}

func TestWebhooksRedeliverQueuesDelivery(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_hook")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/org_hook/webhook-endpoints/whe_1/deliveries/whd_1/redeliver":
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{
				"delivery": map[string]any{
					"id":              "whd_2",
					"endpoint_id":     "whe_1",
					"organization_id": "org_hook",
					"event_type":      "backup.completed",
					"state":           "pending",
					"attempts":        0,
					"max_attempts":    5,
					"created_at":      "2026-07-28T10:00:00Z",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := runCommand(t, t.TempDir(),
		"webhooks", "redeliver", "whd_1",
		"--endpoint", "whe_1",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	)
	if err != nil {
		t.Fatalf("execute webhooks redeliver: %v", err)
	}
	if !strings.Contains(output, "Queued redelivery whd_2 of delivery whd_1 for webhook endpoint whe_1") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestWebhooksRedeliverRequiresEndpoint(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)

	_, err := runCommand(t, t.TempDir(),
		"webhooks", "redeliver", "whd_1",
		"--api-url", "http://127.0.0.1:0",
		"--api-key", "capy_test_key",
	)
	if err == nil {
		t.Fatalf("expected an error without --endpoint")
	}
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.UsageError {
		t.Fatalf("expected usage exit code, got %v", err)
	}
}

// TestInitResolvesToTheInitGroup: `create` once carried the alias "init", and
// cobra's first match shadowed the `init` group, so the documented
// `capydb init drizzle` started `capydb create` - and could create a project.
func TestInitResolvesToTheInitGroup(t *testing.T) {
	root := newRootCommand(&app{cwd: t.TempDir()}, "test")
	command, _, err := root.Find([]string{"init", "drizzle"})
	if err != nil {
		t.Fatalf("Find(init drizzle) error = %v", err)
	}
	if command.CommandPath() != "capydb init drizzle" {
		t.Fatalf("init drizzle resolved to %q, want %q", command.CommandPath(), "capydb init drizzle")
	}
}
