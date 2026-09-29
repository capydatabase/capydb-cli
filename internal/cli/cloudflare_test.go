package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/exitcode"
)

func cloudflareServer(t *testing.T, body *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/integrations/cloudflare/databases" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("the partner path must not send CapyDB credentials, got %q", got)
		}
		*body = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{
			"job":          map[string]any{"id": "job_cf", "state": "pending"},
			"organization": map[string]any{"id": "org_cf", "name": "Acme"},
			"project":      map[string]any{"id": "prj_cf", "name": "edge-db"},
		})
	}))
}

func TestCloudflareCreateDatabaseSendsAuthorizationWithoutAPIKey(t *testing.T) {
	isolateUserConfig(t)
	var body map[string]any
	server := cloudflareServer(t, &body)
	defer server.Close()

	output, err := runCommand(t, t.TempDir(), "cloudflare", "create-database",
		"--api-url", server.URL,
		"--account-id", "cf_acct", "--signature", "abc123", "--timestamp", "1790000000",
		"--name", "edge-db", "--plan", "ship", "--postgres-version", "17")
	if err != nil {
		t.Fatalf("cloudflare create-database: %v\n%s", err, output)
	}
	if body["account_id"] != "cf_acct" || body["signature"] != "abc123" || body["timestamp"] != float64(1790000000) ||
		body["billing_plan"] != "ship" || body["postgres_version"] != "17" {
		t.Fatalf("unexpected request body: %#v", body)
	}
	for _, want := range []string{"Database edge-db (prj_cf) is provisioning.", "billed through Cloudflare", "Provision job: job_cf"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}

	jsonOutput, err := runCommand(t, t.TempDir(), "cloudflare", "create-database", "-o", "json",
		"--api-url", server.URL,
		"--account-id", "cf_acct", "--signature", "abc123", "--timestamp", "1790000000", "--name", "edge-db")
	if err != nil {
		t.Fatalf("cloudflare create-database -o json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonOutput), &decoded); err != nil || decoded["job"] == nil {
		t.Fatalf("-o json output is not the result document: %v\n%s", err, jsonOutput)
	}
}

func TestCloudflareCreateDatabaseMissingFlagsIsUsageError(t *testing.T) {
	isolateUserConfig(t)
	_, err := runCommand(t, t.TempDir(), "cloudflare", "create-database", "--account-id", "cf_acct", "--name", "x")
	var coded *exitcode.Error
	if !errors.As(err, &coded) || coded.Code != exitcode.UsageError || !strings.Contains(err.Error(), "--signature is required") {
		t.Fatalf("expected a usage error naming --signature, got %v", err)
	}
}
