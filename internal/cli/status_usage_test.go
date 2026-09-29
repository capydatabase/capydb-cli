package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func usageRoutes(t *testing.T) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /status": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"status": "operational", "components": []any{}})
		},
		"GET /v1/me": func(w http.ResponseWriter, r *http.Request) { writeViewer(t, w, "org_1") },
		"GET /v1/projects/prj_1/preview-databases": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"preview_databases": []any{map[string]any{"id": "pdb_1"}}})
		},
		"GET /v1/projects/prj_1/backups": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"backups": []any{
				map[string]any{"id": "b1", "size_bytes": 1024},
				map[string]any{"id": "b2", "size_bytes": 2048},
			}})
		},
	}
}

func TestStatusUsageReportsMetersAndWarnings(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	routes := usageRoutes(t)
	routes["GET /v1/projects/prj_1/observability"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"observability": map[string]any{
			"database_size_bytes": 900, "storage_limit_bytes": 1000, "storage_usage_percent": 90,
			"connection_count": 2, "connection_limit": 20, "connection_usage_percent": 10,
			"alerts": []string{"database_size_over_90_percent"},
		}})
	}
	server := newFakeControlPlane(t, nil, routes)

	output, err := runCommand(t, t.TempDir(), "status", "--usage", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("status --usage: %v\n%s", err, output)
	}
	for _, want := range []string{"plan:", "storage:     900 B of 1000 B (90.0%)", "connections: 2 of 20 (10.0%)", "previews:    1 active", "backups:     2 retained, 3.0 KB", "storage is at 90%"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in\n%s", want, output)
		}
	}
}

func TestStatusUsageDoesNotWakeAPausedDatabase(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	// No observability route: a request to it fails the test.
	server := newFakeControlPlane(t, map[string]any{"runtime_status": "paused", "storage_limit_bytes": 1 << 30}, usageRoutes(t))

	output, err := runCommand(t, t.TempDir(), "status", "--usage", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test", "--output", "json")
	if err != nil {
		t.Fatalf("status --usage: %v\n%s", err, output)
	}
	var report struct {
		Usage statusUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("decode: %v\n%s", err, output)
	}
	if !report.Usage.LiveSkipped || report.Usage.Storage == nil || report.Usage.Storage.Limit != 1<<30 {
		t.Fatalf("unexpected usage: %+v", report.Usage)
	}
}

func TestStatusUsageNeedsAProject(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	if _, err := runCommand(t, t.TempDir(), "status", "--usage", "--api-key", "capy_test", "--api-url", "http://127.0.0.1:1"); err == nil {
		t.Fatal("expected an error without a linked project")
	}
}

func TestStatusUsageKeepsTheLatestJob(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	routes := usageRoutes(t)
	routes["GET /v1/jobs/job_9"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_9", "type": "project.backup", "state": "completed"}})
	}
	server := newFakeControlPlane(t, map[string]any{"runtime_status": "paused", "latest_job_id": "job_9"}, routes)
	output, err := runCommand(t, t.TempDir(), "status", "--usage", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("status --usage: %v\n%s", err, output)
	}
	if !strings.Contains(output, "latest_job_id: job_9") {
		t.Fatalf("latest job dropped:\n%s", output)
	}
}
