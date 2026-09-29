package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envSyncRoutes(t *testing.T, provider string, body *map[string]any) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/integrations/" + provider + "/connect": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{
				"integration": map[string]any{"id": "int_1", "provider": provider, "config": map[string]any{"hook_error": "deploy hook registration failed; preview branches disabled"}},
				"job":         map[string]any{"id": "job_1", "state": "pending"},
			})
		},
		"GET /v1/jobs/job_1": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_1", "state": "completed", "type": "integration.sync_env"}})
		},
	}
}

func TestEnvSyncVercelReadsTheVercelLink(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	t.Setenv("VERCEL_TOKEN", "vercel_tok")
	var body map[string]any
	server := newFakeControlPlane(t, nil, envSyncRoutes(t, "vercel", &body))
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".vercel"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, ".vercel", "project.json"), `{"projectId": "prj_vercel", "orgId": "team_abc"}`)

	output, err := runCommand(t, dir, "env", "sync", "vercel", "--project", "prj_1", "--preview-branches", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("env sync vercel: %v\n%s", err, output)
	}
	if body["vercel_project_id"] != "prj_vercel" || body["team_id"] != "team_abc" || body["token"] != "vercel_tok" || body["preview_branches"] != true {
		t.Fatalf("connect body = %v", body)
	}
	if !strings.Contains(output, "Env vars pushed to Vercel project prj_vercel") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestEnvSyncNetlifyReportsHookFailure(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var body map[string]any
	server := newFakeControlPlane(t, nil, envSyncRoutes(t, "netlify", &body))
	output, err := runCommand(t, t.TempDir(), "env", "sync", "netlify", "--site", "site_1", "--token", "ntl", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("env sync netlify: %v\n%s", err, output)
	}
	if body["site_id"] != "site_1" || body["token"] != "ntl" {
		t.Fatalf("connect body = %v", body)
	}
	if !strings.Contains(output, "note: deploy hook registration failed") {
		t.Fatalf("hook failure not reported:\n%s", output)
	}
}

func TestEnvSyncRequiresTokenAndTarget(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	t.Setenv("VERCEL_TOKEN", "")
	t.Setenv("NETLIFY_AUTH_TOKEN", "")
	for _, args := range [][]string{
		{"env", "sync", "vercel", "--vercel-project", "p"},
		{"env", "sync", "vercel", "--token", "t"},
		{"env", "sync", "netlify", "--site", "s"},
		{"env", "sync", "netlify", "--token", "t"},
	} {
		if _, err := runCommand(t, t.TempDir(), append(args, "--api-key", "k", "--api-url", "http://127.0.0.1:1")...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
