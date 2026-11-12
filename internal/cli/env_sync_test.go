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
	// Not connected: the stored-token sync answers 404, so a token is required.
	notConnected := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": "not found"})
	}
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/integrations/vercel/sync":  notConnected,
		"POST /v1/projects/prj_1/integrations/netlify/sync": notConnected,
	})
	for _, tt := range []struct {
		args []string
		want string
	}{
		{args: []string{"env", "sync", "vercel", "--vercel-project", "p"}, want: "a Vercel token is required"},
		{args: []string{"env", "sync", "vercel", "--token", "t"}, want: "no Vercel project"},
		{args: []string{"env", "sync", "netlify", "--site", "s"}, want: "a Netlify token is required"},
		{args: []string{"env", "sync", "netlify", "--token", "t"}, want: "no Netlify site"},
	} {
		_, err := runCommand(t, t.TempDir(), append(tt.args, "--project", "prj_1", "--api-key", "k", "--api-url", server.URL)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err = %v, want %q", tt.args, err, tt.want)
		}
	}
}

// TestEnvSyncWithoutATokenResyncsAConnectedProject: re-running env sync to
// re-push used to reconnect, registering a new deploy hook each time (FG-6).
// Without a token, on a connected project, it now queues the stored-token sync
// instead; on an unconnected one (404) the token is still required, and so it
// is when --preview-branches asks to change the connection (O-07).
func TestEnvSyncWithoutATokenResyncsAConnectedProject(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	t.Setenv("VERCEL_TOKEN", "")
	t.Setenv("NETLIFY_AUTH_TOKEN", "")
	connected := true
	syncs := 0
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/integrations/netlify/sync": func(w http.ResponseWriter, r *http.Request) {
			if !connected {
				w.WriteHeader(http.StatusNotFound)
				writeJSON(t, w, map[string]any{"error": "not found"})
				return
			}
			syncs++
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_s", "state": "queued", "type": "integration.sync_env"}})
		},
		"GET /v1/jobs/job_s": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_s", "state": "completed", "type": "integration.sync_env"}})
		},
	})
	args := []string{"env", "sync", "netlify", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test"}

	output, err := runCommand(t, t.TempDir(), args...)
	if err != nil || syncs != 1 || !strings.Contains(output, "already connected to netlify; pushing env vars again with the stored token") {
		t.Fatalf("resync: err=%v syncs=%d\n%s", err, syncs, output)
	}

	if _, err := runCommand(t, t.TempDir(), append(args, "--preview-branches")...); err == nil || !strings.Contains(err.Error(), "a Netlify token is required") || syncs != 1 {
		t.Fatalf("--preview-branches without a token: err=%v syncs=%d", err, syncs)
	}

	connected = false
	if _, err := runCommand(t, t.TempDir(), args...); err == nil || !strings.Contains(err.Error(), "a Netlify token is required") {
		t.Fatalf("unconnected project: err = %v, want the token requirement", err)
	}
}
