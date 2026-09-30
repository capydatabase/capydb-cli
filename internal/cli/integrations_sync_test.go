package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestIntegrationsSyncQueuesAPush(t *testing.T) {
	status := http.StatusAccepted
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/integrations/vercel/sync": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if status != http.StatusAccepted {
				writeJSON(t, w, map[string]any{"error": "refused"})
				return
			}
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_s", "state": "queued", "type": "integration.sync_env"}})
		},
	})
	args := []string{"integrations", "sync", "Vercel", "--project", "prj_1", "--api-url", server.URL, "--api-key", "capy_test"}

	output, err := runCommand(t, t.TempDir(), args...)
	if err != nil || !strings.Contains(output, "Queued vercel env push for project demo") || !strings.Contains(output, "Syncing environment variables") {
		t.Fatalf("sync: %v\n%s", err, output)
	}

	status = http.StatusNotFound
	if _, err := runCommand(t, t.TempDir(), args...); err == nil || !strings.Contains(err.Error(), "has no vercel integration") {
		t.Fatalf("404 error = %v", err)
	}
	status = http.StatusConflict
	if _, err := runCommand(t, t.TempDir(), args...); err == nil || !strings.Contains(err.Error(), "already queued or running") {
		t.Fatalf("409 error = %v", err)
	}
}

func TestIntegrationsSyncRejectsUnknownProviders(t *testing.T) {
	_, err := runCommand(t, t.TempDir(), "integrations", "sync", "heroku", "--api-url", "http://127.0.0.1:1", "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "vercel, netlify, cloudflare") {
		t.Fatalf("err = %v", err)
	}
}
