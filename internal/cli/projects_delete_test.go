package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deleteProjectServer answers project resolution for one project and records
// whether (and with which approval token) the delete endpoint was called.
func deleteProjectServer(t *testing.T, environment string, deleteCalls *int, gotToken *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{"projects": []map[string]any{{
				"id": "prj_1", "name": "shop", "slug": "shop", "environment": environment,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeViewer(t, w, "org_1")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/projects/prj_1":
			*deleteCalls++
			*gotToken = r.URL.Query().Get("approval_token")
			w.WriteHeader(http.StatusAccepted)
			writeJSON(t, w, map[string]any{"job": map[string]any{"id": "job_del", "state": "pending", "type": "project.delete"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

func TestProjectsDeleteProductionWithoutApprovalDoesNotCallDelete(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var calls int
	var token string
	server := deleteProjectServer(t, "production", &calls, &token)
	defer server.Close()

	_, err := runCommand(t, t.TempDir(), "projects", "delete", "shop", "--confirm",
		"--api-url", server.URL, "--api-key", "capy_test_key", "--app-url", "https://capydb.dev")
	if err == nil {
		t.Fatal("expected an error for a production delete without an approval")
	}
	if calls != 0 {
		t.Fatalf("delete endpoint called %d times without an approval", calls)
	}
	for _, want := range []string{"needs an approval", "https://capydb.dev/dashboard/", "/settings", "--approval-token", "Nothing was deleted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestProjectsDeleteProductionPresentsApprovalToken(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	t.Setenv("CAPYDB_APPROVAL_TOKEN", "apr_from_env")
	var calls int
	var token string
	server := deleteProjectServer(t, "production", &calls, &token)
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "projects", "delete", "shop", "--yes",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("projects delete: %v\n%s", err, output)
	}
	if calls != 1 || token != "apr_from_env" {
		t.Fatalf("delete calls = %d, token = %q; want 1 call with the env token", calls, token)
	}
	if !strings.Contains(output, "Queued deletion job job_del for project shop (prj_1)") {
		t.Fatalf("unexpected output:\n%s", output)
	}
}

func TestProjectsDeleteNonProductionNeedsOnlyConfirmation(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	var calls int
	var token string
	server := deleteProjectServer(t, "non_production", &calls, &token)
	defer server.Close()

	if _, err := runCommand(t, t.TempDir(), "projects", "delete", "shop",
		"--api-url", server.URL, "--api-key", "capy_test_key"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("expected a not-confirmed error without --confirm in CI, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("delete endpoint called without confirmation")
	}

	output, err := runCommand(t, t.TempDir(), "projects", "delete", "shop", "--confirm",
		"--api-url", server.URL, "--api-key", "capy_test_key")
	if err != nil {
		t.Fatalf("projects delete: %v\n%s", err, output)
	}
	if calls != 1 || token != "" {
		t.Fatalf("delete calls = %d, token = %q; want 1 call without a token", calls, token)
	}
}

func TestProjectsDeleteRequiresExplicitProject(t *testing.T) {
	t.Setenv("CI", "true")
	isolateUserConfig(t)
	if _, err := runCommand(t, t.TempDir(), "projects", "delete", "--confirm"); err == nil {
		t.Fatal("expected a usage error when no project is named")
	}
}
