package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeJSON(r *http.Request, dest any) error {
	return json.NewDecoder(r.Body).Decode(dest)
}

func TestResolveRestoreTargetKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		targetKind  string
		previewRef  string
		previewName string
		want        string
		wantErr     bool
	}{
		{name: "explicit project", targetKind: "project", want: "project"},
		{name: "explicit new_preview", targetKind: "NEW_PREVIEW", want: "new_preview"},
		{name: "infer preview from ref", previewRef: "prv_1", want: "preview"},
		{name: "infer new_preview from name", previewName: "qa", want: "new_preview"},
		{name: "default safe new_preview", want: "new_preview"},
		{name: "invalid kind", targetKind: "bogus", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRestoreTargetKind(tt.targetKind, tt.previewRef, tt.previewName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveRestoreTargetKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRestoreSourceDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		backupKey      string
		restoreTime    string
		restorePointID string
		want           string
	}{
		{name: "backup key", backupKey: "base_20260826", want: "backup base_20260826"},
		{name: "restore time", restoreTime: "2026-08-26T10:00:00Z", want: "the state at 2026-08-26T10:00:00Z"},
		{name: "restore point", restorePointID: "rp_1", want: "restore point rp_1"},
		{name: "fallback", want: "the requested restore source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restoreSourceDescription(tt.backupKey, tt.restoreTime, tt.restorePointID); got != tt.want {
				t.Fatalf("restoreSourceDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRestoreOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		targetKind string
		previewID  string
		want       string
	}{
		{
			name:       "project overwrite",
			targetKind: "project",
			want: "Restore complete: the live database for project test-app now matches backup base_1.\n" +
				"Connections resume automatically. Verify with `capydb sql \"select 1\"`.\n",
		},
		{
			name:       "new preview with id",
			targetKind: "new_preview",
			previewID:  "prv_9",
			want: "Restore complete: preview prv_9 now holds the restored data.\n" +
				"Get its connection string with `capydb connection-string --preview prv_9`.\n",
		},
		{
			name:       "existing preview with id",
			targetKind: "preview",
			previewID:  "prv_2",
			want: "Restore complete: preview prv_2 now holds the restored data.\n" +
				"Get its connection string with `capydb connection-string --preview prv_2`.\n",
		},
		{
			name:       "preview without id",
			targetKind: "new_preview",
			want: "Restore complete: the preview database now holds the restored data.\n" +
				"Find it with `capydb preview list`.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restoreOutcome("test-app", tt.targetKind, "backup base_1", tt.previewID); got != tt.want {
				t.Fatalf("restoreOutcome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPreviewExtendCallsEndpoint(t *testing.T) {
	t.Setenv("CI", "true")

	var gotBody map[string]int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer capy_test_key" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/preview-databases/prv_extend/extend" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := decodeJSON(r, &gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeJSON(t, w, map[string]any{
			"preview": map[string]any{
				"id":             "prv_extend",
				"name":           "qa-db",
				"project_id":     "project_1",
				"ttl_expires_at": "2026-06-01T12:00:00Z",
			},
		})
	}))
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"preview",
		"extend",
		"prv_extend",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
		"--ttl-hours", "24",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute preview extend: %v", err)
	}

	if gotBody["ttl_hours"] != 24 {
		t.Fatalf("unexpected ttl_hours in body: %#v", gotBody)
	}

	text := output.String()
	if !strings.Contains(text, "Set preview qa-db (prv_extend) TTL to 24h from now") {
		t.Fatalf("unexpected output:\n%s", text)
	}
	if !strings.Contains(text, "ttl_expires_at: 2026-06-01T12:00:00Z") {
		t.Fatalf("expected new ttl in output:\n%s", text)
	}
}

func TestPreviewExtendRejectsOutOfRangeTTL(t *testing.T) {
	t.Setenv("CI", "true")

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	output := new(bytes.Buffer)
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs([]string{
		"preview",
		"extend",
		"prv_1",
		"--api-key", "capy_test_key",
		"--ttl-hours", "0",
	})

	if err := command.Execute(); err == nil {
		t.Fatalf("expected error for out-of-range ttl-hours")
	}
}

// newImportGateServer serves the minimal API surface the import/rotate
// commands touch, records whether any mutating endpoint was hit, and captures
// the JSON body it received.
func newImportGateServer(t *testing.T, mutatingPath string, gotBody *map[string]any, mutated *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			writeJSON(t, w, map[string]any{
				"projects": []map[string]any{{
					"id":              "project_1",
					"organization_id": "org_1",
					"region":          "swedencentral",
					"name":            "test-app",
					"slug":            "test-app",
					"state":           "ready",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			writeJSON(t, w, map[string]any{
				"organization": map[string]any{"id": "org_1", "clerk_organization_slug": "acme"},
				"principal":    map[string]any{"organization_id": "org_1", "scopes": []string{"*"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/project_1/approvals":
			// The CLI must never mint its own approval: only a person in the
			// dashboard can, and the control plane refuses an API key.
			t.Errorf("the CLI minted an approval; it must present the one it was given")
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodPost && r.URL.Path == mutatingPath:
			*mutated = true
			if gotBody != nil {
				if err := decodeJSON(r, gotBody); err != nil {
					t.Fatalf("decode body: %v", err)
				}
			}
			writeJSON(t, w, map[string]any{
				"job": map[string]any{
					"id":              "job_1",
					"organization_id": "org_1",
					"project_id":      "project_1",
					"state":           "pending",
					"type":            "project.import",
					"max_attempts":    3,
					"created_at":      "2026-07-14T00:00:00Z",
					"updated_at":      "2026-07-14T00:00:00Z",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

// The typed-name/--confirm gates are the safety rails on the destructive
// import family: non-interactive runs must hard-refuse without the flag and
// must never reach the mutating endpoint.
func TestImportRefusesWithoutConfirmNonInteractive(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	server := newImportGateServer(t, "/v1/projects/project_1/imports", nil, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"import",
		"--project", "test-app",
		"--source-url", "postgres://user:pass@db.example.com:5432/app",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("expected confirm refusal, got %v", err)
	}
	if mutated {
		t.Fatalf("import endpoint was called despite the refused gate")
	}
}

func TestImportConfirmFlagSendsConfirmTrue(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	gotBody := map[string]any{}
	server := newImportGateServer(t, "/v1/projects/project_1/imports", &gotBody, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"import",
		"--project", "test-app",
		"--source-url", "postgres://user:pass@db.example.com:5432/app",
		"--confirm",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute import --confirm: %v", err)
	}
	if !mutated {
		t.Fatalf("import endpoint was never called")
	}
	if gotBody["confirm"] != true {
		t.Fatalf("expected confirm: true in body, got %#v", gotBody)
	}
}

func TestImportFollowRefusesWithoutConfirmNonInteractive(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	server := newImportGateServer(t, "/v1/projects/project_1/imports/follow", nil, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"import",
		"--follow",
		"--project", "test-app",
		"--source-url", "postgres://user:pass@db.example.com:5432/app",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("expected confirm refusal, got %v", err)
	}
	if mutated {
		t.Fatalf("follow start endpoint was called despite the refused gate")
	}
}

func TestImportFollowConfirmFlagSendsConfirmTrue(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	gotBody := map[string]any{}
	server := newImportGateServer(t, "/v1/projects/project_1/imports/follow", &gotBody, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"import",
		"--follow",
		"--project", "test-app",
		"--source-url", "postgres://user:pass@db.example.com:5432/app",
		"--confirm",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute import --follow --confirm: %v", err)
	}
	if !mutated {
		t.Fatalf("follow start endpoint was never called")
	}
	if gotBody["confirm"] != true {
		t.Fatalf("expected confirm: true in body, got %#v", gotBody)
	}
}

func TestImportCutoverRefusesWithoutConfirmNonInteractive(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	server := newImportGateServer(t, "/v1/projects/project_1/imports/follow/cutover", nil, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"import", "cutover",
		"--project", "test-app",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("expected confirm refusal, got %v", err)
	}
	if mutated {
		t.Fatalf("cutover endpoint was called despite the refused gate")
	}
}

func TestCredentialsRotateConfirmFlag(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	server := newImportGateServer(t, "/v1/projects/project_1/credentials/rotate", nil, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"credentials", "rotate",
		"--project", "test-app",
		"--confirm",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute credentials rotate --confirm: %v", err)
	}
	if !mutated {
		t.Fatalf("rotate endpoint was never called")
	}
}

func TestRestoreConfirmFlagAliasesOverwrite(t *testing.T) {
	t.Setenv("CI", "true")

	var mutated bool
	gotBody := map[string]any{}
	server := newImportGateServer(t, "/v1/projects/project_1/restores", &gotBody, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"restore",
		"--project", "test-app",
		"--target-kind", "project",
		"--backup-key", "logical/app/20260714.dump",
		"--confirm",
		"--approval-token", "ap_test_token",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute restore --confirm: %v", err)
	}
	if !mutated {
		t.Fatalf("restore endpoint was never called")
	}
	if gotBody["approval_token"] != "ap_test_token" {
		t.Fatalf("expected the given approval_token in body, got %#v", gotBody)
	}
}

// Without an approval a person created, an overwrite restore stops before the destructive call and
// says where to create one.
func TestRestoreOverwriteWithoutApprovalPointsAtTheDashboard(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("CAPYDB_APPROVAL_TOKEN", "")

	var mutated bool
	server := newImportGateServer(t, "/v1/projects/project_1/restores", nil, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"restore",
		"--project", "test-app",
		"--target-kind", "project",
		"--backup-key", "logical/app/20260714.dump",
		"--confirm",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--approval-token") || !strings.Contains(err.Error(), "backups") {
		t.Fatalf("expected an error pointing at the backups page and --approval-token, got %v", err)
	}
	if mutated {
		t.Fatalf("the restore endpoint was called without an approval")
	}
}

// CAPYDB_APPROVAL_TOKEN carries the approval for scripts and CI.
func TestRestoreOverwriteReadsTheApprovalFromTheEnvironment(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("CAPYDB_APPROVAL_TOKEN", "ap_env_token")

	var mutated bool
	gotBody := map[string]any{}
	server := newImportGateServer(t, "/v1/projects/project_1/restores", &gotBody, &mutated)
	defer server.Close()

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"restore",
		"--project", "test-app",
		"--target-kind", "project",
		"--backup-key", "logical/app/20260714.dump",
		"--confirm",
		"--api-url", server.URL,
		"--api-key", "capy_test_key",
	})

	if err := command.Execute(); err != nil {
		t.Fatalf("execute restore: %v", err)
	}
	if gotBody["approval_token"] != "ap_env_token" {
		t.Fatalf("expected the approval from CAPYDB_APPROVAL_TOKEN, got %#v", gotBody)
	}
}

func TestPreviewCreateRejectsOutOfRangeTTL(t *testing.T) {
	t.Setenv("CI", "true")

	application := &app{cwd: t.TempDir()}
	command := newRootCommand(application, "test")
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	command.SetArgs([]string{
		"preview", "create",
		"--project", "test-app",
		"--ttl-hours", "500",
		"--api-url", "http://127.0.0.1:0",
		"--api-key", "capy_test_key",
	})

	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "--ttl-hours must be between 1 and 168") {
		t.Fatalf("expected ttl range error, got %v", err)
	}
}
