package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestProjectsRetryQueuesProvisioning(t *testing.T) {
	calls := 0
	server := newFakeControlPlane(t, map[string]any{"state": "failed"}, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/retry-provisioning": func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_p", "state": "queued", "type": "instance.create"}})
		},
	})
	output, err := runCommand(t, t.TempDir(), "projects", "retry", "demo", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("retry: %v\n%s", err, output)
	}
	if calls != 1 || !strings.Contains(output, "Queued provisioning job job_p for project demo") || !strings.Contains(output, "Provisioning database") {
		t.Fatalf("calls=%d output:\n%s", calls, output)
	}
}

func TestProjectsRetryExplainsAConflict(t *testing.T) {
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"POST /v1/projects/prj_1/retry-provisioning": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			writeJSON(t, w, map[string]any{"error": "project is not failed"})
		},
	})
	_, err := runCommand(t, t.TempDir(), "projects", "retry", "prj_1", "--api-url", server.URL, "--api-key", "capy_test")
	if err == nil || !strings.Contains(err.Error(), "only a project whose provisioning failed can be (state ready)") {
		t.Fatalf("conflict error = %v", err)
	}
}
